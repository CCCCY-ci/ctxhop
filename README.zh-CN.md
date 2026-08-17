# AgentSync

[English](README.md) | 简体中文

AgentSync 是一个命令行工具，用于在不同电脑之间同步 Claude Code 会话历史。
它会在本地加密会话数据，把加密后的数据保存到本地目录或 S3 兼容对象存储，
并允许在另一台设备上选择会话进行恢复。

AgentSync 只同步会话记录，不同步开发环境。它不会复制项目文件、未提交的
修改、Claude Code 配置、skills、MCP 服务、凭据或环境变量。目标设备需要提前
安装 Claude Code，并准备好对应项目的 checkout。

当前状态：pre-alpha。当前实现包含目录和 S3 存储、项目绑定、设备配对、密钥轮换
以及恢复安全检查。

## 快速开始

下面使用 Cloudflare R2 作为共享存储，演示设备 A 上传会话、设备 B 查看并恢复会话。

### 开始前准备

- 两台设备都已安装 Claude Code；
- 两台设备都有对应项目的 checkout；
- 有一个 R2 bucket 和一个 R2 S3 API Token；
- Token 可以列出对象，并且可以写入、读取和删除对象。init 会使用临时对象
  进行存储探测。

R2 使用账号级 endpoint，bucket 单独传入：

~~~text
Endpoint: https://<ACCOUNT_ID>.r2.cloudflarestorage.com
Bucket:   <BUCKET_NAME>
Region:   auto
Prefix:   agentsync/demo
~~~

通常不需要 Windows 本机管理员权限。

### 1. 安装 AgentSync

如果已经有二进制文件，可以跳过构建。从源码构建：

~~~bash
git clone https://github.com/CCCCY-ci/agentsync.git
cd agentsync
go build -trimpath -o agentsync ./cmd/agentsync
~~~

将二进制文件注册成用户级命令：

~~~bash
./agentsync install
~~~

Windows PowerShell：

~~~powershell
go build -trimpath -o agentsync.exe ./cmd/agentsync
.\agentsync.exe install
~~~

命令会把二进制文件安装到用户目录。Windows 会在不需要管理员权限的情况下，
把该目录加入用户级 PATH。Unix 如果 `~/.local/bin` 不在 PATH 中，`install`
会打印需要执行的 PATH 命令。重新打开终端后执行：

~~~bash
agentsync version
~~~

使用 `--dir DIR` 可以指定其他安装目录；使用 `--no-path` 只复制二进制文件，
不修改 PATH。

### 2. 初始化设备 A

在设备 A 上执行，尖括号里的内容替换成自己的值：

~~~bash
./agentsync init --backend s3 \
  --endpoint "https://<ACCOUNT_ID>.r2.cloudflarestorage.com" \
  --bucket "<BUCKET_NAME>" \
  --region "auto" \
  --prefix "agentsync/demo" \
  --device-name "device-a"
~~~

init 会提示输入 R2 access key、secret key、可选的 session token，以及加密密码。
输入密钥时不会回显。普通 R2 API Token 没有 session token，直接回车即可。

第一台设备会打印 Recovery Key。请离线保存，并在提示时输入
`saved` 确认。加密密码和 Recovery Key 都丢失后，加密数据无法恢复。

如果检测到 Claude Code，init 会询问是否安装 SessionEnd Hook。输入
`y` 可以在会话结束后自动 push，直接回车则跳过。不需要交互时可以使用
`--no-hook`。

### 3. 绑定项目并上传

在设备 A 上执行：

~~~bash
cd /path/to/project
./agentsync project bind --path .
./agentsync push
~~~

没有可用 Git 身份的项目使用手动名称：

~~~bash
./agentsync project bind --name "my-project" --path .
./agentsync push
~~~

设备 B 使用同一个手动名称。绑定只保存本地关系，push 只上传当前项目的会话。
要同步其他项目，进入对应目录后单独执行绑定。

### 4. 配对设备 B

在设备 A 上创建 invitation：

~~~bash
./agentsync device invite --output agentsync-device-b.json
~~~

