# AgentSync 开发 TODO 与完成情况

> 盘点基线：2026-08-16；实现基线：1a0ca09 feat(sync): add signed device pairing；本次 TODO 同步随当前提交完成。
> 依据：PRD v2.0、docs/specs 下的模块规格、poc 记录、当前源代码和测试。
> 本文只记录当前仓库已经能证明的状态；“代码存在”不等于“跨平台验收已经完成”。

## 状态标记

| 标记 | 含义 |
|---|---|
| ✅ | 已实现，并且当前代码或测试有直接依据 |
| 🟡 | 已有实现，但仍缺验收、边界场景或用户入口 |
| ⬜ | 尚未实现或尚未正式开始 |
| 🔁 | 持续维护项，不以一次提交作为完成条件 |
| 🚫 | 根据 PRD 明确不做或不提供 |

## 1. 总体结论

| 范围 | 状态 | 结论 |
|---|---|---|
| 核心同步链路 | ✅ | 配置、密钥、Adapter、Remote、增量 push、元数据、恢复、队列和 CLI 主链路均已落地 |
| 同步域与设备入组 | 🟡 | 同步域由 Remote namespace + keyfile 确定；domain fingerprint、持久化绑定和 signed device invite/init --invite 配对流程已实现；强访问撤销和真实跨设备验收仍待补 |
| 多项目范围与项目策略 | ✅ | push/watch 当前只处理当前项目，normal/push-only/excluded 和无 Git manual identity 主同步路径均已接入 |
| PRD P0 功能 | 🟡 | 主要代码、MVP 合成验收、CI 和发布基础链路已实现；真实跨平台、Agent 和 Remote 验收仍待闭环 |
| PRD P1 功能 | 🟡 | project、device、stats、watch、history、工作区上下文、doctor、shell completion 和 manual identity 均有基础实现；同步域入组与真实跨设备验收仍待补 |
| PoC-1 路径与恢复 | 🟡 | 同操作系统/同机模拟通过部分范围，真实 Windows ↔ POSIX 和复杂会话仍待验证 |
| PoC-1b 新设备恢复 | ✅ | 同机模拟的第二设备流程已通过；真实跨系统和复杂会话仍属于补充验收 |
| PoC-2 工作区指纹 | ✅ | 9 个场景和 454 文件基准已有 PoC 记录与实现 |
| PoC-3 S3/dir 一致性 | 🟡 | 一致性保护和模拟场景已完成；真实 S3/第三方同步工具矩阵待验收 |
| PoC-4 Codex Adapter | ⬜ | 尚未开始，属于 P2 |
| MVP 端到端验收 | 🟡 | poc/mvp 合成矩阵和 CI 已可重复运行；真实跨平台、S3 和 Agent 环境仍待验收 |
| 发布工程 | 🟡 | CI 原生矩阵、交叉构建、GitHub Release、checksum 和包管理器 manifest 模板已实现；签名及外部渠道发布仍待配置 |

结论：项目已经不是 README 所描述的“只有接口脚手架”，而是具备完整核心同步链路的 pre-alpha 实现。当前最重要的工作是把跨平台、Remote 一致性、失败恢复和发布保障固化为可重复验收。

## 2. 已完成或已有直接实现的事项

### 2.1 配置、密钥和 Remote 基础

| 事项 | 状态 | 当前依据 |
|---|---|---|
| 配置层 | ✅ | internal/config 已提供配置加载、校验、保存和默认值处理 |
| 初始化流程 | ✅ | agentsync init 创建配置、设备身份和本地密钥材料；口令通过交互输入，不从命令行参数接收 |
| 本地密钥与加密 | ✅ | internal/crypto 已提供密钥文件、口令解锁、Recovery Key 相关流程和加密/解密能力；internal/config/secrets.go 及其回归测试已纳入版本控制，随机源失败时不会发布半成品 device.key |
| 口令 API | ✅ | keyfile 已有 ChangePassphrase 和 ResetPassphrase API；agentsync passphrase change/reset CLI 已接入，真实远端验收待补 |
| 本地目录 Remote | ✅ | internal/remote/dir 已实现对象读写、列表和目录布局 |
| S3 Remote | ✅ | internal/remote/s3 与 SigV4 相关实现已存在 |
| 远端对象布局 | ✅ | 项目、会话、设备、分片和元数据使用稳定的版本化布局 |
| 隐私边界 | ✅ | 对象标识使用不透明 ID；明文会话内容和本地路径不直接写入 Remote 元数据 |
| 设备身份和模式 | ✅ | 配置中保存设备 ID/名称，并支持 normal、push-only、disabled 三种设备模式 |
| 配置更新安全性 | ✅ | 原子保存保证不会产生半写文件；规格已明确多进程同时写采用后写者胜，不做锁，绑定丢失可察觉且可重做 |

