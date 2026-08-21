# AgentSync

[English](README.md) | 简体中文

AgentSync 是一个命令行工具，用于在不同电脑之间同步 Claude Code 和 Codex CLI 会话历史。
它会在本地加密会话数据，把加密后的数据保存到本地目录或 S3 兼容对象存储，
并允许在另一台设备上选择会话进行恢复。

AgentSync 会同步会话记录，并在 session 中发现结构化的 Agent/工具依赖时，
保存一份很小的加密依赖清单。对于 session 结构化引用到的 Codex skill，如果本地存在
经过过滤的非敏感 `SKILL.md`，也会把它作为独立环境组件上传；对于实际调用到的
Codex MCP server，只保留命令 basename、安全参数和启动超时这类非敏感意图。普通的
`push`、Hook 和 `watch` 不会上传项目文件正文。只有明确执行 `push --include-workspace`
时，Git 项目会根据 session fingerprint 选择文件；完全没有 Git 的项目会扫描安全过滤后的项目目录；凭据、
token、密钥材料、`.env`、`.git` 数据都不会进入快照。
如果 Codex session 的结构化元数据中明确记录了 model、model_provider 或 effort，
AgentSync 只会保存这些白名单设置的项目级摘要。目标设备仍需要提前安装相应的 Agent，
并准备好对应项目的 checkout。

会话正文、环境清单、工作区快照和 Git 传输正文会先规范化，并在压缩确实能减少体积时压缩，
再加密上传。压缩封装带有格式版本、大小和解压比例限制；过小或不可压缩的内容保持原样，
旧的未压缩远端对象仍然可以读取。凭据、token 和密钥材料不会进入这条流水线。

当前状态：pre-alpha。当前实现包含目录和 S3 存储、项目绑定、设备配对、密钥轮换、
Claude Code 和 Codex 适配器、SessionEnd Hook、恢复安全检查，以及只读环境预览、
经过明确确认后按需应用的 Codex Skill 组件和有限工作区快照；MCP 意图和 session 设置
组件仍只做预览。

## 快速开始

下面使用 Cloudflare R2 作为共享存储，演示设备 A 上传会话、设备 B 查看并恢复会话。

### 开始前准备

- 两台设备都已安装 Claude Code 或 Codex CLI；
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

如果检测到 Claude Code 或 Codex CLI，init 会询问是否安装对应的 SessionEnd Hook。输入
`y` 可以在会话结束后自动 push，直接回车则跳过。不需要交互时可以使用
`--no-hook`。

对于 Codex，安装后请重启 Codex，执行 /hooks，并信任 AgentSync Hook。
如果已经执行过 init，不需要重新初始化同步域，可以直接安装：

~~~bash
./agentsync hook install --agent codex
~~~

Claude Code 使用 --agent claude-code；不传 --agent 会配置检测到的所有支持的 Agent。

### 3. 绑定项目并上传

在设备 A 上执行：

~~~bash
cd /path/to/project
./agentsync project bind --path .
./agentsync push

# 可选：明确上传有限工作区快照。Git 项目使用 session fingerprint；无 Git 项目扫描安全过滤后的项目目录。
./agentsync push --include-workspace
~~~

普通 push 还会记录一份很小的加密 Git 状态摘要，包括仓库 HEAD、分支、upstream
和 dirty 路径，但不会上传 Git 对象或项目文件。如果需要明确携带本地 commit 或
tracked/untracked 的未提交修改到另一份 checkout，执行：

~~~bash
./agentsync push --include-git-state
~~~

这个选项会先做敏感内容预检，再生成 Git 原生 bundle。它不会上传整个 .git 目录。
设备 B 需要明确执行应用；commit 会先导入隐藏的 AgentSync ref，当前分支不会自动改变。

如果要传输已有的 stash，而不是当前工作区，先查看 stash 列表，再明确指定引用：

~~~bash
git stash list
./agentsync push --git-stash 'stash@{0}'
~~~

`--git-stash` 会自动启用显式 Git 传输，并用指定 stash 替代传输中的 worktree 部分。
原 stash 只会被读取，不会被应用、修改或删除；当前工作区的修改不会进入这份 worktree bundle。
指定的 stash 仍会执行同样的敏感路径和内容安全检查。

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

