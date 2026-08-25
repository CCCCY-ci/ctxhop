# CtxHop

<p align="center">
  <img src="assets/ctxhop-logo.png" alt="CtxHop Logo" width="180">
</p>

<p align="center">
  <a href="https://github.com/CCCCY-ci/ctxhop/releases/latest"><img src="https://img.shields.io/github/v/release/CCCCY-ci/ctxhop?sort=semver" alt="Latest release"></a>
  <a href="https://github.com/CCCCY-ci/ctxhop/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-blue.svg" alt="Apache-2.0 License"></a>
</p>

[English](README.md) | 简体中文

**换设备，不换上下文。**

CtxHop 是一个面向 Claude Code、Codex 等 AI Coding Agent 的跨设备会话与工作区同步工具。你可以在一台设备上开始开发，在另一台设备上恢复原来的 Session 并继续工作；源设备无需保持在线。

CtxHop 按项目同步 Agent Session，并可按需携带有限的工作区与 Git 状态。数据在离开设备前完成本地加密，存储后端由你控制，可使用本地目录或 Cloudflare R2 等 S3 兼容对象存储。

## 主要功能

- **跨设备续接 Session**：同步 Claude Code 和 Codex 的项目级会话，在另一台已授权设备上直接恢复并继续。
- **可选工作区交接**：使用 `--workspace` 时，同时携带有限的项目文件和 Git 状态，适合处理尚未提交的工作。
- **项目级同步边界**：只有显式绑定的项目和已授权设备参与同步，不扫描或传输无关项目。
- **自托管存储**：支持本地目录与 S3 兼容对象存储，包括 Cloudflare R2。
- **安全恢复**：`resume --preview` 可在写入前预览恢复内容；冲突检查失败时不会修改目标工作区。

## 演示

[观看 CtxHop 演示视频](assets/ctxhop-demo.mp4)

## 安装

### Windows