通过可信渠道把 JSON 文件传给设备 B。文件包含远端配置和签名的同步域证明，
不包含 R2 凭据、加密密码或会话内容。

在设备 B 上初始化：

~~~bash
./agentsync init --invite ./agentsync-device-b.json --device-name "device-b"
~~~

按提示输入 R2 凭据，并使用与设备 A 相同的加密密码。不要把
`--invite` 与 `--endpoint`、`--bucket`、`--region`、`--prefix` 等
后端参数一起使用。

### 5. 在设备 B 查看并恢复

先在设备 B 准备好相同项目并完成绑定：

~~~bash
cd /path/to/the/same/project
./agentsync project bind --path .
./agentsync pull --check
./agentsync list
~~~

使用 `list` 打印出的会话 ID 进行恢复：

~~~bash
./agentsync resume <NATIVE_SESSION_ID>
claude --resume
~~~

`pull --check` 只读取远端元数据。`resume` 才会下载选中的加密会话并恢复到
Claude Code，不会复制项目文件、Git 修改、skills、MCP 服务或凭据。

默认恢复会检查目标工作区。如果工作区不同，先查看差异，再决定是否使用
`--allow-divergent`。

### 6. 添加其他项目

AgentSync 不会自动扫描所有目录。要同步哪个项目，就进入该项目并单独绑定：

~~~bash
cd /path/to/another/project
./agentsync project bind --path .
./agentsync push
~~~

没有 Git 的项目在两台设备上使用相同的 `--name`。

## CLI 命令

下面是一张当前支持的完整命令表。除非命令提供 path 参数，会话相关命令都针对
当前项目。删除类命令默认会要求确认，传入 `--yes` 才会跳过确认。

| 命令 | 说明 |
|---|---|
| `agentsync help` | 显示命令用法。 |
| `agentsync version` | 显示版本、commit、构建时间和运行时信息。 |
| `agentsync completion bash, zsh, fish, powershell, pwsh` | 生成 Shell 补全；`pwsh` 是 `powershell` 的别名。 |
| `agentsync init [backend options]` | 创建或加入加密同步域并写入本地配置，可选安装 Claude Code Hook。 |
| `agentsync install [--dir DIR] [--no-path]` | 把当前二进制安装到用户级命令目录；Windows 会更新用户级 PATH。 |
| `agentsync status [--json] [--remote]` | 显示本地状态；`--remote` 会检查远端元数据。 |
| `agentsync doctor [--json]` | 检查配置、后端访问、Agent 安装、项目身份和最近的本地错误。 |
| `agentsync project bind [--path DIR] [--name NAME or --identity ID]` | 绑定本地项目；没有 Git 时使用 `--name`。 |
| `agentsync project unbind [--path DIR or --identity ID]` | 删除本地项目绑定。 |
| `agentsync project mode normal / push-only / excluded [--path DIR or --identity ID]` | 设置项目同步策略。 |
| `agentsync project list [--json]` | 列出已绑定项目及其策略。 |
| `agentsync push [SESSION_ID] [--session SESSION_ID] [--agentsync-hook]` | 上传当前项目的新记录。 |
| `agentsync watch [--interval DURATION] [--once] [--json]` | 持续扫描并上传当前项目；`--once` 只执行一次。 |
| `agentsync pull --check [--json]` | 检查加密远端元数据，不下载会话正文。 |
| `agentsync list [--json]` | 列出当前项目可用的会话。 |
| `agentsync resume [SESSION_ID] [restore options]` | 恢复一个会话；选项包括 `--version`、`--allow-limited`、`--allow-divergent`、`--no-workspace-context` 和 `--replace-existing`。 |
| `agentsync history SESSION_ID [--json]` | 查看会话可恢复版本和 fork。 |
| `agentsync history cleanup SESSION_ID [cleanup options]` | 删除一个会话，是 `remote delete-session` 的别名。 |
| `agentsync history prune SESSION_ID --keep N or --before RFC3339` | 按一种保留规则删除旧版本。 |
| `agentsync stats [--json]` | 显示本地跨设备恢复统计。 |
| `agentsync device status [--json]` | 显示本机设备模式。 |
| `agentsync device mode normal / push-only / disabled` | 修改本机设备模式。 |
| `agentsync device list [--json]` | 列出同步域中已授权的设备。 |
| `agentsync device rename NAME` | 修改本机显示名称。 |
| `agentsync device invite [--output FILE]` | 创建给另一台设备使用的签名 invitation。 |
| `agentsync device rotate-key` | 保存新的 Recovery Key 后发布新一代加密密钥。 |
| `agentsync device remove DEVICE_ID [--yes]` | 撤销设备对未来 generation 的访问，并删除它的远端分支。 |
| `agentsync passphrase change` | 使用当前密码修改加密密码。 |
| `agentsync passphrase reset` | 使用已有 Recovery Key 重置加密密码。 |
| `agentsync remote delete-session SESSION_ID [--remote-id] [--yes]` | 删除一个远端会话。 |
| `agentsync remote delete-project [--path DIR] [--yes]` | 删除一个项目的全部远端会话。 |
| `agentsync remote delete-all [--yes]` | 删除配置同步域命名空间下的全部对象。 |

