# CtxHop

<p align="center">
  <img src="assets/ctxhop-logo.png" alt="CtxHop Logo" width="180">
</p>

<p align="center">
  <a href="https://github.com/CCCCY-ci/ctxhop/releases/latest"><img src="https://img.shields.io/github/v/release/CCCCY-ci/ctxhop?sort=semver" alt="Latest release"></a>
  <a href="https://github.com/CCCCY-ci/ctxhop/blob/main/LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg" alt="MIT License"></a>
</p>

<p align="center">
  <img src="assets/home.png" alt="CtxHop 欢迎页" width="900">
</p>

[English](README.md) | 简体中文

**换设备，不换上下文。**

CtxHop 是一个本地优先的 CLI，用于在设备之间传递 Claude Code 和 Codex
Session。将项目绑定到 Session Hub，通过你控制的存储同步 Session，再在任意已授权
设备上恢复。

Session Hub 将 Agent 原生 Session 组织为逻辑 Session，保留来源关系，并支持根据所选
上下文创建目标 Agent 的原生 Session。工作区和 Git 交接由用户显式选择，本地加密和
设备授权保护同步边界。

## 主要功能

- **跨设备恢复 Session**：在另一台已授权设备上继续项目 Session。
- **Session Hub**：将 Claude Code 和 Codex Session 组织到同一个逻辑 Session，同时保留原生来源和历史。
- **Agent 切换**：使用 `ctxhop session switch` 将所选上下文带入另一个 Agent 的新原生 Session。
- **工作区交接**：按需随 Session 携带指定的工作区文件与 Git 状态。
- **本地优先存储**：数据在设备本地加密，并保存到由你控制的后端。

## 结构关系

CtxHop 使用清晰的层级关系：

~~~text
Domain
└── Hub
    └── Project
        └── Session
            ├── Claude Code 原生 Session / Replica
            └── Codex 原生 Session / Replica
~~~

- **Domain**：加密同步边界。Remote 命名空间、密钥文件和已授权设备共同定义一个共享数据空间。
- **Hub**：Domain 内的项目组织空间，用于分组和隔离项目；新建 Domain 时会自动创建 `default` Hub。
- **Project**：工作区、Git 状态和 Session 的项目级边界。
- **Session**：跨 Agent 共享的一段逻辑开发上下文。

在常规操作中，Domain 和 `default` Hub 由系统自动处理，用户直接操作当前 Project 及其
Session。仅当需要在同一授权 Domain 内划分不同项目组时，才需要切换或创建其他 Hub。

## 演示

![CtxHop 演示](assets/ctxhop.gif)

## 安装

