# dmon-cli

`dmon` 的全称是 **Directory Monitor**（目录监控）。一个轻量、单二进制的目录文件采集器。它只通过 `stat` 读取文件元数据，**绝不读取文件内容**；业务 JSON 写到 stdout，运行日志写到 stderr。

CLI 基于 [Cobra](https://github.com/spf13/cobra)，支持标准帮助、版本信息、参数校验和清晰的错误提示。

## 项目结构

```text
dmon-cli/
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
├── examples/dmon.yaml       # 完整配置样例
├── main.go                 # 仅调用 cmd.Execute()
├── go.mod
└── README.md
```

`internal` 中的实现不能被其他 Go module 直接导入，避免 CLI 内部协议意外成为公共 API。未来确实需要作为 Go 库使用时，再将稳定接口放入 `pkg`。

## Agent Skill

仓库提供精简的 [dmon Skill](skills/dmon/SKILL.md) 和 [中文 Skill 指南](skills/dmon/skill_zh.md)，指导 Agent 安装工具并选择最低心智负担的处理流程。英文主 Skill 会在中文任务中按需加载中文版。安装到 Codex：

```bash
mkdir -p ~/.codex/skills
cp -R skills/dmon ~/.codex/skills/dmon
```

Skill 默认引导 Agent 使用 `next → done/retry`，已有确定性处理程序时使用单命令 `next --exec`；只有明确需要持续事件流时才启动 watch。Release 压缩包中也包含该 Skill。

## 让 Agent 一键安装

把 [AGENT_INSTALL.md](AGENT_INSTALL.md) 中的文本复制给任意 Agent（Claude、Codex、Hermes 等），它就会按系统类型从 GitHub Release 下载最新版并安装，无需安装 Go：

```bash
curl -fsSL https://raw.githubusercontent.com/difyz9/dmon-cli/main/scripts/install.sh | bash
```

使用 Hermes 定时运行确定性媒体流水线时，参阅 [Hermes no-agent/script 模式学习笔记](docs/hermes-no-agent-script-runner.md)。

需要由 Hermes 定时创建 Agent 会话、加载 Skill 并驱动流水线时，参阅 [Hermes 定时唤醒 Agent 与视频流水线学习笔记](docs/hermes-scheduled-agent-video-pipeline.md)。

关于 FIFO、全局单任务、租约、延迟重试和死信机制的设计依据，参阅 [持久化队列设计](docs/durable-queue-design.md)。

未来需要安全地启用多个 Agent 并行消费时，参阅 [多 Agent 消费设计备忘](docs/multi-agent-consumer-design.md)。该方案目前仅作为后续设计，现版本仍建议使用 `max_inflight: 1`。

需要为 Hermes、Multica 或其他 Agent 定义可移植、可恢复的工作流任务链时，参阅 [Agent 工作流任务链设计教程](docs/agent-workflow-task-chain-guide.md)。

想直接运行一个通过 `echo` 写入 `task.md` 的教学任务链，可使用 [Agent 工作流演示项目](examples/agent-workflow-demo/README.md)。它包含 AGENTS.md、Skill、workflow、runner、manifest，以及 Hermes/Multica 测试步骤。

## 构建

```bash
go build -o dmon .
```

推送任意 Git tag 会触发 GitHub Actions，由 [GoReleaser](https://goreleaser.com)（配置见 `.goreleaser.yml`）构建 Linux、macOS、Windows 的 amd64/arm64 压缩包，生成 SHA256 校验文件并自动创建 GitHub Release：

```bash
git tag v0.1.0
git push origin v0.1.0
```

## 使用

默认读取用户目录下的 `~/.config/dmon/dmon.yaml`。可以通过 init 命令生成配置：

```bash
dmon init
```

仓库内也提供了可复制的 [examples/dmon.yaml](examples/dmon.yaml)。

已有配置默认不会覆盖；需要重新生成时显式执行 `dmon init --force`。

init 会在用户的下载目录下创建 `dmon` 文件夹（如 `~/Downloads/dmon`），并把它的绝对路径写入配置。生成的配置内容如下：

```yaml
watch: ~/Downloads/dmon   # init 写入绝对路径，例如 /Users/apple/Downloads/dmon
archive_dir: ""
scan_interval: 2s
inactive: 3s
include: '\.csv$|\.jpg$|\.mp4$'
exclude: '\.tmp$|\.part$'
queue:
  max_inflight: 1
  retry_delay: 30s
  max_attempts: 5
```

配置完成后可直接运行：

```bash
dmon
```

配置文件不存在时使用内置默认值。也可以通过 `--config /path/to/config.yaml` 指定其他配置。命令行参数优先级高于 YAML，例如：

```bash
./dmon \
  --config ~/.config/dmon/dmon.yaml \
  --watch ~/Downloads/dmon \
  --scan-interval 2s \
  --inactive 3s \
  --archive-dir ~/Downloads/dmon-archive \
  --include '\.csv$|\.jpg$|\.mp4$' \
  --exclude '\.tmp$|\.part$' \
  --checkpoint ~/.local/state/dmon/state.db
```

程序启动时立即递归扫描监控目录及其所有子目录，之后按 `--scan-interval` 轮询，因此适用于 inotify 不可靠的 NFS。文件的 inode、大小或 mtime 发生变化时会重新开始静止计时；连续静止达到 `--inactive` 后输出一次。文件再次变化后可以再次输出。重命名后旧路径状态会被清理，新路径作为新文件观察；删除文件对应的 checkpoint 也会自动清理。

默认状态库存放在 `~/.local/state/dmon/state.db`（设置了 `XDG_STATE_HOME` 时使用该目录），不再依赖当前工作目录。YAML 中仍可通过 `checkpoint` 覆盖。

设置 `--archive-dir` 后，常驻 watch 模式在 JSON 成功写入 stdout 后移动源文件；Agent 的可靠投递模式则在 `done` 后移动。归档目录保留监控目录下的相对结构，例如 `incoming/camera-1/a.mp4` 会移动到 `archive/camera-1/a.mp4`。目标已存在时不会覆盖。归档使用原子 `rename`，因此监控目录与归档目录应位于同一文件系统；未设置该参数则不移动文件。

watch 和 next 均使用 NDJSON，每个就绪文件输出一行 JSON：

```json
{"event_id":"mec...-a13f...","event":"file_ready","status":"acknowledged","file_path":"/Users/apple/Downloads/dmon/a.csv","file_name":"a.csv","size":1234,"mtime":"2026-08-05T14:20:30+08:00","inode":143211,"ext":".csv"}
```

可直接管道给下游：

```bash
./dmon 2>dmon.log | python3 consumer.py
```

## Agent 可靠投递

Agent 应优先使用 `next`，它等待文件稳定、原子领取事件并退出。每个文件版本拥有持久化且稳定的 `event_id`：

```bash
dmon next --timeout 30s --lease 5m --max-files 1
```

`next` 使用 NDJSON，每行一个事件：

```json
{"event_id":"mec...-a13f...","event":"file_ready","status":"claimed","lease_until":"2026-08-05T20:05:00+08:00","file_path":"/Users/apple/Downloads/dmon/a.csv","file_name":"a.csv","size":1234,"mtime":"2026-08-05T19:59:50+08:00","inode":143211,"ext":".csv"}
```

Agent 处理成功后确认；配置了归档目录时，此时才会移动文件：

```bash
dmon done 'mec...-a13f...'
```

处理失败时释放到队尾，并在配置的 `retry_delay` 后重新领取；`--reason` 会持久化失败原因：

```bash
dmon retry 'mec...-a13f...' --reason 'subtitle command failed'
```

队列默认 `max_inflight: 1`：只要已有事件处于 `claimed`，后续 `next` 就不领取新文件，而以退出码 3 表示“流水线忙碌”。退出码 2 表示等待超时/当前无文件；Hermes 定时任务可将 2 和 3 都视为静默结束。新文件仍会持续进入持久化 `ready` 队列，必须等上一个事件 `done` 或租约过期后才会投递。

事件按 `ready_at` 严格 FIFO 领取，相同时间按路径排序。每次领取增加 `attempt_count`；失败达到 `max_attempts` 后进入 `dead`，避免坏文件永久阻塞队列：

```bash
dmon queue status
dmon queue list --status ready
dmon queue list --status dead
dmon queue restore 'mec...-a13f...'
```

如果 Agent 异常退出且没有 done，租约到期后事件会再次投递，但 `event_id` 保持不变。Agent 应以 `event_id` 作为幂等键，避免极端故障窗口中的重复业务处理。其他错误退出码为 1。旧命令 `wait/ack/nack` 分别作为 `next/done/retry` 的兼容别名保留。

简单的下游命令可以让 CLI 自动确认或释放。子命令退出码为 0 时自动 done，非 0 时自动 retry；文件信息通过环境变量传递，子命令输出进入 stderr，不污染事件 JSON：

```bash
dmon next --exec 'processor "$DIRWATCH_FILE_PATH"'
```

可用变量包括 `DIRWATCH_EVENT_ID`、`DIRWATCH_FILE_PATH` 和 `DIRWATCH_FILE_NAME`。

查看全部或指定状态的 checkpoint：

```bash
dmon state list
dmon state list --status claimed
dmon state list --status acknowledged
dmon status
```

## 行为说明

- 递归扫描监控目录；动态新增的子目录和文件会在下一轮被发现并输出。
- include 为空表示全部包含；exclude 为空表示不排除。
- checkpoint 使用 bbolt 持久化，程序重启不会重复输出未变化的文件。
- 发现与领取分为两个阶段；领取在单个 bbolt 事务内执行，并发唤醒也受全局 `max_inflight` 约束。
- 同一 checkpoint 同时只由一个进程打开；`next` 返回并释放数据库后再执行 `done` 或 `retry`。
- 同一路径被新 inode 替换，或文件大小/mtime 改变，会被视作新版本。
- 支持 Linux/macOS；Windows 的 inode 退化为 0，但 size/mtime 和路径去重仍然有效。
