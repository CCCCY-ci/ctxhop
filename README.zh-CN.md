# AgentSync

简体中文 | [English](README.md)

AgentSync 是一个本地 CLI 工具，用来在不同设备或不同安装之间继续 Claude
Code 会话，同时避免复制 Claude 凭据，也不直接同步 Claude 正在使用的实时
数据目录。

> **当前状态：pre-alpha。** 加密同步/恢复链路、本地目录和 S3 兼容 Remote、
> 项目策略、设备模式、工作区安全检查、历史维护和 Shell 补全已经实现。
> 2026-08-17 已使用 Cloudflare R2 这个 S3 兼容 provider 完成一次真实单设备
> 验收。真实跨操作系统、第三方 Remote 和实时 Agent 会话验收仍属于后续矩阵。

## 功能概览

AgentSync 在用户自己的设备上运行，主要流程是：

1. 发现 Claude Code 的会话 JSONL 文件；
2. 规范化其中与机器相关的路径；
3. 在写入 Remote 之前加密会话记录和元数据；
4. 在本地维护追加上传游标和重试队列；
5. 按需检查远端元数据；
6. 把选中的一个会话恢复到 Claude Code 原生会话目录。

Remote 可以是本地目录或 S3 兼容对象存储，包括 Cloudflare R2。AgentSync
没有独立服务端、账号系统或遥测收集器，唯一的网络目标就是用户配置的 Remote。

每台设备只写入自己的不透明远端分支。上传时不会再次列举或下载本设备的
分支；正常上传只读取小型 keyfile/身份信息，用于确认同步域没有被替换。
元数据检查和读取会话正文的恢复流程都是显式操作。

## 重要边界

AgentSync 同步的是会话历史，不是整台开发机器。

当前不会同步：

- Claude/API 凭据、登录态或其他秘密；
- 整个 Claude 数据目录、缓存或正在运行的数据库；
- skill、MCP server、插件、环境变量或其他 Agent 安装环境；
- 项目文件、未提交 Git 修改、分支、worktree 或构建产物。

目标设备必须提前准备好相应的 Agent、依赖、skill/MCP 配置和项目 checkout。
工作区 fingerprint 只是安全检查，不是文件同步机制。

push 和 watch 每次只处理当前项目，不会扫描机器上的全部项目。项目可以有
多个，但每个项目都需要稳定身份，并且可以单独设置本地策略。

对象存储服务器也不是 AgentSync 设备。S3 兼容存储只是传输 Remote。没有安装
Claude Code 的无头服务器可以保存对象或执行部分管理检查，但不能独立提供
原生 Claude Code 会话恢复体验。

## 运行要求

- 从源码构建需要 Go 1.26 或更高版本；
- 需要在发现或恢复 Claude 会话的设备上安装 Claude Code；
- 推荐项目使用 Git，以获得稳定项目身份和更完整的工作区检查；
  没有 Git 的项目可以使用手动身份，但有下文说明的限制；
- 使用 S3 兼容 Remote 时，需要 endpoint、bucket、凭据，以及对目标
  bucket/prefix 的相应权限。

当前代码可以构建 Windows、macOS 和 Linux。真实跨系统路径转换和原生 Agent
验收属于额外外部矩阵，目前没有宣称完成。

## 从源码安装

~~~bash
git clone https://github.com/CCCCY-ci/agentsync.git
cd agentsync

# macOS/Linux
go build -trimpath -o agentsync ./cmd/agentsync
./agentsync version

# Windows PowerShell
go build -trimpath -o agentsync.exe ./cmd/agentsync
.\agentsync.exe version
~~~

没有注入发布元数据时，开发构建会显示 dev。当项目发布正式 tag 和二进制后，
也可以使用 Go 工具安装：

~~~bash
go install github.com/CCCCY-ci/agentsync/cmd/agentsync@<VERSION>
agentsync version
~~~

运行 agentsync help 查看顶层命令。init 会拒绝覆盖已经存在的有效配置；
需要隔离测试时，请使用不同的 AGENTSYNC_CONFIG_DIR。

## 配置目录与本地文件

默认配置目录：

| 平台 | 目录 |
|---|---|
| Windows | %APPDATA%\agentsync |
| macOS | ~/Library/Application Support/agentsync |
| Linux/Unix | $XDG_CONFIG_HOME/agentsync，否则使用 ~/.config/agentsync |

适合测试、Hook 环境和多套隔离配置时，也可以显式指定：

~~~bash
AGENTSYNC_CONFIG_DIR=/path/to/agentsync-config agentsync status
~~~