从 [Releases](https://github.com/CCCCY-ci/ctxhop/releases) 下载对应平台和 CPU 架构的安装包。

### Windows

下载并运行对应架构的安装器：

- `CtxHop-Setup_<version>_windows_amd64.exe`
- `CtxHop-Setup_<version>_windows_arm64.exe`

安装器将 CtxHop 安装到 `%USERPROFILE%\.ctxhop\bin`，并将目录加入当前用户的 PATH，无需管理员权限。
重新打开终端后验证：

```powershell
ctxhop version
```

需要便携使用时，下载 `ctxhop_<version>_windows_<arch>.zip`，解压
`ctxhop.exe`，再将所在目录加入 PATH。

### macOS / Linux

选择对应平台和 CPU 架构的压缩包：

- macOS Intel：`ctxhop_<version>_darwin_amd64.zip`
- macOS Apple Silicon：`ctxhop_<version>_darwin_arm64.zip`
- Linux x86_64：`ctxhop_<version>_linux_amd64.zip`
- Linux ARM64：`ctxhop_<version>_linux_arm64.zip`

在终端中解压并安装：

```bash
unzip ctxhop_<version>_<os>_<arch>.zip
sh install.sh
```

默认安装到 `$XDG_BIN_HOME`；未设置时使用 `$HOME/.local/bin`。自定义用户级安装目录：

```bash
CTXHOP_INSTALL_DIR=/path/to/bin sh install.sh
```

如果安装目录不在 PATH 中，安装脚本会输出所需的 Shell 配置。重新打开终端后验证：

```bash
ctxhop version
```

### 使用 Go 安装（可选）

需要 Go 1.26 或更高版本：

```bash
go install github.com/CCCCY-ci/ctxhop/cmd/ctxhop@latest
```

请确保 Go 的二进制目录已加入 PATH；需要固定版本时，将 `@latest` 换成发布标签。

### 初始化 CtxHop

在任意平台安装 CLI 后执行：

```bash
ctxhop init
```

该命令配置存储、加密、设备身份和 Agent Hook，并创建或加入同步域。

### 卸载

```bash
ctxhop uninstall
```

卸载会移除本地 CLI、配置、设备密钥、状态、日志和 CtxHop 注册的 Agent Hook，远端对象及本地目录后端数据会保留。
如果目录后端与本地配置目录重叠，请先移动同步目录。

## 快速开始：Cloudflare R2

下面以 Cloudflare R2 为共享后端，其他 S3 兼容对象存储的流程相同。
开始前准备 R2 Bucket、Access Key、Secret Access Key，以及两台设备上的项目工作副本。

以下命令使用系统自动创建的 `default` Hub。

R2 配置示例：

```text
Endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
Bucket:   <BUCKET_NAME>
Region:   auto
Prefix:   ctxhop/demo     # 可选
```

同一同步域中的所有设备使用相同的 Bucket 和 Prefix。

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
首次初始化会生成 **Recovery Key**，请离线保存。

### 2. 绑定项目并上传

设备 A 完成一次 Session 后，在项目目录中执行：

```bash
cd /path/to/project
ctxhop project bind --path .
ctxhop push
```

需要交接未提交的工作区与 Git 状态时：

```bash
ctxhop push --workspace
```

### 3. 授权设备 B

在设备 A 生成邀请文件：

```bash
ctxhop device invite --output ctxhop-device-b.json
```

将邀请文件传到设备 B，然后执行：

```bash
ctxhop init \
  --invite ./ctxhop-device-b.json \
  --device-name "device-b"
```

设备 B 输入自己的 R2 凭据，以及与设备 A 相同的加密密码。

### 4. 恢复 Session

在设备 B 准备好对应项目后：

```bash
cd /path/to/project
ctxhop project bind --path .
ctxhop list
ctxhop resume <SESSION_ID>
```

恢复完成后，使用 Agent 原生命令继续会话：

```bash
# Codex
codex resume <SESSION_ID>

# Claude Code
claude --resume <SESSION_ID>
```

### 可选：切换到另一个 Agent

跨 Agent 切换会创建目标 Agent 的新 Session，源 Session 保持不变：

```bash
# 预览切换
ctxhop session switch <SESSION_ID> --to codex --preview

# 创建并启动目标 Session
ctxhop session switch <SESSION_ID> --to codex --launch
```

切换到 Claude Code 时，将 `--to codex` 替换为 `--to claude-code`。

## 同步内容

CtxHop 默认同步加密的 Session 上下文和项目识别信息。
工作区文件与 Git 状态只有在使用 `--workspace` 时才会同步。
Agent 配置会在同步前经过筛选。

| 内容 | 范围 |
|---|---|
| Session 上下文 | 默认同步，以加密的 Agent Session 记录保存。 |
| 项目识别信息与 Git 摘要 | 默认同步，用于在不同设备上匹配同一个项目。 |
| Agent 环境 | 根据 `init` 配置同步经过筛选的 Skills、MCP 意图和允许的 Session 设置。 |
| 工作区与 Git 状态 | 可选，仅由 `push --workspace` 和 `resume --workspace` 传输。 |
| 凭据与敏感信息 | 永不同步，包括 token、私钥、登录文件、headers、环境变量密钥和 `.env` 文件。 |

项目文件和完整 Git 仓库不属于默认同步范围。

## 常用命令

执行 `ctxhop <command> --help` 查看参数，或使用 `ctxhop help <command> [action]`
浏览命令索引。在终端直接运行 `ctxhop` 打开交互式工作台；重定向输入/输出时显示
命令索引。输入关键词过滤，用方向键移动，按 Enter 执行。标有 `[--json]` 的命令
支持机器可读输出。

`<HUB>`、`<PROJECT_ID>`、`<SESSION_ID>`、`<REPLICA_ID>`、`<CONTRIBUTION_ID>`
和 `<NATIVE_ID>` 表示需要由你提供的选择值。

### 初始化与帮助

| 命令 | 说明 |
|---|---|
| `ctxhop` | 打开交互式工作台。 |
| `ctxhop init [options]` | 配置存储、加密、设备身份和 Agent Hooks。 |
| `ctxhop install [--dir DIR] [--no-path]` | 安装 CtxHop 命令。 |
| `ctxhop update` | 检查并安装最新版本。 |
| `ctxhop uninstall [--dir DIR]` | 移除本地 CtxHop 安装。 |
| `ctxhop help [<command> [action]]` | 查看命令索引或命令参数。 |
| `ctxhop version` | 查看已安装的版本。 |

### 项目与同步

| 命令 | 说明 |
|---|---|
| `ctxhop project bind [--path DIR] [--identity ID or --name NAME] [--hub HUB]` | 将项目绑定到稳定身份和 Hub。 |
| `ctxhop project unbind [--path DIR or --identity ID]` | 移除项目绑定。 |
| `ctxhop project mode <MODE> [--path DIR or --identity ID]` | 设置项目同步模式：`normal`、`push-only` 或 `excluded`。 |
| `ctxhop project list [--hub HUB] [--json]` | 列出项目绑定。 |
| `ctxhop project discover [--json]` | 发现已授权设备上的项目。 |
| `ctxhop project move <PROJECT_ID> --to <HUB> [--json]` | 将项目移动到另一个 Hub。 |
| `ctxhop push [--workspace] [--git-stash STASH] [SESSION_ID]` | 推送项目 Session 和所选环境；`--workspace` 包含工作区与 Git 状态。 |
| `ctxhop pull [--json]` | 读取远端元数据。 |
| `ctxhop list [--json]` | 列出当前项目的 Session。 |
| `ctxhop resume [SESSION_ID] [options]` | 恢复 Session 和所选环境。 |
| `ctxhop watch [--interval DURATION] [--once] [--json]` | 监视本地 Agent Session 并推送更新。 |

`ctxhop list` 或 `ctxhop resume` 未提供选择值时打开 Session 选择器；使用
`ctxhop session list` 查看逻辑 Session、Agent 来源和 Replica。

### Hub 与逻辑 Session

Session Hub 将 Agent 原生 Session 组织为逻辑 Session，并保留来源关系。同 Agent
续接使用 `session resume`；跨 Agent 续接使用 `switch`。

| 命令 | 说明 |
|---|---|
| `ctxhop hub create [--json] <HUB>` | 创建并发布 Hub。 |
| `ctxhop hub list [--json]` | 列出 Hub 和当前 Hub。 |
| `ctxhop hub use [--json] <HUB>` | 选择当前 Hub。 |
| `ctxhop session discover [--json]` | 发现本机 Agent Session 及其 Hub 关联。 |
| `ctxhop session list [--json]` | 列出逻辑 Session、Agent 来源和 Replica。 |
| `ctxhop session show <SESSION_ID> [--json]` | 查看逻辑 Session 及其来源元数据。 |
| `ctxhop session resume <SESSION_ID> [options]` | 在当前设备恢复原生 Replica。 |
| `ctxhop session switch <SESSION_ID> [options]` | 预览或根据所选上下文创建目标原生 Session。 |
| `ctxhop session attach <SESSION_ID> [options] [--json]` | 将已有原生 Session 接入逻辑 Session。 |
| `ctxhop session reconcile [options] [--json]` | 对比本机原生 Session 与 Hub 绑定状态。 |
| `ctxhop session migrate [--json] [--preview] [--publish-v2] [--rollback] [SESSION_ID]` | 将旧格式 Session 元数据迁移到逻辑 Session 视图。 |

Switch 参数：

| 参数 | 说明 |
|---|---|
| `--to AGENT` | 选择目标 Agent。 |
| `--context causal-head`、`all-heads` 或 `agent-only` | 选择上下文策略。 |
| `--head CONTRIBUTION_ID` | 选择 causal head，可重复指定多个。 |
| `--source AGENT` | 为 `agent-only` 选择来源 Agent。 |
| `--preview` | 预览转换计划，不修改本地状态；省略后直接执行切换。 |
| `--with-environment` | 在切换时包含可移植的环境组件。 |
| `--launch` | 切换完成后启动目标 Agent。 |
| `--allow-unsupported` | 在预览报告中包含不支持的记录。 |

迁移命令：

```bash
ctxhop session migrate --preview
ctxhop session migrate <SESSION_ID> --publish-v2
ctxhop session migrate <SESSION_ID> --rollback
```

省略 `--preview` 时立即执行迁移；`--publish-v2` 将所选旧分支发布为 Replica，`--rollback`
选择旧格式读取器。

### 设备与安全

| 命令 | 说明 |
|---|---|
| `ctxhop device invite [--output PATH]` | 创建设备邀请。 |
| `ctxhop device status [--json]` | 查看本地设备身份和模式。 |
| `ctxhop device mode <MODE>` | 设置设备模式。 |
| `ctxhop device list [--json]` | 列出已授权设备。 |
| `ctxhop device rename <NAME>` | 修改本地设备名称。 |
| `ctxhop device remove <DEVICE_ID>` | 撤销设备。 |
| `ctxhop device rotate-key` | 轮换加密密钥。 |
| `ctxhop passphrase change` | 修改加密密码。 |
| `ctxhop passphrase reset` | 使用 Recovery Key 重置加密密码。 |
| `ctxhop hook install [--agent all, claude-code, or codex]` | 为指定 Agent 安装 SessionEnd Hook。 |

### 历史与远端数据

| 命令 | 说明 |
|---|---|
| `ctxhop history [--json] <SESSION_ID>` | 列出 Session 版本。 |
| `ctxhop history cleanup [--remote-id] [--path DIR] <SESSION_ID>` | 删除 Session 的全部远端版本。 |
| `ctxhop history prune [--remote-id] [--path DIR] (--keep N or --before RFC3339) <SESSION_ID>` | 保留最新版本，或删除指定时间前的版本。 |
| `ctxhop remote delete-session [--remote-id] [--path DIR] <SESSION_ID>` | 删除远端 Session。 |
| `ctxhop remote delete-project [--path DIR]` | 删除当前项目的远端数据。 |
| `ctxhop remote delete-all` | 删除当前同步域的远端数据。 |

使用 `--remote-id` 指定远端 ID。

### 状态与维护

| 命令 | 说明 |
|---|---|
| `ctxhop status [--remote] [--json]` | 查看同步状态；`--remote` 包含远端状态。 |
| `ctxhop doctor [--json]` | 检查配置、后端、Agent、项目和 Hook 状态。 |
| `ctxhop stats [--json]` | 查看跨设备恢复统计。 |

## 配置

CtxHop 使用以下目录保存本地配置、设备密钥和同步状态：

| 系统 | 默认目录 |
|---|---|
| Windows | `%USERPROFILE%\.ctxhop` |
| macOS / Linux | `~/.ctxhop` |

可通过 `CTXHOP_CONFIG_DIR` 指定其他配置目录：

```bash
export CTXHOP_CONFIG_DIR="$HOME/.ctxhop-custom"
```

PowerShell：

```powershell
$env:CTXHOP_CONFIG_DIR = Join-Path $env:USERPROFILE '.ctxhop-custom'
```

该目录包含本地配置和设备密钥，请勿提交到仓库或公开分享。

## 开发

需要 Go 1.26 或更高版本。

克隆并构建：

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

基础检查：

```bash
go test ./...
go vet ./...
```

提交前检查：

```bash
go test -race ./...
```

构建全部支持的平台：

```bash
bash scripts/build.sh
```

Windows PowerShell：

```powershell
.\scripts\build.ps1
```

请勿将真实 Session 文件、Token 或后端凭据提交到仓库。

## 许可证

CtxHop 使用 [MIT License](LICENSE)。
