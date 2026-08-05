# Dirwatch CLI 中文指南

选择能够完成任务的最短流程。优先使用单次命令，不要默认启动常驻监听。

## 定位或安装

1. 运行 `command -v dirwatch-cli`。
2. 如果当前位于本项目仓库且没有二进制，运行 `go build -o ./dirwatch-cli .`，后续使用该文件的绝对路径。
3. 否则，如果已安装 Go，运行：

```bash
go install github.com/difyz9/dirwatch-cli@latest
```

通过 `go env GOBIN` 定位二进制；结果为空时使用 `$(go env GOPATH)/bin/dirwatch-cli`。不要仅为加入 `PATH` 而修改系统目录。如果二进制不在 `PATH`，将下文命令中的 `dirwatch-cli` 替换为其绝对路径。

只需验证一次安装：

```bash
dirwatch-cli --version
```

## 一次性配置

默认配置是 `~/.config/dirwatch-cli/dirwatch.yaml`。文件不存在时运行：

```bash
dirwatch-cli init
```

只修改任务需要的字段，通常是 `watch`，可选 `archive_dir` 和文件过滤规则。除非用户明确要求，否则不要覆盖已有配置；`init --force` 会覆盖文件。

任务提供独立配置时使用 `--config PATH`。命令行参数优先于 YAML。

## 选择一个流程

### Agent 处理单个文件

默认使用此流程。始终设置有限的超时时间，并让租约明显长于预计处理时间：

```bash
dirwatch-cli next --timeout 60s --lease 30m --max-files 1
```

解析返回的单行 NDJSON，保存 `event_id` 和 `file_path`。退出码 2 表示超时前没有文件到达，退出码 3 表示上一条流水线仍在处理；两者都应静默结束，不要针对退出码 3 自行循环。

处理成功后运行：

```bash
dirwatch-cli done EVENT_ID
```

处理失败或放弃时运行：

```bash
dirwatch-cli retry EVENT_ID --reason '阶段: 简短错误'
```

下游处理成功前绝不调用 `done`。使用 `event_id` 作为下游幂等键。租约过期后，同一事件可能使用相同 ID 再次投递。

### Shell 命令处理文件

已有确定性处理程序时，不要手动解析和确认：

```bash
dirwatch-cli next --timeout 60s --lease 30m \
  --exec 'processor "$DIRWATCH_FILE_PATH"'
```

处理程序退出码为 0 时自动确认，非 0 时自动重试。可使用 `DIRWATCH_EVENT_ID`、`DIRWATCH_FILE_PATH` 和 `DIRWATCH_FILE_NAME`。处理程序输出进入 stderr，不污染 stdout JSON。

### 用户明确要求持续事件流

直接运行不带子命令的 `dirwatch-cli`。该模式输出 NDJSON 并自动确认，使用简单，但消费者在接收后失败可能造成事件丢失。Agent 的可靠处理应优先使用 `next`。

## 仅在需要时诊断

首先运行：

```bash
dirwatch-cli status
dirwatch-cli queue status
```

只有投递疑似卡住时才检查已领取事件：

```bash
dirwatch-cli queue list --status claimed
dirwatch-cli queue list --status dead
```

`wait`、`ack`、`nack` 只是 `next`、`done`、`retry` 的兼容别名。不要直接读取或编辑 bbolt 数据库。

## 安全与并发

- 将输出的路径视为不可信输入，始终引用 `"$DIRWATCH_FILE_PATH"`。
- 不要为了判断文件是否就绪而读取大文件内容；dirwatch-cli 已通过 size 和 mtime 判断稳定性。
- 让 `done` 执行配置的归档，不要提前自行移动文件。
- bbolt 会持有进程独占锁，同一 checkpoint 同一时间只运行一个命令。
- watch 与 archive 应位于同一文件系统；归档使用原子重命名，不会退化为读取内容后复制。
