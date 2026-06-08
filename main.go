package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/jackc/pgx/v5"
	"github.com/local/pg-table-copy-fyne/internal/dbcopy"
)

// connForm 聚合一组连接输入控件，方便从 UI 提取数据库配置。
type connForm struct {
	host     *widget.Entry
	port     *widget.Entry
	database *widget.Entry
	user     *widget.Entry
	password *widget.Entry
	sslMode  *widget.Select
}

// tableRow 是表格中每一行的显示状态。
type tableRow struct {
	info   dbcopy.TableInfo
	status string
	copied int64
	err    string
}

// uiState 保存跨按钮、后台任务和表格刷新共享的运行状态。
type uiState struct {
	mu         sync.RWMutex
	rows       []tableRow
	sourceConn *pgx.Conn
	targetConn *pgx.Conn
	cancelCopy context.CancelFunc
	copying    bool
}

func main() {
	fyneApp := app.NewWithID("com.local.pg-table-copy")
	window := fyneApp.NewWindow("PostgreSQL 远程表复制")
	window.Resize(fyne.NewSize(1180, 760))

	state := &uiState{}
	sourceForm := newConnForm()
	targetForm := newConnForm()

	sourcePanel := buildConnPanel("A 数据库（源，只读）", sourceForm)
	targetPanel := buildConnPanel("B 数据库（目标，写入）", targetForm)

	statusLabel := widget.NewLabel("未连接")
	copyLabel := widget.NewLabel("等待操作")
	progress := widget.NewProgressBar()
	progress.Min = 0
	progress.Max = 1

	truncateCheck := widget.NewCheck("复制前清空目标表", nil)
	truncateCheck.SetChecked(true)

	table := buildTable(state)

	var testAButton *widget.Button
	var testBButton *widget.Button
	var connectButton *widget.Button
	var refreshButton *widget.Button
	var copyButton *widget.Button
	var cancelButton *widget.Button

	// 后台任务运行时禁用会改变连接或表清单的按钮，避免连接被并发替换。
	setBusy := func(busy bool) {
		if busy {
			testAButton.Disable()
			testBButton.Disable()
			connectButton.Disable()
			refreshButton.Disable()
			copyButton.Disable()
			return
		}
		testAButton.Enable()
		testBButton.Enable()
		connectButton.Enable()
		refreshButton.Enable()
		copyButton.Enable()
	}

	testAButton = widget.NewButtonWithIcon("测试 A", theme.ConfirmIcon(), func() {
		cfg, err := sourceForm.config()
		if err != nil {
			dialog.ShowError(err, window)
			return
		}
		testConnection(window, cfg, "A 数据库", testAButton)
	})

	testBButton = widget.NewButtonWithIcon("测试 B", theme.ConfirmIcon(), func() {
		cfg, err := targetForm.config()
		if err != nil {
			dialog.ShowError(err, window)
			return
		}
		testConnection(window, cfg, "B 数据库", testBButton)
	})

	connectButton = widget.NewButtonWithIcon("连接并读取表", theme.ViewRefreshIcon(), func() {
		sourceCfg, err := sourceForm.config()
		if err != nil {
			dialog.ShowError(err, window)
			return
		}
		targetCfg, err := targetForm.config()
		if err != nil {
			dialog.ShowError(err, window)
			return
		}

		setBusy(true)
		statusLabel.SetText("连接中...")
		copyLabel.SetText("正在读取 A 数据库表清单")
		go connectAndLoad(window, state, sourceCfg, targetCfg, table, statusLabel, copyLabel, setBusy)
	})

	refreshButton = widget.NewButtonWithIcon("刷新表列表", theme.ViewRefreshIcon(), func() {
		state.mu.RLock()
		sourceConn := state.sourceConn
		copying := state.copying
		state.mu.RUnlock()
		if sourceConn == nil {
			dialog.ShowInformation("提示", "请先连接两个数据库。", window)
			return
		}
		if copying {
			return
		}

		setBusy(true)
		copyLabel.SetText("正在刷新表清单")
		go refreshTables(window, state, sourceConn, table, copyLabel, setBusy)
	})

	copyButton = widget.NewButtonWithIcon("开始复制全部表", theme.MediaPlayIcon(), func() {
		state.mu.RLock()
		sourceConn := state.sourceConn
		targetConn := state.targetConn
		rowCount := len(state.rows)
		copying := state.copying
		state.mu.RUnlock()

		if sourceConn == nil || targetConn == nil {
			dialog.ShowInformation("提示", "请先连接两个数据库并读取表清单。", window)
			return
		}
		if rowCount == 0 {
			dialog.ShowInformation("提示", "A 数据库没有可复制的普通表。", window)
			return
		}
		if copying {
			return
		}

		ctx, cancel := context.WithCancel(context.Background())
		state.mu.Lock()
		state.cancelCopy = cancel
		state.copying = true
		state.mu.Unlock()

		progress.SetValue(0)
		copyLabel.SetText("开始复制")
		setBusy(true)
		cancelButton.Enable()

		opts := dbcopy.CopyOptions{Truncate: truncateCheck.Checked, ProgressEvery: 10000}
		go copyAllTables(ctx, state, sourceConn, targetConn, opts, table, progress, copyLabel, cancelButton, setBusy)
	})

	cancelButton = widget.NewButtonWithIcon("取消复制", theme.CancelIcon(), func() {
		state.mu.RLock()
		cancel := state.cancelCopy
		state.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
	})
	cancelButton.Disable()

	actionRow := container.NewHBox(
		testAButton,
		testBButton,
		connectButton,
		layout.NewSpacer(),
		truncateCheck,
		refreshButton,
		copyButton,
		cancelButton,
	)

	header := container.NewVBox(
		container.NewGridWithColumns(2, sourcePanel, targetPanel),
		actionRow,
		container.NewGridWithColumns(2, statusLabel, copyLabel),
		progress,
	)

	content := container.NewBorder(header, nil, nil, nil, table)
	window.SetContent(content)
	window.SetCloseIntercept(func() {
		// 关闭窗口时先取消复制，再异步关闭数据库连接，让 UI 能快速退出。
		state.mu.RLock()
		cancel := state.cancelCopy
		sourceConn := state.sourceConn
		targetConn := state.targetConn
		state.mu.RUnlock()
		if cancel != nil {
			cancel()
		}
		go closeConn(sourceConn)
		go closeConn(targetConn)
		window.SetCloseIntercept(nil)
		window.Close()
	})
	window.ShowAndRun()
}

