# 让 Agent 安装 dmon

把下方「复制给 Agent 的文本」整块发给任意 Agent（Claude、Codex、Hermes 等），它就会按当前系统的操作系统和架构，从 GitHub 最新 Release 下载对应版本并安装到用户环境，**无需安装 Go**。

- 项目：`github.com/difyz9/dmon-cli`
- 二进制：`dmon`（全称 Directory Monitor，目录监控）
- 安装脚本：[scripts/install.sh](scripts/install.sh)

## 复制给 Agent 的文本

> 直接复制下面整块即可：

```text
请安装 dmon 命令行工具（项目 github.com/difyz9/dmon-cli，Directory Monitor 目录监控），
按当前系统的操作系统和架构，从 GitHub 最新 Release 下载对应版本并安装到用户环境：

1. 首选运行官方安装脚本（自动识别 OS/架构、下载最新版、校验 sha256、安装到 ~/.local/bin）：
   curl -fsSL https://raw.githubusercontent.com/difyz9/dmon-cli/main/scripts/install.sh | bash

2. 脚本不可用时的后备：用 `uname -s` 和 `uname -m` 判断系统类型（darwin/linux × amd64/arm64），
   查询 https://api.github.com/repos/difyz9/dmon-cli/releases/latest 得到最新版本号 <TAG>，
   下载 https://github.com/difyz9/dmon-cli/releases/download/<TAG>/dmon_<TAG>_<OS>_<ARCH>.tar.gz，
   解压后把 dmon 安装到 ~/.local/bin，确保该目录在 PATH 中并赋予可执行权限。

3. 验证安装：dmon --version

4. 基本用法：
   - dmon init：生成配置（默认监听 ~/Downloads/dmon，init 会自动创建该目录）
   - dmon next --timeout 60s --lease 30m --max-files 1：等待并领取一个就绪文件（输出一行 JSON）
   - 处理成功后：dmon done <event_id>
   - 处理失败后：dmon retry <event_id> --reason '失败原因'
   - 诊断：dmon status、dmon queue status、dmon queue list --status dead
```

## 安装脚本做了什么

`scripts/install.sh` 的步骤：

1. 识别操作系统与架构（`uname -s` / `uname -m`，支持 darwin/linux × amd64/arm64）。
2. 通过 GitHub API 查询最新 Release 版本号。
3. 下载匹配的资产 `dmon_<版本>_<OS>_<ARCH>.tar.gz`。
4. 下载 `checksums.txt` 并校验 sha256，不匹配则中止。
5. 解压并把 `dmon` 安装到 `~/.local/bin`（可用环境变量 `DMON_INSTALL_DIR` 覆盖目录）。
6. macOS 上清除 Gatekeeper 隔离标记（`xattr`），运行 `dmon --version` 验证。

## 手动安装（脚本不可用的后备）

```bash
# 1. 判断系统类型
uname -s   # Linux / Darwin
uname -m   # x86_64 / arm64

# 2. 查最新版本号
curl -fsSL https://api.github.com/repos/difyz9/dmon-cli/releases/latest

# 3. 下载并安装（示例为 linux/amd64，替换 TAG、OS、ARCH）
curl -fsSL -o /tmp/dmon.tar.gz \
  https://github.com/difyz9/dmon-cli/releases/download/<TAG>/dmon_<TAG>_<OS>_<ARCH>.tar.gz
mkdir -p ~/.local/bin
tar -xzf /tmp/dmon.tar.gz -C ~/.local/bin dmon
chmod +x ~/.local/bin/dmon
export PATH="$HOME/.local/bin:$PATH"
dmon --version
```

## Windows 用户

安装脚本面向 Linux/macOS。Windows 请从 Release 页面下载 `dmon_<版本>_windows_amd64.zip`（或 arm64），解压后把 `dmon.exe` 放到一个已加入 PATH 的目录。

## 卸载

```bash
rm -f ~/.local/bin/dmon
```

## 常见问题

- **`dmon: command not found`**：`~/.local/bin` 不在 PATH 中，执行 `export PATH="$HOME/.local/bin:$PATH"`（可写入 `~/.zshrc` / `~/.bashrc`）。
- **macOS 提示「无法打开，因为无法验证开发者」**：执行 `xattr -dr com.apple.quarantine ~/.local/bin/dmon`（安装脚本已自动处理）。
- **没有 Go 也能装**：Release 提供预编译二进制；有 Go 时也可用 `go install github.com/difyz9/dmon-cli@latest`。