PowerShell：

~~~powershell
$env:AGENTSYNC_CONFIG_DIR = 'D:\path\to\agentsync-config'
.\agentsync.exe status
~~~

主要文件：

| 路径 | 作用 |
|---|---|
| config.json | backend、endpoint/bucket/prefix、设备状态、项目绑定和策略 |
| secrets | 加密保存的 Remote 凭据、标识密钥和本地设备授权 |
| device.key | 解开加密 secrets 的本地设备密钥 |
| state/ | 上传游标、重试队列、拉取观察状态、恢复统计和诊断信息 |

不要把这个目录提交到 Git。config.json 不包含 Remote 凭据或密码，但可能包含
endpoint、bucket 名称和本机项目绝对路径。秘密输入都不会回显。

Claude Code 数据目录与 AgentSync 配置目录分开。测试或使用非默认 Claude 数据
目录时，可以设置 CLAUDE_CONFIG_DIR：

~~~bash
CLAUDE_CONFIG_DIR=/path/to/claude-data agentsync list
~~~

隔离 CI 测试可以使用下列环境变量提供临时 S3 兼容凭据，变量本身不会写入磁盘：

~~~text
AGENTSYNC_ACCESS_KEY_ID
AGENTSYNC_SECRET_ACCESS_KEY
AGENTSYNC_SESSION_TOKEN
~~~
Access Key 和 Secret Key 要成对提供。普通安装优先使用加密 secrets 文件；
外部测试优先使用短期凭据。

## 快速开始：本地目录 Remote

本地目录 Remote 是验证单设备最简单的方式，也适合配合已有且可信的目录
同步工具使用。

初始化第一台设备：

~~~bash
agentsync init --backend dir --path /path/to/agentsync-store --device-name laptop-a --no-hook
~~~

Windows PowerShell：

~~~powershell
.\agentsync.exe init --backend dir --path D:\data\agentsync-store --device-name laptop-a --no-hook
~~~

init 会提示输入 Encryption password，两次输入都不会回显。第一次创建同步域
远端 keyfile 时还会打印 Recovery Key。请在继续前把它离线保存好。密码和
Recovery Key 同时丢失时，已经加密的数据无法恢复。

如果检测到 Claude Code 且没有使用 --no-hook，init 会询问是否注册 SessionEnd
Hook。Hook 会在会话结束时调用 agentsync push。Hook 不是必需的，手工 push
和 watch 也可以正常工作。

Git 项目有稳定 remote 时：

~~~bash
cd /path/to/project
agentsync push
~~~

没有可用 Git 身份时：

~~~bash
agentsync project bind --name client-project --path /path/to/project
cd /path/to/project
agentsync push
~~~

代表同一个逻辑 no-Git 项目的每台设备都必须使用相同的手动名称。绑定只是
本地配置，不会自动上传。

查看和恢复：

~~~bash
agentsync list
agentsync status
agentsync pull --check
agentsync resume <SESSION_ID>
claude --resume
~~~

pull --check 只读取元数据，不下载加密 shard 正文、不写 Claude 文件、不恢复
会话，也不会推进本地拉取观察标记。resume 才是明确读取正文并写入原生会话
目录的操作。

## S3 兼容存储（以 Cloudflare R2 为例）

初始化任意 S3 兼容 Remote：

~~~bash
agentsync init --backend s3 --endpoint https://s3.example.com --bucket my-agent-sync --region us-east-1 --prefix agentsync
~~~

Cloudflare R2 使用账号级 S3 endpoint，签名 region 通常使用 auto：

~~~bash
agentsync init --backend s3 --endpoint https://<ACCOUNT_ID>.r2.cloudflarestorage.com --bucket <BUCKET_NAME> --region auto --prefix agentsync
~~~

R2 是 S3 兼容 provider，不是单独的 AgentSync backend。它的 endpoint 是账号
级地址。不要把 bucket 名称拼到 endpoint 后面，bucket 要通过 --bucket 单独传入。
下面这种写法错误：

~~~text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com/<BUCKET_NAME>
~~~

endpoint、bucket、prefix 会写入 config.json。Access Key、Secret Key 和可选
Session Token 通过隐藏输入读取，并在没有环境变量覆盖时保存到加密 secrets。
不要把它们放进 Shell 历史或命令行参数。

初始化探测会写入、读取并删除一个临时对象，因此凭据至少需要对目标 bucket/
prefix 具备对象 Put/Get/Delete 权限。后续 list、pull --check、resume 和清理
操作还需要对应的列举/读取/删除权限。外部测试请使用专用 bucket 或 prefix 和
短期凭据。

