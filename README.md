# light-spool-cli

一个轻量、单二进制的目录文件采集器。它只通过 `stat` 读取文件元数据，**绝不读取文件内容**；业务 JSON 写到 stdout，运行日志写到 stderr。

## 构建

```bash
go build -o light-spool-cli .
```

## 使用

默认读取用户目录下的 `~/.config/light-spool-cli/config.yaml`。配置文件不存在时使用内置默认值：

```yaml
watch: /data/incoming
archive_dir: /data/archive
scan_interval: 2s
inactive: 3s
checkpoint: ./spool.db
include: '\.csv$|\.jpg$|\.mp4$'
exclude: '\.tmp$|\.part$'
```

配置完成后可直接运行：

```bash
light-spool-cli
```

也可以通过 `--config /path/to/config.yaml` 指定其他配置。命令行参数优先级高于 YAML，例如：

```bash
./light-spool-cli \
  --config ~/.config/light-spool-cli/config.yaml \
  --watch /data/incoming \
  --scan-interval 2s \
  --inactive 3s \
  --archive-dir /data/archive \
  --include '\.csv$|\.jpg$|\.mp4$' \
  --exclude '\.tmp$|\.part$' \
  --checkpoint ./spool.db
```

程序启动时立即递归扫描监控目录及其所有子目录，之后按 `--scan-interval` 轮询，因此适用于 inotify 不可靠的 NFS。文件的 inode、大小或 mtime 发生变化时会重新开始静止计时；连续静止达到 `--inactive` 后输出一次。文件再次变化后可以再次输出。重命名后旧路径状态会被清理，新路径作为新文件观察；删除文件对应的 checkpoint 也会自动清理。

设置 `--archive-dir` 后，JSON 成功写入 stdout 才会移动源文件。归档目录保留监控目录下的相对结构，例如 `incoming/camera-1/a.mp4` 会移动到 `archive/camera-1/a.mp4`。目标已存在时不会覆盖，只在 stderr 记录错误。归档使用原子 `rename`，因此监控目录与归档目录应位于同一文件系统；未设置该参数则不移动文件。

每批就绪文件输出为一行 JSON 数组，文件按路径排序：

```json
[{"file_path":"/data/incoming/a.csv","file_name":"a.csv","size":1234,"mtime":"2026-08-05T14:20:30+08:00","inode":143211,"ext":".csv"}]
```

可直接管道给下游：

```bash
./light-spool-cli 2>spool.log | python3 consumer.py
```

## 行为说明

- 递归扫描监控目录；动态新增的子目录和文件会在下一轮被发现并输出。
- include 为空表示全部包含；exclude 为空表示不排除。
- checkpoint 使用 bbolt 持久化，程序重启不会重复输出未变化的文件。
- 同一路径被新 inode 替换，或文件大小/mtime 改变，会被视作新版本。
- 支持 Linux/macOS；Windows 的 inode 退化为 0，但 size/mtime 和路径去重仍然有效。
