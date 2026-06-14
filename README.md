# PostgreSQL 远程表复制工具

这是一个 Go + Fyne 桌面程序，用于把远程 PostgreSQL A 库中的普通表复制到远程 PostgreSQL B 库。

## 功能

- 首屏输入 A/B 两个远程 PostgreSQL 连接信息。
- 支持分别测试 A/B 连接，也支持连接后自动读取 A 库表清单。
- 以表格形式展示 A 库表，一行一个表，包含预估行数、状态、已复制行数和错误信息。
- 在 B 库自动创建 schema 和表列结构。
- 不创建外键、主键、唯一约束、索引、默认值或触发器，避免依赖关系影响插入顺序。
- 使用 pgx `COPY` 从 A 读取并写入 B，比逐行 INSERT 更适合大表复制。
- 每张表使用目标库事务包装；某张表复制失败时，该表的目标端建表/清空/写入会回滚。

## 运行

本机需要安装 Go 和 Fyne 的桌面编译依赖。

如果 Go 安装在 macOS 官方默认目录，但终端找不到 `go`，把下面一行加入 `~/.zshrc` 或 `~/.zprofile` 后重新打开终端：

```bash
export PATH="/usr/local/go/bin:$PATH"
```

```bash
go mod tidy
go run .
```

如果要打包桌面应用：

```bash
go install fyne.io/fyne/v2/cmd/fyne@latest
fyne package
```

# 打包win，先安装
brew install mingw-w64

# win下的gcc
/usr/local/bin/x86_64-w64-mingw32-gcc

# 执行打包
```bash
CGO_ENABLED=1 \
GOOS=windows \
GOARCH=amd64 \
CC=x86_64-w64-mingw32-gcc \
CXX=x86_64-w64-mingw32-g++ \
/Users/edy/go/bin/fyne package --target windows -icon ./Icon.png
```

跨平台打包请参考 Fyne 官方文档，不同系统需要对应的图形库/交叉编译环境。

## 复制规则

目标表默认通过以下方式创建：

```sql
CREATE SCHEMA IF NOT EXISTS "schema";
CREATE TABLE IF NOT EXISTS "schema"."table" ();
ALTER TABLE "schema"."table" ADD COLUMN IF NOT EXISTS "column" type;
```

为了兼容没有安装相同扩展或自定义类型的目标库，非 `pg_catalog` 类型、枚举、组合类型、部分 domain 会转换为 `text` 列，并在源端 `SELECT` 时转换为文本。

默认勾选“复制前清空目标表”。如果取消勾选，数据会追加写入目标表；若目标表已有数据或列类型不兼容，复制可能失败并显示在对应表行。

## 注意事项

- 该工具只复制普通表和分区父表可查询到的数据，不复制视图、函数、权限、索引、外键或触发器。
- 源库只执行 catalog 查询和 `SELECT`，不会修改源库。
- 如果目标库已经存在同名表，工具只会补充缺失列，不会修改已有列类型。
- 超大表复制期间可以点击“取消复制”，当前表事务会回滚。