执行 `agentsync <command> --help` 查看具体参数。需要自动化时，在支持的命令上
使用 `--json`。

## 配置

### 配置目录

没有设置 `AGENTSYNC_CONFIG_DIR` 时，AgentSync 使用：

| 平台 | 默认目录 |
|---|---|
| Windows | `%APPDATA%\\agentsync` |
| macOS | `~/Library/Application Support/agentsync` |
| Linux 和其他 Unix 系统 | `$XDG_CONFIG_HOME/agentsync`，否则使用 `~/.config/agentsync` |

初始化成功后，init 会打印实际目录。要使用当前用户 home 下的
`.agentsync`：

~~~bash
export AGENTSYNC_CONFIG_DIR="$HOME/.agentsync"
~~~

PowerShell：

~~~powershell
$env:AGENTSYNC_CONFIG_DIR = Join-Path $env:USERPROFILE '.agentsync'
~~~

该目录包含配置、加密 secrets、设备密钥和本地状态。不要提交或公开这个目录。
Claude Code 的数据目录与 AgentSync 分开，可以通过 `CLAUDE_CONFIG_DIR` 指定。

在 CI 或短期测试中，可以设置 `AGENTSYNC_ACCESS_KEY_ID` 和
`AGENTSYNC_SECRET_ACCESS_KEY`；`AGENTSYNC_SESSION_TOKEN` 可选。环境变量中的
凭据不会写入磁盘。

## 限制与安全

- 会话数据和元数据在上传前加密。
- 每台设备都有独立的 device ID 和远端分支。
- `push` 写入当前设备的分支，不会把该分支再拉回本机。
- `pull --check` 只读取元数据；`resume` 才是显式下载正文并恢复的操作。
- 不同步项目文件、未提交的 Git 修改、分支、skills、MCP 服务、插件、凭据和任意
  环境状态。
- 目标设备必须提前准备好 Claude Code 和项目。
- Git 项目有更强的工作区检查；没有 Git 的项目使用 touched 文件回退检查。
- 没有 Claude Code 的服务器可以保存数据并执行管理检查，但不能上传或原生恢复
  Claude 会话。
- 加密密码和 Recovery Key 同时丢失后，加密数据无法恢复。

### 常见初始化问题

- **backend probe failed：** 使用账号级 R2 endpoint，bucket 单独传入，region 设置为
  `auto`，并检查对象列出、读取、写入和删除权限。
- **passphrase does not unlock storage：** 使用这个同步域原来的加密密码。
- **already configured：** 使用现有配置，或设置新的 `AGENTSYNC_CONFIG_DIR`；init
  不会覆盖有效配置。
- **设备 B 没有会话：** 两台设备绑定相同的项目身份，并确认设备 A 已经完成
  `push`。

## 开发

~~~bash
go test ./...
go build -trimpath -o agentsync ./cmd/agentsync
~~~

## 许可证

AgentSync 使用 [Apache License 2.0](LICENSE)。重新分发时请保留 [NOTICE](NOTICE) 文件。