// newConnForm 创建一组连接输入控件，并设置适合 PostgreSQL 的默认值。
func newConnForm() *connForm {
	host := widget.NewEntry()
	host.SetPlaceHolder("127.0.0.1")

	port := widget.NewEntry()
	port.SetText("5432")

	database := widget.NewEntry()
	database.SetPlaceHolder("postgres")

	user := widget.NewEntry()
	user.SetPlaceHolder("postgres")

	password := widget.NewPasswordEntry()

	sslMode := widget.NewSelect([]string{"prefer", "disable", "require", "verify-ca", "verify-full"}, nil)
	sslMode.SetSelected("prefer")

	return &connForm{
		host:     host,
		port:     port,
		database: database,
		user:     user,
		password: password,
		sslMode:  sslMode,
	}
}

// buildConnPanel 渲染 A/B 数据库连接信息区域。
func buildConnPanel(title string, form *connForm) fyne.CanvasObject {
	titleLabel := widget.NewLabelWithStyle(title, fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	formWidget := widget.NewForm(
		widget.NewFormItem("主机", form.host),
		widget.NewFormItem("端口", form.port),
		widget.NewFormItem("数据库", form.database),
		widget.NewFormItem("用户", form.user),
		widget.NewFormItem("密码", form.password),
		widget.NewFormItem("SSL", form.sslMode),
	)
	return container.NewVBox(titleLabel, formWidget)
}

// config 从输入控件转换为数据库连接配置。
func (f *connForm) config() (dbcopy.ConnConfig, error) {
	port, err := strconv.Atoi(strings.TrimSpace(f.port.Text))
	if err != nil {
		return dbcopy.ConnConfig{}, fmt.Errorf("端口必须是数字: %w", err)
	}
	cfg := dbcopy.ConnConfig{
		Host:     strings.TrimSpace(f.host.Text),
		Port:     port,
		Database: strings.TrimSpace(f.database.Text),
		User:     strings.TrimSpace(f.user.Text),
		Password: f.password.Text,
		SSLMode:  strings.TrimSpace(f.sslMode.Selected),
	}
	return cfg, cfg.Validate()
}

// buildTable 创建表清单，数据来自 uiState，刷新时重新读取状态快照。
func buildTable(state *uiState) *widget.Table {
	headers := []string{"表名", "预估行数", "状态", "已复制", "错误"}
	table := widget.NewTable(
		func() (int, int) {
			state.mu.RLock()
			defer state.mu.RUnlock()
			return len(state.rows) + 1, len(headers)
		},
		func() fyne.CanvasObject {
			label := widget.NewLabel("")
			label.Truncation = fyne.TextTruncateEllipsis
			return label
		},
		func(id widget.TableCellID, obj fyne.CanvasObject) {
			label := obj.(*widget.Label)
			if id.Row == 0 {
				label.TextStyle = fyne.TextStyle{Bold: true}
				label.SetText(headers[id.Col])
				return
			}

			state.mu.RLock()
			defer state.mu.RUnlock()
			if id.Row-1 >= len(state.rows) {
				label.SetText("")
				return
			}
			row := state.rows[id.Row-1]
			label.TextStyle = fyne.TextStyle{}

			switch id.Col {
			case 0:
				label.SetText(row.info.Ref.DisplayName())
			case 1:
				label.SetText(formatInt(row.info.EstimatedRows))
			case 2:
				label.SetText(row.status)
			case 3:
				label.SetText(formatInt(row.copied))
			case 4:
				label.SetText(row.err)
			default:
				label.SetText("")
			}
		},
	)
	table.SetColumnWidth(0, 280)
	table.SetColumnWidth(1, 110)
	table.SetColumnWidth(2, 120)
	table.SetColumnWidth(3, 110)
	table.SetColumnWidth(4, 440)
	return table
}

// testConnection 在后台测试连接，结果通过 fyne.Do 切回 UI 线程展示。
func testConnection(window fyne.Window, cfg dbcopy.ConnConfig, title string, button *widget.Button) {
	button.Disable()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		defer cancel()
		err := dbcopy.TestConnection(ctx, cfg)
		fyne.Do(func() {
			button.Enable()
			if err != nil {
				dialog.ShowError(fmt.Errorf("%s连接失败: %w", title, err), window)
				return
			}
			dialog.ShowInformation("连接成功", title+"连接正常。", window)
		})
	}()
}