已授权设备可以发现其他设备发布的新项目：

~~~bash
./agentsync project discover
~~~

这个命令只列出项目身份，不会自动 clone 仓库，也不会绑定本地目录。先在设备 B
准备好项目 checkout，再完成绑定：

~~~bash
cd /path/to/the/same/project
./agentsync project bind --path .
./agentsync pull --check
./agentsync list
~~~

恢复前可以先查看这个 session 记录到的依赖引用：

~~~bash
./agentsync env preview <NATIVE_SESSION_ID>
~~~

这个命令只读远端加密 metadata，不会安装、应用或执行任何内容。如果上传了安全的
Codex Skill、MCP 意图或 session 设置组件，预览只显示类型、作用域、大小和 fingerprint，
不显示也不应用组件正文。
对于 MCP 意图和白名单 Codex settings，预览会按组件的 global/project 作用域检查相应配置，
并显示 missing、changed 或 unchanged。
不安全或无法读取的值会保持为 unavailable/manual；`env apply` 不会写入这些配置。
预览还会显示本机工具是否存在以及 SessionEnd Hook 状态。版本差异只作提示，适配器仍按
session 实际包含的字段判断兼容性。

`env preview` 用于只读查看本地组件差异。`env apply` 不加 `--yes` 时也只显示差异，不会写入文件：

~~~bash
./agentsync env apply <NATIVE_SESSION_ID>
~~~

确认输出后，明确加上 `--yes` 才会应用过滤后的 Codex Skill 文件；替换已有文件前会先创建备份：

~~~bash
./agentsync env apply --yes <NATIVE_SESSION_ID>
~~~

MCP 和 session 设置组件目前只做预览，不会修改原始配置、安装工具或执行命令。
如果设备 A 使用了 `push --include-workspace`，设备 B 可以先查看并明确应用这个有限工作区快照。
预览不会写入文件；加上 `--yes` 后才会写入可用的文件正文，替换已有文件前会先备份：

~~~bash
./agentsync workspace preview <NATIVE_SESSION_ID>
./agentsync workspace apply <NATIVE_SESSION_ID>
./agentsync workspace apply --yes <NATIVE_SESSION_ID>
~~~

Git 项目快照只包含该 session fingerprint 已选中的文件；无 Git 项目快照来自安全过滤后的目录扫描。不可用、敏感、二进制或超出大小限制的正文
会保留为需要手动处理的项目。无 Git 快照中的本地多余文件会显示为删除候选，但不会自动删除本地文件，也不会切换分支、提交、stash 或执行 Git 命令。

使用 `list` 打印出的会话 ID 进行恢复：

~~~bash
# 目标工作区与源设备一致时使用。
./agentsync resume <NATIVE_SESSION_ID>

# 设备 B 的项目路径或工作区不一致时使用。
./agentsync resume --allow-divergent <NATIVE_SESSION_ID>
~~~

恢复成功后，执行下面的命令即可在 Claude Code 的会话列表中看到同步过来的 session：

~~~bash
claude --resume
~~~

`pull --check` 只读取远端元数据。`env preview` 会显示 session 记录到的结构化依赖和本地
组件差异。只有源设备使用了 `push --include-workspace` 时，`workspace preview` 才会显示
有限工作区快照。`resume` 才会下载选中的加密会话并恢复到 Claude Code。`env apply` 不加
`--yes` 只显示差异；`env apply --yes` 会备份后写入过滤后的 Codex Skill 文件，
`workspace apply --yes` 会用同样的方式写入可用的有限工作区正文。它们都不会安装工具、
修改原始 MCP 配置或执行命令。

恢复前可以先查看 session 记录的 Git 状态：

~~~bash
./agentsync git preview <NATIVE_SESSION_ID>
./agentsync git apply <NATIVE_SESSION_ID>
./agentsync git apply --yes <NATIVE_SESSION_ID>
~~~