### 2.2 Claude Adapter、路径和项目识别

| 事项 | 状态 | 当前依据 |
|---|---|---|
| Claude Code 检测 | ✅ | Adapter 能识别本地 Claude 状态目录并报告版本/可用性 |
| 版本兼容性 | ✅ | 已有 supported、limited、unsupported 等兼容性判断 |
| 会话发现 | ✅ | 能发现本地会话并读取规范化记录 |
| 规范化与本地化 | ✅ | 已实现 canonical record 与本地 Agent 格式之间的转换 |
| 路径改写 | ✅ | 恢复时根据源工作区与当前工作区信息改写路径 |
| 原子写入 | ✅ | 本地会话写入和目标文件替换使用原子化流程 |
| touched 记录和增量读取 | ✅ | 已有 touched 状态、游标和增量读取逻辑 |
| Hook | ✅ | 已实现 Claude 相关 hook 的安装/检查基础能力 |
| Git 项目识别 | ✅ | 已实现 Git identity、项目目录识别和项目元数据 |
| 手工/无 Git 项目身份 | ✅ | project bind --name 保存的 manual identity 已由共享 current-project resolver 接入 push/watch/list/pull/resume/history/status/remote |
| 工作区指纹 | ✅ | internal/project/fingerprint.go 已实现 Git 状态、相对路径和排除规则相关指纹 |
| 模糊测试 | ✅ | ReadRecords、Canonicalize、Decrypt、ParseShard 等关键边界已有 fuzz 测试 |

### 2.3 Syncer、Syncflow 和恢复链路

| 事项 | 状态 | 当前依据 |
|---|---|---|
| 增量上传 | ✅ | 按本地游标读取新增记录，生成 canonical stream 并切分为分片 |
| 分片与对象 | ✅ | 已有 shard、metadata、branch、cursor 等对象模型 |
| 远端元数据 | ✅ | push 会发布加密会话摘要/版本元数据，便于列表和 pull check |
| 队列与重试 | ✅ | internal/syncer 和 internal/syncflow 已有持久化队列、失败分类和重试流程 |
| 设备分支隔离 | ✅ | queue key 和 Remote 对象布局包含本地设备 ID；每台设备只写自己的分支 |
| 远端元数据检查 | ✅ | status/list/pull check 可读取变更的外部设备元数据，不直接读取全部 shard body |
| 恢复与 resume | ✅ | 能选择本地或远端版本，执行兼容性和工作区检查后恢复 |
| fork 版本 | ✅ | 恢复链路已有 fork/分叉选择和版本处理逻辑 |
| 工作区安全检查 | ✅ | 恢复前会检查指纹缺失、过期、差异和已有会话等情况 |
| 恢复统计 | ✅ | stats 已区分本设备恢复与跨设备恢复，不参与同步决策 |
| 远端格式兼容 | ✅ | 已统一记录 version 1 基线、未来版本 fail-closed、迁移前置条件和回滚规则；真正发生格式升级时再实现对应 migration |

### 2.4 CLI 命令盘点

