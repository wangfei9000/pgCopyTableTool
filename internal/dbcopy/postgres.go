package dbcopy

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// 这些阶段文本会直接显示在表格状态列。
	PhaseReadingStructure = "读取结构"
	PhaseCreatingTable    = "建立目标表"
	PhaseTruncating       = "清空目标表"
	PhaseCopying          = "复制中"
	PhaseCompleted        = "完成"
)

// executor 让 EnsureTable 可以同时接受普通连接和事务。
type executor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

// Connect 创建 PostgreSQL 连接并执行 Ping，确保配置真实可用。
func Connect(ctx context.Context, cfg ConnConfig) (*pgx.Conn, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	conn, err := pgx.Connect(ctx, cfg.ConnString())
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	return conn, nil
}

// TestConnection 用于界面中的“测试连接”按钮。
func TestConnection(ctx context.Context, cfg ConnConfig) error {
	conn, err := Connect(ctx, cfg)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	return nil
}

// ListTables 读取源库中可复制的普通表和分区父表。
func ListTables(ctx context.Context, conn *pgx.Conn) ([]TableInfo, error) {
	rows, err := conn.Query(ctx, `
		SELECT
			n.nspname,
			c.relname,
			GREATEST(COALESCE(s.n_live_tup, c.reltuples::bigint, 0), 0)::bigint AS estimated_rows
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
		WHERE c.relkind IN ('r', 'p')
			AND NOT c.relispartition
			-- 排除系统 schema 和分区子表，避免重复复制同一份逻辑数据。
			AND n.nspname NOT IN ('pg_catalog', 'information_schema')
			AND n.nspname NOT LIKE 'pg_toast%'
		ORDER BY n.nspname, c.relname
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tables []TableInfo
	for rows.Next() {
		var table TableInfo
		if err := rows.Scan(&table.Ref.Schema, &table.Ref.Name, &table.EstimatedRows); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

// LoadColumns 读取源表列结构，并把目标库可能不存在的自定义类型降级为 text。
func LoadColumns(ctx context.Context, conn *pgx.Conn, ref TableRef) ([]ColumnInfo, error) {
	rows, err := conn.Query(ctx, `
		SELECT
			a.attname,
			-- 目标库只负责承接数据，不强制要求安装源库所有扩展或自定义类型。
			CASE
				WHEN t.typtype = 'd' AND base_ns.nspname = 'pg_catalog'
					THEN pg_catalog.format_type(t.typbasetype, t.typtypmod)
				WHEN t.typtype = 'd'
					THEN 'text'
				WHEN type_ns.nspname <> 'pg_catalog'
					THEN 'text'
				WHEN elem.oid IS NOT NULL AND elem_ns.nspname <> 'pg_catalog'
					THEN 'text'
				ELSE pg_catalog.format_type(a.atttypid, a.atttypmod)
			END AS target_type,
			(
				t.typtype = 'd'
				OR type_ns.nspname <> 'pg_catalog'
				OR (elem.oid IS NOT NULL AND elem_ns.nspname <> 'pg_catalog')
			) AS cast_for_select
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_catalog.pg_type t ON t.oid = a.atttypid
		JOIN pg_catalog.pg_namespace type_ns ON type_ns.oid = t.typnamespace
		LEFT JOIN pg_catalog.pg_type elem ON elem.oid = t.typelem
		LEFT JOIN pg_catalog.pg_namespace elem_ns ON elem_ns.oid = elem.typnamespace
		LEFT JOIN pg_catalog.pg_type base_t ON base_t.oid = t.typbasetype
		LEFT JOIN pg_catalog.pg_namespace base_ns ON base_ns.oid = base_t.typnamespace
		WHERE n.nspname = $1
			AND c.relname = $2
			AND a.attnum > 0
			AND NOT a.attisdropped
		ORDER BY a.attnum
	`, ref.Schema, ref.Name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var columns []ColumnInfo
	for rows.Next() {
		var col ColumnInfo
		if err := rows.Scan(&col.Name, &col.DataType, &col.CastForSelect); err != nil {
			return nil, err
		}
		columns = append(columns, col)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("%s has no copyable columns", ref.DisplayName())
	}
	return columns, nil
}

// CopyTable 复制单张表：读取结构、目标建表、可选清空、再用 COPY 写入。
// 目标端操作放在同一个事务里；如果复制失败，该表本次写入会回滚。
func CopyTable(
	ctx context.Context,
	source *pgx.Conn,
	target *pgx.Conn,
	table TableInfo,
	opts CopyOptions,
	onProgress func(CopyProgress),
) (int64, error) {
	progressEvery := opts.ProgressEvery
	if progressEvery <= 0 {
		progressEvery = 10000
	}

	report := func(phase string, copied int64) {
		if onProgress != nil {
			onProgress(CopyProgress{Table: table.Ref, Phase: phase, Copied: copied})
		}
	}

	report(PhaseReadingStructure, 0)
	columns, err := LoadColumns(ctx, source, table.Ref)
	if err != nil {
		return 0, err
	}

	tx, err := target.Begin(ctx)
	if err != nil {
		return 0, err
	}
	// Commit 成功前 defer Rollback 都是安全的；成功后 Rollback 会被 pgx 忽略。
	defer tx.Rollback(context.Background())

	report(PhaseCreatingTable, 0)
	if err := EnsureTable(ctx, tx, table.Ref, columns); err != nil {
		return 0, err
	}

	if opts.Truncate {
		report(PhaseTruncating, 0)
		if _, err := tx.Exec(ctx, "TRUNCATE TABLE "+quoteQualified(table.Ref)); err != nil {
			return 0, err
		}
	}

	// 源端保持普通 SELECT，目标端走 pgx CopyFrom，兼顾远程复制和吞吐性能。
	selectSQL := buildSelectSQL(table.Ref, columns)
	rows, err := source.Query(ctx, selectSQL)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	columnNames := make([]string, len(columns))
	for i, col := range columns {
		columnNames[i] = col.Name
	}

	src := &progressRowsSource{
		rows:          rows,
		table:         table.Ref,
		progressEvery: progressEvery,
		onProgress:    onProgress,
		lastReport:    time.Now(),
	}

	report(PhaseCopying, 0)
	copied, err := tx.CopyFrom(ctx, pgx.Identifier{table.Ref.Schema, table.Ref.Name}, columnNames, src)
	if err != nil {
		return src.copied, err
	}
	if err := src.Err(); err != nil {
		return src.copied, err
	}

	if err := tx.Commit(ctx); err != nil {
		return copied, err
	}
	report(PhaseCompleted, copied)
	return copied, nil
}

// EnsureTable 只创建 schema、表和缺失列，不创建任何外键、索引、约束或触发器。
func EnsureTable(ctx context.Context, exec executor, ref TableRef, columns []ColumnInfo) error {
	if len(columns) == 0 {
		return errors.New("no columns to create")
	}

	if _, err := exec.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoteIdent(ref.Schema)); err != nil {
		return err
	}
	if _, err := exec.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+quoteQualified(ref)+" ()"); err != nil {
		return err
	}

	for _, col := range columns {
		stmt := fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s",
			quoteQualified(ref),
			quoteIdent(col.Name),
			col.DataType,
		)
		if _, err := exec.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("add column %s: %w", col.Name, err)
		}
	}
	return nil
}

// buildSelectSQL 为 COPY 数据源生成稳定的列顺序。
func buildSelectSQL(ref TableRef, columns []ColumnInfo) string {
	items := make([]string, len(columns))
	for i, col := range columns {
		ident := quoteIdent(col.Name)
		if col.CastForSelect {
			items[i] = ident + "::" + col.DataType
		} else {
			items[i] = ident
		}
	}
	return "SELECT " + strings.Join(items, ", ") + " FROM " + quoteQualified(ref)
}

func quoteQualified(ref TableRef) string {
	return quoteIdent(ref.Schema) + "." + quoteIdent(ref.Name)
}

func quoteIdent(v string) string {
	return `"` + strings.ReplaceAll(v, `"`, `""`) + `"`
}

// progressRowsSource 把 pgx.Rows 包装成 CopyFromSource，同时统计已复制行数。
type progressRowsSource struct {
	rows          pgx.Rows
	table         TableRef
	values        []any
	err           error
	copied        int64
	progressEvery int64
	lastReport    time.Time
	onProgress    func(CopyProgress)
}

func (s *progressRowsSource) Next() bool {
	if !s.rows.Next() {
		return false
	}

	values, err := s.rows.Values()
	if err != nil {
		s.err = err
		return false
	}
	s.values = values
	s.copied++

	if s.onProgress != nil && (s.copied%s.progressEvery == 0 || time.Since(s.lastReport) > 750*time.Millisecond) {
		s.lastReport = time.Now()
		s.onProgress(CopyProgress{Table: s.table, Phase: PhaseCopying, Copied: s.copied})
	}
	return true
}

func (s *progressRowsSource) Values() ([]any, error) {
	return s.values, nil
}

func (s *progressRowsSource) Err() error {
	if s.err != nil {
		return s.err
	}
	return s.rows.Err()
}