git preview 只读；git apply 不加 --yes 也只预览。加上 --yes 后，显式上传的
本地 commit 会导入到隐藏的 refs/agentsync/...，工作区快照只有在目标工作区干净且
HEAD 与来源基线一致时才会应用。应用前还会检查快照涉及的路径；如果目标端已有未跟踪或被
忽略的同名文件/目录，即使 `git status` 看起来干净，也会报告冲突并保持目标不变。它不会
切换分支、merge、rebase、commit 或 push。文本输出和 `--json` 输出会列出机器可读的冲突
值，例如 `target-dirty`（目标工作区有改动）、`base-diverged`（目标 HEAD 与来源基线不一致）、
`path-collision`（目标路径会被未跟踪或忽略的文件/目录挡住）、`transfer-import-failed`
（传输正文导入失败）和 `partial-apply`（应用已经开始但没有完成）。这些值只是停止原因，
不是覆盖目标文件的指令。如果 Git 应用已经启动但中途失败，先检查并手动处理 `git status`，
目标重新通过 preflight 后再执行同一个 `git apply --yes`；AgentSync 不会自动 reset 或删除文件。

commit bundle 导入后，输出和本地应用记录会保存隐藏 commit ref、来源基线和目标分支，
方便你手动检查。可以先执行 `git log --oneline --reverse <COMMIT_REF>`，确认后再用
正常 Git 命令整合；AgentSync 不会自动执行这一步。相同传输成功应用后再次执行
`git apply --yes` 会报告 `already-applied`，不会再次修改工作区。如果之前的应用失败并
要求手动清理，需要先处理 `git status`。目标重新通过同样的 preflight 后，可以再次执行
`git apply --yes`；AgentSync 仍不会自动 reset 或删除文件。
恢复 session 时：

- AgentSync 会先检查相关的项目文件。这里检查的是项目文件，不是 session 内容；如果这些
  文件和源设备不一致，恢复会停止，也不会写入 session 文件。
- 如果确认当前项目没选错，但仍然要继续恢复，就加上 `--allow-divergent`。它只会继续恢复
  session，不会修改或同步项目文件。
- `workspace: divergent` 表示 session 已经恢复，但这台设备上的相关项目文件与源设备不一致。
- `workspace context: injected` 表示恢复的会话里加入了一条本地差异提示，不会上传到远端。
- `workspace verdict is divergent` 表示恢复被停止，session 文件没有写入。

例如：

~~~text
resumed: 将sidecar服务迁移到浏览器插件架构
session: b9dcdfcc-0470-4692-a9d9-cb3d9c6e8c6d
workspace: divergent (1 file differences)
workspace context: injected
~~~

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
| `agentsync init [--invite FILE or backend options]` | 创建或加入加密同步域并写入本地配置；使用 `--invite` 加入已有设备的同步域，可选安装 Claude Code 或 Codex SessionEnd Hook。 |
| `agentsync hook install [--agent all|claude-code|codex]` | 安装支持的 Agent SessionEnd Hook，用于会话结束后自动 push。会保留已有 Hook；Codex 安装后需要重启并在 /hooks 中信任。 |
| `agentsync install [--dir DIR] [--no-path]` | 把当前二进制安装到用户级命令目录；Windows 会更新用户级 PATH。 |
| `agentsync status [--json] [--remote]` | 显示本地状态；`--remote` 会检查远端元数据。 |
| `agentsync doctor [--json]` | 检查配置、后端访问、Agent 安装、项目身份和最近的本地错误。 |
| `agentsync project bind [--path DIR] [--name NAME or --identity ID]` | 绑定本地项目；没有 Git 时使用 `--name`。 |
| `agentsync project unbind [--path DIR or --identity ID]` | 删除本地项目绑定。 |
| `agentsync project mode normal / push-only / excluded [--path DIR or --identity ID]` | 设置项目同步策略。 |
| `agentsync project list [--json]` | 列出已绑定项目及其策略。 |
| `agentsync project discover [--json]` | 列出已授权设备发布的项目；不会自动绑定或 clone 项目。 |
| `agentsync push [--include-workspace] [--include-git-state] [--git-stash STASH_REF] [--session SESSION_ID 或 SESSION_ID] [--agentsync-hook]` | 上传当前项目的新记录和加密 Git 状态；--include-git-state 明确上传 Git 原生 commit/worktree 传输内容，--git-stash 选择已有的 `stash@{N}` 并自动启用 --include-git-state，--include-workspace 是另一条有限文件快照路径。 |
| `agentsync watch [--interval DURATION] [--once] [--json]` | 持续扫描并上传当前项目；`--once` 只执行一次。 |
| `agentsync pull --check [--json]` | 检查加密远端元数据，不下载会话正文。 |
| `agentsync list [--json]` | 列出当前项目可用的会话。 |
| `agentsync env preview [--json] SESSION_ID` | 查看 session 记录到的结构化依赖和本地组件差异；只读。 |
| `agentsync env apply [--yes] [--json] SESSION_ID` | 显示组件差异；只有加上 `--yes` 才会备份并写入过滤后的 Codex Skill 文件，MCP/settings 仍只做预览。 |
| `agentsync workspace preview [--json] SESSION_ID` | 对比明确上传的有限工作区快照和当前项目；只读，不显示文件正文。 |
| `agentsync workspace apply [--yes] [--json] SESSION_ID` | 显示工作区差异；只有加上 `--yes` 才会先备份再写入可用的过滤后文件正文，不会删除文件或执行 Git 命令。 |
| `agentsync git preview/apply [--yes] [--json] SESSION_ID` | 查看或明确应用 session 的 Git 状态；preview 和不加 --yes 的 apply 只读，apply --yes 只导入隐藏 ref 并在匹配的干净基线上应用工作区。 |
| `agentsync resume [restore options] [SESSION_ID]` | 下载并恢复一个会话；选项包括 `--version`、`--allow-limited`、`--allow-divergent`、`--no-workspace-context` 和 `--replace-existing`，选项要放在会话 ID 前面。 |
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