| 命令 | 状态 | 说明 |
|---|---|---|
| agentsync init | ✅ | 初始化配置、密钥和设备身份 |
| agentsync status | ✅ | 输出本地状态，并检查元数据层的远端变化 |
| agentsync list | ✅ | 合并本地会话和远端元数据，区分本地/外部设备来源 |
| agentsync resume | ✅ | 选择版本并执行恢复；远端 body 读取前有设备模式和工作区检查 |
| agentsync push | ✅ | 增量上传、元数据发布、队列重试 |
| agentsync doctor | ✅ | 配置、backend、Agent、版本、兼容性、hook、项目检查和脱敏的最近错误历史均已覆盖 |
| agentsync project | ✅ | 项目策略、Git/manual identity 配置命令和当前项目共享解析已实现 |
| agentsync history | 🟡 | 支持读取和展示版本历史、cleanup，以及按 --keep/--before 的 prune；本地故障注入回归已补，真实 Remote 故障验收待补 |
| agentsync device | ✅ | 支持 status、mode、list、rename、remove，并处理确认 |
| agentsync remote | ✅ | 支持按会话、按项目和清空 Remote；删除前统一确认并支持 --yes，失败时报告已删除对象数 |
| agentsync stats | ✅ | 输出本地恢复统计 |
| agentsync pull | ✅ | 当前作为显式的 metadata-only pull check 使用；不是自动下载全部远端会话 |
| agentsync watch | ✅ | 轮询本地变化并 push，支持本地快照去重、失败重试、once/json；当前是 push-only |
| JSON 输出 | ✅ | 已处理提示和 JSON 输出隔离，避免交互提示污染机器可读输出 |
| shell completion | ✅ | 已提供 Bash、Zsh、Fish、PowerShell（含 pwsh 别名）脚本生成和静态命令/参数候选 |

### 2.5 设备标识与拉取规则

这一项已经在核心设计和代码中考虑，当前规则如下：

1. 每台设备有持久化的设备 ID。它同时参与队列键和远端对象布局，设备 A 上传时只写设备 A 自己的 branch。
2. push 不会因为上传而先列出或下载远端会话，因此设备 A 对自己刚上传的内容不会发生一次无意义的 self-pull。
3. status、list 和显式 pull check 只读取必要的加密元数据，并排除本设备 branch；它们用于发现外部设备的版本变化，不会把全部 shard body 拉下来。
4. 只有用户显式进入 resume/restore 选择流程后，程序才会按选定版本读取远端会话 body，并继续做兼容性、工作区和分叉检查。
5. push-only 和 disabled 设备在 body-read 流程前会被拒绝；因此可以把某设备配置成“只上传、不接收远端会话”。
6. watch 当前只负责本地变化检测和 push，不负责后台自动拉取远端会话。

因此，设备 A 持续对话时，正常的 watch 上传不会触发全量同步；设备 B 先通过 device invite/init --invite 明确加入同一同步域，再通过 metadata-only check 和 resume 进入读取流程。剩余工作是用真实 A/B/C 设备场景补齐验收，并单独设计强访问撤销。

### 2.6 同步域、项目范围和无 Git 项目

- 一个同步域可以包含多个项目；project ID 从同步域密钥和项目稳定身份派生，项目之间不会共享 session 历史；
- 当前 push/watch/list/pull/resume/history 都围绕当前项目运行，不会默认扫描整台机器的所有 Claude Code 项目；
- project mode 的 normal、push-only、excluded 是项目级策略，与设备级 normal、push-only、disabled 分开；
- 当前同步域是 Remote namespace + keyfile 的隐式组合；设备 ID 只区分组内 branch，不负责强授权；domain fingerprint、持久化 binding 和 signed device invite/init --invite 已用于确认第二台设备打开的是同一同步域；强访问撤销仍待设计；
- domain fingerprint：已接入 init/status/doctor；新配置持久化 accepted value，所有 Remote-reading 命令校验 namespace/keyfile binding；device invite/init --invite 已补齐显式配对流程；
- 设备邀请包与 init --invite 已实现，用于显式确认第二台设备打开的是同一 Remote/keyfile；当前 device remove 只删除远端数据，不能撤销设备已有的密钥材料；
- 无 Git 项目应使用跨设备稳定的 manual identity，例如 manual:client-project，不能使用绝对路径、用户名或主机名；共享 resolver 已接入主同步路径；
- 当前 project bind --name 的 binding 已由主同步命令统一读取；无 binding 时仍按 fail-closed 规则拒绝自动同步。

## 3. 仍需推进的 P0 / MVP 项

### 3.1 PoC-1：真实跨平台路径与恢复

状态：🟡 部分完成。

已有结果：

- 同操作系统、不同路径和第二设备模拟流程已有实现与记录；
- 基础路径 canonicalize/localize、指纹和恢复检查已经存在。

仍需实测并记录：

- Windows ↔ macOS/Linux 的真实恢复；
- 不同用户名、不同 home 目录和不同盘符；
- Windows 分隔符、大小写差异和大小写敏感文件系统；
- 非 ASCII、空格、长路径；
- Agent 正在写入时的读取/恢复；
- 长会话、复杂会话、附件、plan、subagent 等记录；
- 失败中断、重复恢复和恢复后继续 push；
- 内存占用、耗时和大记录边界。