// connectAndLoad 同时连接 A/B 数据库，并读取 A 库表清单刷新到界面。
func connectAndLoad(
	window fyne.Window,
	state *uiState,
	sourceCfg dbcopy.ConnConfig,
	targetCfg dbcopy.ConnConfig,
	table *widget.Table,
	statusLabel *widget.Label,
	copyLabel *widget.Label,
	setBusy func(bool),
) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()

	sourceConn, err := dbcopy.Connect(ctx, sourceCfg)
	if err != nil {
		fyne.Do(func() {
			setBusy(false)
			statusLabel.SetText("A 数据库连接失败")
			copyLabel.SetText("等待操作")
			dialog.ShowError(fmt.Errorf("A 数据库连接失败: %w", err), window)
		})
		return
	}

	targetConn, err := dbcopy.Connect(ctx, targetCfg)
	if err != nil {
		_ = sourceConn.Close(context.Background())
		fyne.Do(func() {
			setBusy(false)
			statusLabel.SetText("B 数据库连接失败")
			copyLabel.SetText("等待操作")
			dialog.ShowError(fmt.Errorf("B 数据库连接失败: %w", err), window)
		})
		return
	}

	tables, err := dbcopy.ListTables(ctx, sourceConn)
	if err != nil {
		_ = sourceConn.Close(context.Background())
		_ = targetConn.Close(context.Background())
		fyne.Do(func() {
			setBusy(false)
			statusLabel.SetText("读取表失败")
			copyLabel.SetText("等待操作")
			dialog.ShowError(fmt.Errorf("读取 A 数据库表清单失败: %w", err), window)
		})
		return
	}

	newRows := make([]tableRow, len(tables))
	for i, t := range tables {
		newRows[i] = tableRow{info: t, status: "等待复制"}
	}

	fyne.Do(func() {
		// 新连接可用后再替换旧连接，避免中间状态影响用户继续操作。
		state.mu.Lock()
		oldSource := state.sourceConn
		oldTarget := state.targetConn
		state.sourceConn = sourceConn
		state.targetConn = targetConn
		state.rows = newRows
		state.mu.Unlock()

		go closeConn(oldSource)
		go closeConn(oldTarget)
		table.Refresh()
		setBusy(false)
		statusLabel.SetText(fmt.Sprintf("已连接，读取到 %d 张表", len(tables)))
		copyLabel.SetText("请选择复制操作")
	})
}