没有设置 `AGENTSYNC_CONFIG_DIR` 时，AgentSync 在所有平台都使用当前用户 home 下
清晰可见的 `~/.agentsync`：

| 平台 | 默认目录 |
|---|---|
| Windows | `%USERPROFILE%\.agentsync` |
| macOS | `~/.agentsync` |
| Linux 和其他 Unix 系统 | `~/.agentsync` |

初始化成功后，`init` 会打印实际目录。需要使用自定义配置目录时，可以覆盖这个默认目录：

~~~bash
export AGENTSYNC_CONFIG_DIR="$HOME/.agentsync-custom"
~~~

PowerShell：

~~~powershell
$env:AGENTSYNC_CONFIG_DIR = Join-Path $env:USERPROFILE '.agentsync-custom'
~~~

该目录包含配置、加密 secrets、设备密钥和本地状态。不要提交或公开这个目录。Claude
Code 的数据目录与 AgentSync 分开，可以通过 `CLAUDE_CONFIG_DIR` 指定。

在 CI 或短期测试中，可以设置 `AGENTSYNC_ACCESS_KEY_ID` 和
`AGENTSYNC_SECRET_ACCESS_KEY`；`AGENTSYNC_SESSION_TOKEN` 可选。环境变量中的凭据不会写入磁盘。

## 限制与安全

- 会话正文、环境清单、工作区快照和 Git 传输正文会在加密前尽量压缩。压缩封装有格式版本、
  解压大小和比例限制；过小或不可压缩的内容保持原样，旧的未压缩远端对象仍然可以读取。
  凭据、token 和密钥材料不会进入这条流水线。
- 每台设备都有独立的 device ID 和远端分支。
- `push` 写入当前设备的分支，不会把该分支再拉回本机。
- `pull --check` 只读取元数据；`resume` 才是显式下载正文并恢复的操作。
- 默认不会上传项目文件正文：普通 push、Hook 和 watch 只同步会话、环境和小型 Git
  状态摘要。push --include-workspace 会生成有限快照：Git 项目使用 session fingerprint，
  无 Git 项目扫描安全过滤后的项目目录。push --include-git-state 是另一条明确的 Git 原生 commit/worktree bundle 传输
  路径。传输前会做敏感内容预检，无法安全检查时直接失败；整个 .git、token、凭据、
  密钥材料和 .env 文件永不上传。git preview 只读；git apply --yes 只会把 commit
  导入隐藏 ref，并在目标工作区干净且基线匹配时应用工作区快照，不会切换分支、提交、
  merge、rebase 或 push。
- 目标设备必须提前准备好 Claude Code 和项目。
- Git 项目有更强的工作区检查；没有 Git 的项目使用 manual identity。普通工作区上下文使用 touched 文件回退检查，显式 --include-workspace 时使用有限目录扫描。
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