### 3.2 PoC-2：工作区指纹规模和误报

状态：🟡 已通过基础范围，仍需扩大。

已有结果：

- 9 个场景已有 PoC 记录；
- 454 文件基准约 0.2 秒；
- internal/project/fingerprint.go 和测试已落地。

仍需补充：

- 1 万级文件和 GB 级仓库；
- 大仓库执行 git status 的实际成本；
- 更多路径、重命名、未跟踪文件、子模块和特殊权限场景；
- 真实项目中的误报/漏报样本；
- 面向用户的差异解释，帮助用户判断继续恢复、fork 或取消。

### 3.3 PoC-3：S3/dir 最终一致性与部分同步

状态：🟡 基础实现完成，真实环境待验收。

已新增 docs/specs/poc-3-remote-consistency.md；代码层保护和本地模拟矩阵已完成，真实环境仍需按该文档验收：

- S3 list 延迟、对象刚写入后不可见、分页和临时错误；
- dir Remote 在复制中断、目录不完整、部分对象缺失时的行为；
- metadata 已可见但 shard 尚未可见；
- shard 已存在但 metadata 尚未发布；
- 网络中断、进程中断、重试和重复上传；
- list 结果缺口不能被误判为永久删除；
- 远端恢复、队列重试和重新扫描后的最终一致结果；
- S3 与 dir 的相同场景对照测试。

### 3.4 MVP 端到端验收

状态：🟡 合成矩阵已完成，真实环境待验收。

poc/mvp 已将 PRD §15 的核心同步、恢复、分叉和失败关闭场景固化为可执行检查；真实平台与 Remote 仍需按 docs/specs/mvp-acceptance-matrix.md 验收：

- 新设备只拿到配置、Remote 地址、口令/Recovery Key 时可完成恢复；
- 源设备离线、Remote 只有部分对象时行为安全可解释；
- Windows/macOS/Linux 或 WSL 的跨平台恢复；
- 工作区一致、差异、无指纹、过期指纹的全部分支；
- 并发写入、fork、重复 push、恢复中断和队列重试；
- S3、dir、错误配置、网络故障、进程崩溃；
- 删除/卸载 AgentSync 后，Agent 原有数据不被误删；
- 口令错误、Recovery Key 丢失、远端对象损坏时的错误提示和退出码。

## 4. 尚未实现或尚未闭环的功能

### 4.1 安全与生命周期

状态：🟡（代码路径已完成，真实远端故障验收仍待补）。

- 口令更换 CLI：✅ 已实现 `agentsync passphrase change`；保留数据密钥并替换已存在的远端 keyfile，仍需真实远端故障验收；
- Recovery Key 重置 CLI：✅ 已实现 `agentsync passphrase reset`；使用 Recovery Key 替换远端 keyfile，并在读取前明确提示原 Recovery Key 不会重新生成；仍需真实故障验收；
- 远端删除会话：✅ 已实现 `agentsync remote delete-session`；默认从当前项目和 native session ID 推导不透明 remote ID，也支持 `--remote-id`；
- 远端删除项目：✅ 已实现 `agentsync remote delete-project`；仅按当前稳定项目身份生成项目前缀，不接受任意远端前缀；
- 清空整个 Remote：✅ 已实现 `agentsync remote delete-all`；显式确认会提示包含 keyfile 和设备记录，`--yes` 可用于无人交互；
- 上述删除操作：✅ 统一使用显式确认和 `--yes` 语义；部分失败会返回已删除对象数，避免把不完整清理误报为成功；
- history cleanup/prune：✅ 已实现显式 cleanup 和按 maximal version 的 `--keep`/`--before` prune；未知更新时间的版本默认保留，避免误删；真实远端验收待补。

### 4.2 P1 用户体验

状态：🟡（已有基础实现，显式入组和真实跨设备验收仍待补）。