默认使用 virtual-hosted addressing。某些 S3 gateway 要求 bucket 出现在 URL
路径中，这时加 --path-style：

~~~bash
agentsync init --backend s3 --endpoint https://s3.example.com --bucket my-agent-sync --region us-east-1 --path-style
~~~

初始化完成后，显式开启的集成测试：

~~~bash
# macOS/Linux
AGENTSYNC_CONFIG_DIR=/path/to/agentsync-config AGENTSYNC_S3_INTEGRATION=1 go test ./internal/remote -run '^TestS3Integration$' -count=1 -v

# Windows PowerShell
$env:AGENTSYNC_CONFIG_DIR = 'D:\path\to\agentsync-config'
$env:AGENTSYNC_S3_INTEGRATION = '1'
go test .\internal\remote -run '^TestS3Integration$' -count=1 -v
~~~

该测试默认关闭，绝不能指向生产 bucket。旧版 AGENTSYNC_S3_* 变量仍可作为
隔离 CI 的 fallback。

## 使用第二台设备

第二个安装通过签名 invitation 加入同一个同步域，会获得新的不透明设备 ID，并
写入独立的远端设备分支。

设备 A：

~~~bash
agentsync device invite --output agentsync-invite.json
~~~

通过可信渠道把 invitation 文件传给设备 B。文件包含 Remote 设置和非秘密的同步域
fingerprint，但不包含加密密码、Remote 凭据或会话内容。

设备 B：

~~~bash
agentsync init --invite agentsync-invite.json --device-name laptop-b
~~~
如果设备 B 本地还没有 S3 兼容 Remote 凭据，按提示输入。--invite 会携带 Remote
设置，不要再与 --backend、--path、--endpoint、--bucket、--region、--prefix 或
--path-style 同时使用。

可额外校验同步域：

~~~bash
agentsync init --invite agentsync-invite.json --device-name laptop-b --expect-domain-fingerprint <FINGERPRINT>
~~~

设备 B 还需要绑定相同的项目身份。没有 Git 时重复相同的手动绑定：

~~~bash
agentsync project bind --name client-project --path /path/to/project
~~~

查看成员：

~~~bash
agentsync device list
agentsync device status
~~~

Invitation 是配对文件，不是服务端账号。Remote namespace 和 keyfile 定义
同步域；每个安装拥有独立的设备身份和授权。

## 日常上传与恢复流程

~~~text
Claude Code 写入会话
        |
        | SessionEnd Hook 或手工 push
        v
agentsync push
        |
        | 加密的设备分支 shard + 加密元数据
        v
配置的目录或 S3 兼容 Remote
        |
        | 显式元数据检查
        v
agentsync pull --check
        |
        | 显式下载正文并执行安全检查
        v
agentsync resume [SESSION_ID]
        |
        v
Claude Code 原生会话目录
~~~

目标设备：

~~~bash
cd /path/to/the/same/project
agentsync pull --check
agentsync list
agentsync resume <SESSION_ID>
claude --resume
~~~

push 只处理当前项目，不会把本设备自己的分支再拉回来。watch 发现 session
snapshot 没有变化时会跳过下一次重复上传。重复执行 push 也是安全的：没有新
的会话前缀时，本地游标不会创建新的逻辑 shard。

存在多个 fork 时：

~~~bash
agentsync history <SESSION_ID>
agentsync resume <SESSION_ID> --version 1
~~~

除非明确要替换本地会话，否则不要使用 --replace-existing；默认会在覆盖前停止。

## 项目与同步策略

项目选择是显式的：当前工作目录是默认项目根目录，push、watch、list、pull、
resume、history 和远端生命周期命令都针对当前项目，不会自动扫描所有项目。

查看绑定和策略：

~~~bash
agentsync project list
agentsync project list --json
~~~

绑定 Git 或手动身份：

~~~bash
agentsync project bind --path .
agentsync project bind --identity https://github.com/example/client.git --path .
agentsync project bind --name client-project --path .
~~~

--identity 与 --name 不能同时使用。--name 会创建 manual:name 身份。重复绑定
同一个 root/identity 对是幂等的。

设置项目策略：

~~~bash
agentsync project mode normal --path .
agentsync project mode push-only --path .
agentsync project mode excluded --path .
~~~

| 项目模式 | 上传 | 远端列举/检查/恢复 |
|---|---:|---:|
| normal | 允许 | 允许 |
| push-only | 允许 | 阻止 |
| excluded | 跳过/阻止 | 阻止 |

