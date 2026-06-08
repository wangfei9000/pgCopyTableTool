package dbcopy

import (
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ConnConfig 保存一个 PostgreSQL 远程连接所需的最小配置。
type ConnConfig struct {
	Host     string
	Port     int
	Database string
	User     string
	Password string
	SSLMode  string
}

// Validate 做界面层之外的基础校验，避免把空连接信息传给 pgx。
func (c ConnConfig) Validate() error {
	if strings.TrimSpace(c.Host) == "" {
		return errors.New("host is required")
	}
	if c.Port <= 0 || c.Port > 65535 {
		return errors.New("port must be between 1 and 65535")
	}
	if strings.TrimSpace(c.Database) == "" {
		return errors.New("database is required")
	}
	if strings.TrimSpace(c.User) == "" {
		return errors.New("user is required")
	}
	return nil
}

// ConnString 生成 pgx 可直接使用的 PostgreSQL URL。
func (c ConnConfig) ConnString() string {
	sslMode := strings.TrimSpace(c.SSLMode)
	if sslMode == "" {
		sslMode = "prefer"
	}

	u := &url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(c.User, c.Password),
		Host:   net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
		Path:   "/" + c.Database,
	}
	q := u.Query()
	q.Set("sslmode", sslMode)
	u.RawQuery = q.Encode()
	return u.String()
}

// TableRef 标识一张表，schema 和 name 分开保存，方便安全引用。
type TableRef struct {
	Schema string
	Name   string
}

// DisplayName 返回界面里展示用的 schema.table 名称。
func (r TableRef) DisplayName() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

// TableInfo 是表列表中展示和复制调度需要的信息。
type TableInfo struct {
	Ref           TableRef
	EstimatedRows int64
}

// ColumnInfo 描述从源表读取到的列，以及目标表中要创建的类型。
type ColumnInfo struct {
	Name     string
	DataType string
	// CastForSelect 表示源端读取时需要转成目标类型，主要用于自定义类型兼容。
	CastForSelect bool
}

// CopyOptions 控制每张表的复制行为。
type CopyOptions struct {
	Truncate bool
	// ProgressEvery 控制 COPY 过程中每隔多少行上报一次进度。
	ProgressEvery int64
}

// CopyProgress 是复制核心向 UI 层上报的状态快照。
type CopyProgress struct {
	Table  TableRef
	Phase  string
	Copied int64
}