// refreshTables 保留已有连接，只重新读取源库表列表。
func refreshTables(
	window fyne.Window,
	state *uiState,
	sourceConn *pgx.Conn,
	table *widget.Table,
	copyLabel *widget.Label,
	setBusy func(bool),
) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	tables, err := dbcopy.ListTables(ctx, sourceConn)
	if err != nil {
		fyne.Do(func() {
			setBusy(false)
			copyLabel.SetText("刷新失败")
			dialog.ShowError(fmt.Errorf("刷新表清单失败: %w", err), window)
		})
		return
	}

	newRows := make([]tableRow, len(tables))
	for i, t := range tables {
		newRows[i] = tableRow{info: t, status: "等待复制"}
	}

	fyne.Do(func() {
		state.mu.Lock()
		state.rows = newRows
		state.mu.Unlock()
		table.Refresh()
		setBusy(false)
		copyLabel.SetText(fmt.Sprintf("表清单已刷新，共 %d 张表", len(tables)))
	})
}

// copyAllTables 按表顺序串行复制，单表失败不会阻止后续表继续尝试。
func copyAllTables(
	ctx context.Context,
	state *uiState,
	sourceConn *pgx.Conn,
	targetConn *pgx.Conn,
	opts dbcopy.CopyOptions,
	table *widget.Table,
	progress *widget.ProgressBar,
	copyLabel *widget.Label,
	cancelButton *widget.Button,
	setBusy func(bool),
) {
	state.mu.RLock()
	rows := make([]tableRow, len(state.rows))
	copy(rows, state.rows)
	state.mu.RUnlock()

	total := len(rows)
	for i, row := range rows {
		// 取消信号在表与表之间检查；正在复制的大表由 pgx 查询上下文中断。
		if err := ctx.Err(); err != nil {
			updateRow(state, table, i, func(r *tableRow) {
				r.status = "已取消"
			})
			break
		}

		updateRow(state, table, i, func(r *tableRow) {
			r.status = "准备复制"
			r.copied = 0
			r.err = ""
		})
		fyne.Do(func() {
			copyLabel.SetText(fmt.Sprintf("正在复制 %s", row.info.Ref.DisplayName()))
		})

		copied, err := dbcopy.CopyTable(ctx, sourceConn, targetConn, row.info, opts, func(p dbcopy.CopyProgress) {
			updateRow(state, table, i, func(r *tableRow) {
				r.status = p.Phase
				r.copied = p.Copied
			})
		})
		if err != nil {
			message := err.Error()
			if errors.Is(err, context.Canceled) {
				message = "用户取消"
			}
			updateRow(state, table, i, func(r *tableRow) {
				r.status = "失败"
				if errors.Is(err, context.Canceled) {
					r.status = "已取消"
				}
				r.copied = copied
				r.err = message
			})
			if errors.Is(err, context.Canceled) {
				break
			}
			continue
		}

		updateRow(state, table, i, func(r *tableRow) {
			r.status = "完成"
			r.copied = copied
			r.err = ""
		})
		fyne.Do(func() {
			progress.SetValue(float64(i+1) / float64(total))
		})
	}

	fyne.Do(func() {
		state.mu.Lock()
		state.cancelCopy = nil
		state.copying = false
		state.mu.Unlock()
		cancelButton.Disable()
		setBusy(false)
		if ctx.Err() != nil {
			copyLabel.SetText("复制已取消")
			return
		}
		copyLabel.SetText("复制任务完成")
		progress.SetValue(1)
	})
}

// updateRow 统一在 UI 线程内修改表格行，避免后台 goroutine 直接碰 Fyne 控件。
func updateRow(state *uiState, table *widget.Table, index int, update func(*tableRow)) {
	fyne.Do(func() {
		state.mu.Lock()
		if index >= 0 && index < len(state.rows) {
			update(&state.rows[index])
		}
		state.mu.Unlock()
		table.Refresh()
	})
}

// closeConn 用短超时关闭连接，防止程序退出时长时间等待网络。
func closeConn(conn *pgx.Conn) {
	if conn == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = conn.Close(ctx)
}

// formatInt 给表格里的行数加千分位，便于扫读大表规模。
func formatInt(v int64) string {
	negative := v < 0
	if negative {
		v = -v
	}
	s := strconv.FormatInt(v, 10)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if negative {
		return "-" + s
	}
	return s
}