也可以用 --identity 指定项目。normal 会从两个限制策略列表中移除该身份。

## 设备模式与设备管理

设备 ID 与显示名称、主机名、路径和模式相互独立。重命名不会产生新的远端分支。

~~~bash
agentsync device status
agentsync device mode normal
agentsync device mode push-only
agentsync device mode disabled
~~~

| 设备模式 | 上传 | 元数据/list/pull/resume |
|---|---:|---:|
| normal | 允许 | 显式操作允许 |
| push-only | 允许 | 阻止 |
| disabled | 跳过 | 阻止 |

管理设备：

~~~bash
agentsync device list
agentsync device list --json
agentsync device rename workstation
agentsync device remove <DEVICE_ID>
~~~

除非传入 --yes，device remove 会要求确认。它会轮换内容密钥 generation，
撤销目标设备对后续 generation 的访问，并删除目标设备的远端分支对象。已经被
目标设备复制的明文或历史密钥无法通过远端撤回。

## 密码、Recovery Key 与密钥轮换

密码不能通过命令行参数传入，交互式秘密输入会隐藏。

~~~bash
agentsync passphrase change
agentsync passphrase reset
agentsync device rotate-key
~~~

passphrase change 需要当前密码。passphrase reset 使用已有 Recovery Key，且
不会生成新的 Recovery Key。device rotate-key 会要求输入当前/新密码，打印新
Recovery Key，并要求明确确认已经保存后才发布新 generation。请先保存再确认。

密码和 Recovery Key 同时丢失时，加密同步域无法恢复。
## 历史与远端清理

查看 fork 并保留旧版本：

~~~bash
agentsync history <SESSION_ID>
agentsync history <SESSION_ID> --json
agentsync history prune <SESSION_ID> --keep 3
agentsync history prune <SESSION_ID> --before 2026-08-15T00:00:00Z
~~~

prune 必须二选一指定规则。更新时间未知的版本会保守保留。只有确认目标和
规则无误后才使用 --yes。

history cleanup 是删除一个会话的快捷方式：

~~~bash
agentsync history cleanup <SESSION_ID>
agentsync remote delete-session <SESSION_ID>
agentsync remote delete-session --remote-id <OPAQUE_REMOTE_ID>
agentsync remote delete-project --path .
agentsync remote delete-all
~~~

删除默认要求确认。remote delete-all 会删除配置同步域 namespace 下的全部
远端对象；使用 --yes 前必须确认 backend、bucket、prefix 和配置目录。

恢复统计只保存在本地：

~~~bash
agentsync stats
agentsync stats --json
~~~

## 工作区安全检查

恢复前，AgentSync 会比较目标工作区与加密会话元数据中的 source fingerprint。

Git 项目的 fingerprint 可以包含 repository HEAD/branch 状态、tracked/
untracked dirty 状态，以及会话触碰文件的内容摘要。

手动绑定的 no-Git 项目使用 L3 fallback，只哈希会话报告为 touched 的文件。
没有被会话报告的文件变化可能无法检测，因此 Git 项目的安全覆盖更强。

默认情况下，工作区不一致会阻止恢复：

~~~bash
agentsync resume <SESSION_ID> --allow-divergent
~~~

接受差异后，AgentSync 默认向恢复后的会话写入一条 local-only 说明，让 Agent
重新读取受影响文件。可以只关闭说明，不会关闭安全门：

~~~bash
agentsync resume <SESSION_ID> --allow-divergent --no-workspace-context
~~~

其他显式开关：

~~~bash
agentsync resume <SESSION_ID> --allow-limited
agentsync resume <SESSION_ID> --replace-existing
~~~

allow-limited 允许恢复未经验证的 Agent 格式/版本；version 选择从零开始的
fork；replace-existing 允许覆盖本地已有会话。

## 命令参考

~~~text
agentsync help
agentsync version
agentsync init [--backend dir|s3] [backend options] [--device-name NAME]
               [--device-mode normal|push-only|disabled] [--invite FILE]
agentsync status [--json] [--remote]
agentsync list [--json]
agentsync push [SESSION_ID] [--session SESSION_ID] [--agentsync-hook]
agentsync watch [--interval DURATION] [--once] [--json]
agentsync pull --check [--json]
agentsync resume [SESSION_ID] [--json] [--version N]
                  [--allow-limited] [--allow-divergent]
                  [--no-workspace-context] [--replace-existing]
agentsync history SESSION_ID [--json]
agentsync history cleanup SESSION_ID [--yes] [--remote-id] [--path DIR]
agentsync history prune SESSION_ID [--keep N | --before RFC3339]
                    [--yes] [--remote-id] [--path DIR]
