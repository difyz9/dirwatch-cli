# dirwatch-cli

一个轻量、单二进制的目录文件采集器。它只通过 `stat` 读取文件元数据，**绝不读取文件内容**；业务 JSON 写到 stdout，运行日志写到 stderr。

CLI 基于 [Cobra](https://github.com/spf13/cobra)，支持标准帮助、版本信息、参数校验和清晰的错误提示。

## 项目结构

```text
dirwatch/
├── cmd/                    # Cobra 命令、配置加载和运行编排
│   ├── config.go
│   ├── root.go
│   └── root_test.go
├── internal/
│   ├── collector/          # 递归扫描、静止判定、过滤和归档
│   ├── checkpoint/         # bbolt 状态存储与 ack/nack
│   └── model/              # FileItem、CheckpointRecord 等模型
├── pkg/                    # 预留的公共 API 包
├── configs/default.yaml    # 内置默认值参考
├── examples/dirwatch.yaml  # 完整配置样例
├── main.go                 # 仅调用 cmd.Execute()
├── go.mod
└── README.md
```

`internal` 中的实现不能被其他 Go module 直接导入，避免 CLI 内部协议意外成为公共 API。未来确实需要作为 Go 库使用时，再将稳定接口放入 `pkg`。

## Agent Skill

仓库提供精简的 [dirwatch-cli Skill](skills/dirwatch-cli/SKILL.md) 和 [中文 Skill 指南](skills/dirwatch-cli/skill_zh.md)，指导 Agent 安装工具并选择最低心智负担的处理流程。英文主 Skill 会在中文任务中按需加载中文版。安装到 Codex：

```bash
mkdir -p ~/.codex/skills
cp -R skills/dirwatch-cli ~/.codex/skills/dirwatch-cli
```

Skill 默认引导 Agent 使用 `next → done/retry`，已有确定性处理程序时使用单命令 `next --exec`；只有明确需要持续事件流时才启动 watch。Release 压缩包中也包含该 Skill。

## 构建

```bash
go build -o dirwatch-cli .
```

推送任意 Git tag 会触发 GitHub Actions，构建 Linux、macOS、Windows 的 amd64/arm64 压缩包，生成 SHA256 校验文件并创建 GitHub Release：

```bash
git tag v0.1.0
git push origin v0.1.0
```

## 使用

默认读取用户目录下的 `~/.config/dirwatch-cli/dirwatch.yaml`。可以通过 init 命令生成配置：

```bash
dirwatch-cli init
```

仓库内也提供了可复制的 [examples/dirwatch.yaml](examples/dirwatch.yaml)。

已有配置默认不会覆盖；需要重新生成时显式执行 `dirwatch-cli init --force`。

生成的配置内容如下：

```yaml
watch: /data/incoming
archive_dir: /data/archive
scan_interval: 2s
inactive: 3s
include: '\.csv$|\.jpg$|\.mp4$'
exclude: '\.tmp$|\.part$'
```

配置完成后可直接运行：

```bash
dirwatch-cli
```

配置文件不存在时使用内置默认值。也可以通过 `--config /path/to/config.yaml` 指定其他配置。命令行参数优先级高于 YAML，例如：

```bash
./dirwatch-cli \
  --config ~/.config/dirwatch-cli/dirwatch.yaml \
  --watch /data/incoming \
  --scan-interval 2s \
  --inactive 3s \
  --archive-dir /data/archive \
  --include '\.csv$|\.jpg$|\.mp4$' \
  --exclude '\.tmp$|\.part$' \
  --checkpoint ~/.local/state/dirwatch-cli/state.db
```

程序启动时立即递归扫描监控目录及其所有子目录，之后按 `--scan-interval` 轮询，因此适用于 inotify 不可靠的 NFS。文件的 inode、大小或 mtime 发生变化时会重新开始静止计时；连续静止达到 `--inactive` 后输出一次。文件再次变化后可以再次输出。重命名后旧路径状态会被清理，新路径作为新文件观察；删除文件对应的 checkpoint 也会自动清理。

默认状态库存放在 `~/.local/state/dirwatch-cli/state.db`（设置了 `XDG_STATE_HOME` 时使用该目录），不再依赖当前工作目录。YAML 中仍可通过 `checkpoint` 覆盖。

设置 `--archive-dir` 后，常驻 watch 模式在 JSON 成功写入 stdout 后移动源文件；Agent 的可靠投递模式则在 `done` 后移动。归档目录保留监控目录下的相对结构，例如 `incoming/camera-1/a.mp4` 会移动到 `archive/camera-1/a.mp4`。目标已存在时不会覆盖。归档使用原子 `rename`，因此监控目录与归档目录应位于同一文件系统；未设置该参数则不移动文件。

watch 和 next 均使用 NDJSON，每个就绪文件输出一行 JSON：

```json
{"event_id":"mec...-a13f...","event":"file_ready","status":"acknowledged","file_path":"/data/incoming/a.csv","file_name":"a.csv","size":1234,"mtime":"2026-08-05T14:20:30+08:00","inode":143211,"ext":".csv"}
```

可直接管道给下游：

```bash
./dirwatch-cli 2>dirwatch.log | python3 consumer.py
```

## Agent 可靠投递

Agent 应优先使用 `next`，它等待文件稳定、原子领取事件并退出。每个文件版本拥有持久化且稳定的 `event_id`：

```bash
dirwatch-cli next --timeout 30s --lease 5m --max-files 1
```

`next` 使用 NDJSON，每行一个事件：

```json
{"event_id":"mec...-a13f...","event":"file_ready","status":"claimed","lease_until":"2026-08-05T20:05:00+08:00","file_path":"/data/incoming/a.csv","file_name":"a.csv","size":1234,"mtime":"2026-08-05T19:59:50+08:00","inode":143211,"ext":".csv"}
```

Agent 处理成功后确认；配置了归档目录时，此时才会移动文件：

```bash
dirwatch-cli done 'mec...-a13f...'
```

处理失败时立即释放，供下一次 `next` 重新领取：

```bash
dirwatch-cli retry 'mec...-a13f...'
```

如果 Agent 异常退出且没有 done，租约到期后事件会再次投递，但 `event_id` 保持不变。Agent 应以 `event_id` 作为幂等键，避免极端故障窗口中的重复业务处理。`next --timeout` 超时退出码为 2，其他错误退出码为 1。旧命令 `wait/ack/nack` 分别作为 `next/done/retry` 的兼容别名保留。

简单的下游命令可以让 CLI 自动确认或释放。子命令退出码为 0 时自动 done，非 0 时自动 retry；文件信息通过环境变量传递，子命令输出进入 stderr，不污染事件 JSON：

```bash
dirwatch-cli next --exec 'processor "$DIRWATCH_FILE_PATH"'
```

可用变量包括 `DIRWATCH_EVENT_ID`、`DIRWATCH_FILE_PATH` 和 `DIRWATCH_FILE_NAME`。

查看全部或指定状态的 checkpoint：

```bash
dirwatch-cli state list
dirwatch-cli state list --status claimed
dirwatch-cli state list --status acknowledged
dirwatch-cli status
```

## 行为说明

- 递归扫描监控目录；动态新增的子目录和文件会在下一轮被发现并输出。
- include 为空表示全部包含；exclude 为空表示不排除。
- checkpoint 使用 bbolt 持久化，程序重启不会重复输出未变化的文件。
- 同一 checkpoint 同时只由一个进程打开；`next` 返回并释放数据库后再执行 `done` 或 `retry`。
- 同一路径被新 inode 替换，或文件大小/mtime 改变，会被视作新版本。
- 支持 Linux/macOS；Windows 的 inode 退化为 0，但 size/mtime 和路径去重仍然有效。