- 工作区差异上下文注入：✅ 已实现；resume 默认把差异说明作为本地 isMeta 记录追加到恢复会话，后续 push 会过滤该记录，--no-workspace-context 可关闭；
- shell completion：✅ 已实现 Bash、Zsh、Fish、PowerShell 补全，入口为 agentsync completion <shell>；
- doctor 最近错误：✅ 已实现最多 20 条、仅含时间/命令/错误类别的脱敏持久化历史，并接入 doctor 文本/JSON 报告；
- 失败场景的文档：✅ README 和 docs/specs 已说明 metadata-only check、body read、工作区差异、设备模式和远端清理边界；
- 设备模式的默认推荐：✅ README 已说明 normal、push-only、disabled 的适用行为；

- 同步域指纹与入组确认：✅ domain fingerprint、持久化 namespace binding、signed device invite 和 init --invite 已实现；强访问撤销与真实跨设备验收仍待补；
- 多项目选择：✅ 当前 push/watch 只处理当前项目，project mode 支持 normal、push-only、excluded；全局 --all 未规划为默认行为；
- 无 Git 项目：✅ manual identity 设计和共享 current-project resolver 已接入主同步路径，并有边界测试；

### 4.3 测试与可观测性闭环

状态：🟡。

- 关键包已有单元测试、集成测试和部分 fuzz 测试，且稳定回归测试已纳入仓库；
- 稳定回归测试：✅ 已取消测试代码的全局忽略并纳入仓库；本地真实 Agent 数据和敏感 fixture 仍按 .gitignore 忽略；
- 基础契约回归：✅ dir/S3 写入在 body 读取期间响应取消且不发布对象；命令注册表完整性、归档干净树构建和 watch 缺少 Agent fixture 隔离均已覆盖；
- 还缺少真实 Agent、真实 Remote、跨系统和故障注入矩阵；
- CI 已上传 go test JSON 报告，docs/acceptance/README.md 已提供失败样本和版本兼容记录模板；真实失败样本仍需在外部矩阵中补齐。

## 5. 工程、发布和文档 TODO

### 5.1 跨平台和 CI

状态：🟡（CI/构建基础已完成，真实环境仍待验收）。

- 目标矩阵：windows/amd64、windows/arm64、darwin/amd64、darwin/arm64、linux/amd64、linux/arm64。
- 已完成：scripts/build.sh 和 scripts/build.ps1 均支持六个目标，注入版本/commit/UTC 时间并使用 CGO_ENABLED=0。
- 已完成：GitHub CI 在 Windows、macOS、Linux 原生矩阵执行 test、vet、build；Ubuntu 另执行 race。
- 已完成：CI 运行交叉构建并上传六个产物；交叉编译和发布脚本均使用 trimpath。
- 仍需：真实 Agent 集成测试、本地目录 Remote 之外的真实 S3/第三方同步工具矩阵。
- 已完成：scripts/build.sh 和 scripts/build.ps1 在当前主机目标属于六目标矩阵时执行 `version`/`help` 启动烟测；非主机目标仍只做交叉编译。
- 已完成：使用 `git archive HEAD` 在无未跟踪文件的干净树中执行 `go test ./...`，验证发布提交包含必要源码（包括本地 secrets 实现）。
- 仍需：对非主机目标做目标系统启动检查和可复现构建抽样验证。
### 5.2 发布和安装

状态：🟡（发布自动化已完成，外部渠道和签名仍待配置）。

已完成的交付链路：
- GitHub Releases workflow：v* tag 自动执行 test/vet、构建六个目标并发布 release 资产；
- checksum：release.sh 为每个二进制生成 SHA-256 checksums.txt；
- Homebrew formula / Scoop manifest：已提供带版本和 hash 渲染的模板，并作为 release 资产输出；
- go install、版本检查、升级/回滚和配置兼容性说明已写入 README 与 release-engineering spec；
仍需明确的外部条件：
- Homebrew tap、Scoop bucket 的实际维护者仓库尚未指定，workflow 不会擅自修改第三方仓库；
- 发布签名/证明材料（例如 cosign 或平台签名）尚未配置密钥和信任根；
- 升级、回滚和各目标系统启动检查仍需真实发布前验收。
### 5.3 文档同步

状态：🟡（README 和核心规格已同步，真实验收记录仍需补齐）。