从 [Releases](https://github.com/CCCCY-ci/ctxhop/releases) 下载对应架构的安装器：

- `CtxHop-Setup_<version>_windows_amd64.exe`
- `CtxHop-Setup_<version>_windows_arm64.exe`

安装完成后，CtxHop 会自动打开命令提示符，显示 ASCII Logo 和下一步操作。可以直接运行：

```powershell
ctxhop init
```

### macOS / Linux

下载对应平台的 Release 压缩包并安装：

```bash
unzip ctxhop_<version>_<os>_<arch>.zip
sh install.sh
```

默认安装到 `$XDG_BIN_HOME` 或 `$HOME/.local/bin`。自定义目录：

```bash
CTXHOP_INSTALL_DIR=/path/to/bin sh install.sh
```

### Go Install

```bash
go install github.com/CCCCY-ci/ctxhop/cmd/ctxhop@latest
```

### 卸载

```bash
ctxhop uninstall
```

卸载会移除 CLI、本地 `~/.ctxhop` 配置目录、设备密钥、本地状态、日志以及
CtxHop 注册的 Agent Hook。S3 远端对象和已配置的本地目录后端会保留，不会被删除。
如果本地目录后端与 `.ctxhop` 目录重叠，卸载会停止并提示你先移动同步目录。

## 快速开始：Cloudflare R2

下面以 Cloudflare R2 为共享后端。完整流程只有四步：初始化设备 A、绑定并上传项目、授权设备 B、恢复 Session。

开始前，请准备好 R2 Bucket、对应的 Access Key / Secret Access Key，以及两台设备上的项目工作副本。

R2 配置示例：

```text
Endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
Bucket:   <BUCKET_NAME>
Region:   auto
Prefix:   ctxhop/demo     # 可选
```

同一同步域中的所有设备必须使用相同的 Bucket 和 Prefix。多个同步域共用一个 Bucket 时，建议使用不同 Prefix 隔离。

### 1. 初始化设备 A

```bash
ctxhop init \
  --backend s3 \
  --endpoint "https://<ACCOUNT_ID>.r2.cloudflarestorage.com" \
  --bucket "<BUCKET_NAME>" \
  --region "auto" \
  --prefix "ctxhop/demo" \
  --device-name "device-a"
```

按提示输入 R2 凭据和加密密码。普通 R2 API Token 不需要 Session Token，可直接留空。

首次初始化会生成 **Recovery Key**，请离线保存。加密密码和 Recovery Key 同时丢失后，远端加密数据无法恢复。

初始化过程中，CtxHop 会先检测 Claude Code 和 Codex，再询问 Agent 相关选项。如果检测到其中至少一个，可以输入 `1` 或 `2` 选择自动 SessionEnd Hook 的同步范围：

- `1` — `session`：同步 Session 和过滤后的环境信息；
- `2` — `session+workspace`：在上述内容之外，再同步项目工作区与 Git 状态。

Hook 步骤完成后（或通过 `--no-hook` 跳过 Hook 安装后），CtxHop 才会询问是否同步检测到的 Agent 的过滤后配置，默认开启。如果同时安装了 Claude Code 和 Codex，这个选择同时适用于两个 Agent。只有白名单中的设置和不含敏感信息的 MCP 传输意图会进入同步范围。

CtxHop 永远不会上传完整的 Agent 配置文件或鉴权文件，包括 Claude Code 的凭据与登录状态、Codex 的鉴权数据、Token、Header、环境变量以及 MCP 的 `env` 值。程序可以在本地读取 Claude 用户配置文件，从中提取当前 Session 实际引用的 MCP 命令、参数或 URL，但不会上传该文件本身。输入 `no` 后，Session 和其他允许同步的环境组件仍会上传，但后续上传会排除设置与 MCP 配置。

Hook 只在会话结束时自动执行对应范围的 `push`。安装后，每次对话结束都会自动上传，不需要在每次对话后手动执行 `ctxhop push`。也可以之后手动安装：

```bash
ctxhop hook install --agent codex
# 或
ctxhop hook install --agent claude-code
```

不使用 Hook 时，在初始化阶段指定 `--no-hook`。

如果要让已经初始化的设备切换到另一个同步域，再次执行 `ctxhop init` 即可。
CtxHop 会先询问是否退出当前同步域：

- 输入 `y`：退出当前同步域并继续新的初始化，同时删除本机配置、设备密钥和
  CtxHop 注册的 Hook；远端 S3 对象和独立的目录后端同步目录会保留。
- 输入 `n` 或直接回车：保留当前同步域并取消本次初始化。

如果配置的目录后端与本地 CtxHop 配置目录重叠，请先移动同步目录；CtxHop 会在删除本地同步域状态前停止操作。

### 2. 绑定项目并上传

在项目目录中执行：

```bash
cd /path/to/project
ctxhop project bind --path .
ctxhop push
```

默认 `push` 只同步当前项目的 Session 和过滤后的环境信息。

如果还需要交接未提交的工作区与 Git 状态：

```bash
ctxhop push --workspace
```

没有可用 Git 身份时，可以手动指定项目名；两台设备需使用同一个名称：

```bash
ctxhop project bind --name "my-project" --path .
```

手动项目名用于标识项目，与本地目录路径无关。

`pull`、`list` 和 `resume` 只查询当前目录绑定的项目。`ctxhop project discover`
是例外，它会列出同步域中已授权设备发布的全部项目，方便在绑定新项目之前发现它。

### 3. 授权设备 B

在设备 A 生成邀请文件：

```bash
ctxhop device invite --output ctxhop-device-b.json
```

通过可信渠道将邀请文件传到设备 B，然后执行：

```bash
ctxhop init \
  --invite ./ctxhop-device-b.json \
  --device-name "device-b"
```

设备 B 仍需输入自己的 R2 凭据，以及与设备 A 相同的加密密码。邀请文件本身不包含存储凭据、加密密码或 Session 内容。

### 4. 恢复 Session

如果需要先查看其他设备发布的项目：

```bash
ctxhop project discover
```

在设备 B 准备好对应项目后：

```bash
cd /path/to/project
ctxhop project bind --path .
ctxhop list
```

`list` 列出当前项目可恢复的 Session。源设备安装了 SessionEnd Hook 时，最近一次已结束的对话已经自动上传；在目标设备上直接执行 `list` 或 `resume` 即可读取最新的远端元数据，正常的切换流程不需要额外执行 `ctxhop pull`。如果只想检查远端元数据，再单独使用 `pull`。

恢复指定 Session：

```bash
ctxhop resume <SESSION_ID>
```

先预览、不写入：

```bash
ctxhop resume --preview <SESSION_ID>
```

如果源设备通过 `push --workspace` 上传了工作区数据，可一并恢复：

```bash
ctxhop resume --workspace <SESSION_ID>
```

`resume --workspace` 会先检查目标工作区和 Git 状态；存在冲突时直接停止，不会强制覆盖。

如果只需要 Session，不希望目标工作区差异阻止恢复：

```bash
ctxhop resume --allow-divergent <SESSION_ID>
```

恢复完成后，使用 Agent 原生命令继续会话：

```bash
# Codex
codex resume <SESSION_ID>

# Claude Code
claude --resume <SESSION_ID>
```

## 同步内容

| 内容 | 默认 | 说明 |
|---|---|---|
| Agent Session | 同步 | 本地压缩并加密后上传。 |
| 项目身份与 Git 摘要 | 同步 | 用于跨设备识别项目，不包含项目文件或完整 Git 对象。 |
| Session 相关环境 | 过滤后同步；可在 `init` 中选择配置是否同步 | Claude Code 和 Codex 通过各自 Adapter 仅恢复白名单中的 Skill、MCP 传输意图和 Session 设置。全局设置仍写回用户级配置，项目级 MCP 保持项目范围。完整配置文件和鉴权数据永不同步。 |
| 工作区与 Git 状态 | 按需 | 仅在 `push --workspace` / `resume --workspace` 时处理。 |
| Token、凭据与 `.env` | 永不同步 | Claude Code/Codex 登录状态与鉴权文件、私钥、Header、MCP `env` 值和 Secrets 等不会进入同步数据。 |

普通 `push`、Hook 和 `watch` 不上传项目文件正文。启用 `--workspace` 后，CtxHop 也不会自动删除本地文件、切换分支，或执行 merge、rebase、commit、push、reset 等 Git 操作。

敏感文件、二进制文件、超出大小限制的文件以及存在冲突的路径会保留给用户手动处理。

## 常用命令

执行 `ctxhop <command> --help` 查看完整参数。直接运行 `ctxhop` 会输出命令索引；使用 `ctxhop help <command>` 查看具体命令说明。下面列出所有一级命令和已支持的二级命令。脚本和 CI 可在支持的命令上使用 `--json`。

### 初始化与帮助

| 命令 | 说明 |
|---|---|
| `ctxhop init` | 配置存储后端、加密密码、设备和 Agent Hook。 |
| `ctxhop install` | 将 CtxHop 安装为当前用户的命令。 |
| `ctxhop uninstall` | 移除本地 CtxHop 文件，不删除远端或后端数据。 |
| `ctxhop help [<command>]` | 查看命令索引，或查看指定命令的说明和参数。 |
| `ctxhop version` | 查看已安装的版本。 |

### 项目与同步

| 命令 | 说明 |
|---|---|
| `ctxhop project bind` | 将本地项目绑定到稳定的项目身份。 |
| `ctxhop project unbind` | 移除本地项目绑定。 |
| `ctxhop project mode <mode>` | 将项目设置为 `normal`、`push-only` 或 `excluded`。 |
| `ctxhop project list` | 列出本地项目绑定。 |
| `ctxhop project discover` | 查看其他设备已发布的项目。 |
| `ctxhop push` | 上传当前项目的 Session 和过滤后的环境。 |
| `ctxhop push --workspace` | 同时上传有限工作区和 Git 状态。 |
| `ctxhop pull` | 检查远端元数据，不执行恢复。 |
| `ctxhop list` | 列出当前项目可恢复的 Session。 |
| `ctxhop resume` | 恢复 Session 和过滤后的环境；可按需使用 `--preview`、`--workspace` 或 `--allow-divergent`。 |
| `ctxhop watch` | 监视本地 Session 变化并上传。 |

### 设备与安全

| 命令 | 说明 |
|---|---|
| `ctxhop device invite` | 创建新设备邀请。 |
| `ctxhop device status` | 查看本地设备身份和模式。 |
| `ctxhop device mode <mode>` | 将设备设置为 `normal`、`push-only` 或 `disabled`。 |
| `ctxhop device list` | 查看已授权设备。 |
| `ctxhop device rename <name>` | 修改本地设备显示名称。 |
| `ctxhop device remove <device-id>` | 撤销设备后续访问权限。 |
| `ctxhop device rotate-key` | 轮换加密密钥。 |
| `ctxhop passphrase change` | 修改加密密码。 |
| `ctxhop passphrase reset` | 使用 Recovery Key 重置加密密码。 |

### 历史与远端数据

| 命令 | 说明 |
|---|---|
| `ctxhop history <SESSION_ID>` | 查看 Session 可恢复的历史版本。 |
| `ctxhop history cleanup <SESSION_ID>` | 删除某个 Session 的全部远端版本。 |
| `ctxhop history prune <SESSION_ID>` | 按保留数量或日期清理 Session 版本。 |
| `ctxhop remote delete-session <SESSION_ID>` | 确认后删除一个远端 Session。 |
| `ctxhop remote delete-project` | 确认后删除当前项目的全部远端数据。 |
| `ctxhop remote delete-all` | 确认后删除当前同步域中的全部远端数据。 |

### 状态与维护

| 命令 | 说明 |
|---|---|
| `ctxhop status` | 查看本地状态；`--remote` 同时检查远端。 |
| `ctxhop doctor` | 检查配置、存储后端、Agent 和项目状态。 |
| `ctxhop stats` | 查看跨设备恢复统计。 |
| `ctxhop hook install` | 安装 Claude Code / Codex SessionEnd Hook。 |

## 配置

CtxHop 默认使用 `~/.ctxhop` 保存本地配置、设备密钥和同步状态：

| 系统 | 默认目录 |
|---|---|
| Windows | `%USERPROFILE%\.ctxhop` |
| macOS / Linux | `~/.ctxhop` |

可通过 `CTXHOP_CONFIG_DIR` 修改配置目录：

```bash
export CTXHOP_CONFIG_DIR="$HOME/.ctxhop-custom"
```

PowerShell：

```powershell
$env:CTXHOP_CONFIG_DIR = Join-Path $env:USERPROFILE '.ctxhop-custom'
```

该目录包含本地加密信息和设备密钥，请勿提交到仓库或公开分享。

## 开发

从源码构建：

```bash
git clone https://github.com/CCCCY-ci/ctxhop.git
cd ctxhop
go build -trimpath -o ctxhop ./cmd/ctxhop
./ctxhop install
```

Windows：

```powershell
go build -trimpath -o ctxhop.exe ./cmd/ctxhop
.\ctxhop.exe install
```

检查：

```bash
go test ./...
go test -race ./...
go vet ./...
go build -trimpath -o ctxhop ./cmd/ctxhop
```

请勿将真实 Session 文件、Token 或后端凭据提交到仓库。

## 许可证

CtxHop 使用 [Apache License 2.0](LICENSE)。重新分发时请保留 [NOTICE](NOTICE) 文件。