agentsync stats [--json]
agentsync doctor [--json]
agentsync passphrase change
agentsync passphrase reset
agentsync device status [--json]
agentsync device mode normal|push-only|disabled
agentsync device list [--json]
agentsync device rename NAME
agentsync device invite [--output FILE]
agentsync device rotate-key
agentsync device remove DEVICE_ID [--yes]
agentsync project bind [--path DIR] [--name NAME | --identity ID]
agentsync project unbind [--path DIR | --identity ID]
agentsync project mode normal|push-only|excluded [--path DIR | --identity ID]
agentsync project list [--json]
agentsync remote delete-session SESSION_ID [--yes] [--remote-id] [--path DIR]
agentsync remote delete-project [--yes] [--path DIR]
agentsync remote delete-all [--yes]
agentsync completion bash|zsh|fish|powershell|pwsh
~~~

需要脚本化时，在支持的命令上使用 --json。秘密输入写入 prompt stream，不会
污染 JSON 标准输出。

## Shell 补全

生成补全脚本不会加载配置，也不会访问 Remote：

~~~bash
source <(agentsync completion bash)
source <(agentsync completion zsh)
agentsync completion fish | source
agentsync completion powershell | Invoke-Expression
~~~

pwsh 可以作为 PowerShell 别名。持久安装时，请按照所使用 Shell 的补全机制完成配置。

## 故障排查

### init: backend probe failed

检查 endpoint 使用 http/https 且不包含 bucket 路径，bucket 是否单独传入，
签名 region 是否正确（R2 使用 auto），凭据是否能对 prefix 执行 Put/Get/Delete，
后续流程是否有列举权限，以及系统时间和网络是否正常。

以 R2 为例，应使用：

~~~text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com
~~~

而不是：

~~~text
https://<ACCOUNT_ID>.r2.cloudflarestorage.com/<BUCKET_NAME>
~~~

### init 提示已经配置

使用 status 或 doctor 检查，或者将 AGENTSYNC_CONFIG_DIR 指向新目录创建隔离
安装。删除旧配置前，确认其中的 Recovery Key 和远端数据不再需要。

### 无法解析项目身份

Git 项目需要稳定 Git remote 或显式 project bind identity。no-Git 项目需要
project bind name 或显式 identity。使用 project list 查看本地绑定。

### 找不到会话

确认 Claude Code 已安装，CLAUDE_CONFIG_DIR 指向正确数据目录，当前目录属于
同一个逻辑项目。push 和 watch 只扫描当前项目。

### 看不到远端更新

在目标项目执行 pull --check。确认设备和项目没有设置成 push-only 或 excluded。
设备不会自动拉取自己的分支；远端正文只能由显式 resume 读取。

### 恢复被阻止

先读错误原因：工作区差异先检查后再用 allow-divergent；兼容性受限时审查版本
后再用 allow-limited；只有明确要替换才用 replace-existing；多个 fork 先查
history 再选择 version。

## 安全模型与限制

- 会话正文、项目元数据和设备元数据在写入 Remote 前已经加密；
- Remote 凭据保存在本地加密 secrets 中，不会写入 config.json，也不会由诊断
  命令打印；
- 每台设备拥有独立 ID 和不重叠的远端分支；
- 签名 invitation、同步域 fingerprint、设备级授权、密钥轮换和撤销保护后续
  generation；
- Remote 对象数量、大小和时间仍可能形成元数据侧信道；
- 拥有 Remote 凭据的主体仍可执行 provider 允许的破坏性操作，应使用
  bucket/prefix ACL、短期凭据和 provider 侧轮换；
- 已经复制到设备上的明文或历史密钥无法通过远端撤回；
- 密码和 Recovery Key 同时丢失时不可恢复；
- 实时 Agent 文件和机器环境不属于当前同步格式。

## 开发与验证

~~~bash
go test ./...
go test -race ./...
go vet ./...
go run ./poc/mvp
~~~

专项 PoC 位于 poc/，包测试覆盖 adapter、crypto、config、project、remote、
syncer 和 syncflow。

构建发布目标：

~~~bash
# macOS/Linux
./scripts/build.sh

# Windows PowerShell
.\scripts\build.ps1
~~~

交叉编译产物写入 dist/。仓库包含发布 workflow、包管理模板和格式/版本规格，
但生产签名和外部包渠道仍需显式配置。

## 许可证

[Apache-2.0](LICENSE)。重新分发修改后的版本时，请保留 NOTICE 并标记修改过的文件。