- README：✅ 已改为当前核心链路、MVP/真实验收状态，并补充五分钟运行指南、completion、测试和已知限制；
- README 链接：✅ 已移除不存在的 docs/archive 引用，改为现有 docs/specs 和 TODO 入口；
- docs/specs：🟡 workspace context、doctor、completion、release、format-versioning、sync-domain-project-scope 规格已同步实现状态；docs/acceptance 已补齐外部验收记录模板，历史 Draft 规格仍需随真实验收逐项更新；
- 五分钟指南：✅ README 已覆盖 init、push、list、resume、watch、pull check 和 doctor；
- Adapter/Remote 示例：✅ 已新增可编译的 examples/remote-memory 最小 Remote 实现、契约测试和接入检查清单；
- PRD §9.3 路径改写：✅ 当前 Adapter 规格和 README 已明确跨用户名、跨平台路径通过 canonical/localized 规则处理；
- 已知限制：✅ README 已集中列出访问撤销、口令/Recovery Key 丢失、元数据侧信道和 Agent 版本变化限制；

## 6. 适配器和存储层的持续维护项

### 6.1 Claude Code Adapter

状态：🔁。

- 跟踪 Claude Code 会话格式、目录结构和 hook 配置变化；
- 持续增加已验证 Agent 版本；
- 观察 memory/、backup、plan、subagent 等相邻状态；
- 新版本未知时保持 limited 行为，但恢复必须继续经过显式确认；
- 对新版本补充真实会话和复杂会话样本；
- 长路径、跨用户名和非 ASCII 路径需要真实端到端样本。

### 6.2 Remote 和同步格式

状态：🔁 / 🟡。

- 分片目标大小和记录/字节双阈值仍需真实负载测量；
- 远端列表、最终一致性和分页异常需持续验证；
- 队列重试策略需基于真实网络失败样本调整；
- 远端格式版本升级、旧对象读取和迁移策略需要在发布前固定；
- 配置层多进程并发写入目前是后写者胜；常驻 watch 使用场景下需重新评估；
- Windows 上的权限位不等价于 ACL，若威胁模型扩大到同机其他用户，需要补 ACL 设计。

## 7. 明确延后或不在当前规划内的事项

| 功能 | 状态 | 说明 |
|---|---|---|
| Google Drive / OneDrive / WebDAV | ⬜ P2 | 先完成 dir/S3 和一致性验证 |
| Codex Adapter | ⬜ P2 | 必须先完成 PoC-4；在通过前不得对外承诺支持 |
| Gemini CLI、OpenCode 等更多 Agent | ⬜ V2 | 由社区贡献优先 |
| Linux/WSL 产品化验收 | ⬜ V1 | build 目标已有，完整运行矩阵尚未完成 |
| 本地 Web UI | ⬜ V2 | PRD 明确 CLI 优先 |
| 进入项目目录自动拉取 | ⬜ V2/长期 | 当前只提供显式 pull check、resume 和 push/watch |
| AgentSync 官方托管后端 | 🚫 | PRD 明确不提供、不规划托管服务 |
| 遥测、崩溃上报、自动采集数据 | 🚫 | PRD 的隐私边界明确禁止 |

## 8. 建议推进顺序

1. 完成强访问撤销设计、密钥轮换和真实同步域成员验收。
2. 完成 PoC-3 真实 S3/dir 和第三方目录同步工具验收，锁定最终一致性和部分同步行为。
3. 组织 Windows ↔ macOS 的真实跨设备验收，并补齐 PoC-1 遗留项。
4. 用 poc/mvp 持续执行合成矩阵，并补齐真实平台、Remote 和 Agent 验收记录。
5. 对 passphrase change/reset、远端删除和 history prune 做真实远端故障验收。
6. 完成真实 Agent、Remote、跨系统和故障注入验收，并归档结果和版本兼容记录。
7. 完成发布签名、外部 Homebrew/Scoop 渠道接入和目标系统启动/升级回滚验收。
8. 在 P0/P1 外部验收闭环后，再推进 P2 Remote、Codex Adapter 和更多 Agent。

## 9. 本轮盘点后的判断

当前项目已经不是 README 所描述的“只有接口脚手架”，而是具备完整核心同步链路的 pre-alpha 实现。下一阶段的主要风险不再是基础算法或目录布局，而是：

- 真实跨平台行为是否与同机模拟一致；
- Remote 最终一致性和部分同步是否会制造错误的缺口判断；
- 用户在失败、分叉和工作区不一致时是否能得到足够清楚且安全的下一步；
- 发布和集成测试能否把这些保证固化下来。

因此，在新增 P2 功能前，应优先完成第 8 节的前 3 项。
