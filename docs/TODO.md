# AgentSync 开发 TODO 与完成情况

> 盘点基线：2026-08-15；当前 HEAD：26844ff feat(cli): add polling watch mode。
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
| PRD P0 功能 | 🟡 | 主要代码已实现，但正式 MVP 验收、真实跨平台矩阵、CI 和发布链路尚未闭环 |
| PRD P1 功能 | 🟡 | project、device、stats、watch 等已实现；history、工作区上下文注入、shell completion 仍有缺口 |
| PoC-1 路径与恢复 | 🟡 | 同操作系统/同机模拟通过部分范围，真实 Windows ↔ POSIX 和复杂会话仍待验证 |
| PoC-1b 新设备恢复 | ✅ | 同机模拟的第二设备流程已通过；真实跨系统和复杂会话仍属于补充验收 |
| PoC-2 工作区指纹 | ✅ | 9 个场景和 454 文件基准已有 PoC 记录与实现 |
| PoC-3 S3/dir 一致性 | ⬜ | 当前没有正式 PoC-3 文档或验收矩阵 |
| PoC-4 Codex Adapter | ⬜ | 尚未开始，属于 P2 |
| MVP 端到端验收 | 🟡 | 各能力已有实现和局部测试，但尚未形成可重复的全矩阵验收 |
| 发布工程 | ⬜ / 🟡 | 有跨平台构建脚本，缺 CI、发布包、安装渠道和升级验证 |

结论：项目已经不是 README 所描述的“只有接口脚手架”，而是具备完整核心同步链路的 pre-alpha 实现。当前最重要的工作是把跨平台、Remote 一致性、失败恢复和发布保障固化为可重复验收。

## 2. 已完成或已有直接实现的事项

### 2.1 配置、密钥和 Remote 基础

| 事项 | 状态 | 当前依据 |
|---|---|---|
| 配置层 | ✅ | internal/config 已提供配置加载、校验、保存和默认值处理 |
| 初始化流程 | ✅ | agentsync init 创建配置、设备身份和本地密钥材料；口令通过交互输入，不从命令行参数接收 |
| 本地密钥与加密 | ✅ | internal/crypto 已提供密钥文件、口令解锁、Recovery Key 相关流程和加密/解密能力 |
| 口令 API | ✅ | keyfile 已有 ChangePassphrase 和 ResetPassphrase API；CLI 入口仍列在未完成项 |
| 本地目录 Remote | ✅ | internal/remote/dir 已实现对象读写、列表和目录布局 |
| S3 Remote | ✅ | internal/remote/s3 与 SigV4 相关实现已存在 |
| 远端对象布局 | ✅ | 项目、会话、设备、分片和元数据使用稳定的版本化布局 |
| 隐私边界 | ✅ | 对象标识使用不透明 ID；明文会话内容和本地路径不直接写入 Remote 元数据 |
| 设备身份和模式 | ✅ | 配置中保存设备 ID/名称，并支持 normal、push-only、disabled 三种设备模式 |
| 配置更新安全性 | 🟡 | 已有原子保存等基础能力；多进程同时写配置时目前是后写者胜，仍需评估 |

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
| 远端格式兼容 | 🟡 | 当前格式和旧对象读取有实现，但正式版本升级/迁移策略仍需在发布前固定 |

### 2.4 CLI 命令盘点

| 命令 | 状态 | 说明 |
|---|---|---|
| agentsync init | ✅ | 初始化配置、密钥和设备身份 |
| agentsync status | ✅ | 输出本地状态，并检查元数据层的远端变化 |
| agentsync list | ✅ | 合并本地会话和远端元数据，区分本地/外部设备来源 |
| agentsync resume | ✅ | 选择版本并执行恢复；远端 body 读取前有设备模式和工作区检查 |
| agentsync push | ✅ | 增量上传、元数据发布、队列重试 |
| agentsync doctor | 🟡 | 已覆盖配置、backend、Agent、版本、兼容性、hook、项目检查；缺少统一的最近错误持久化报告 |
| agentsync project | ✅ | 项目策略、识别和相关配置命令已实现 |
| agentsync history | 🟡 | 能读取和展示版本历史；尚无 prune/cleanup |
| agentsync device | ✅ | 支持 status、mode、list、rename、remove，并处理确认 |
| agentsync stats | ✅ | 输出本地恢复统计 |
| agentsync pull | ✅ | 当前作为显式的 metadata-only pull check 使用；不是自动下载全部远端会话 |
| agentsync watch | ✅ | 轮询本地变化并 push，支持本地快照去重、失败重试、once/json；当前是 push-only |
| JSON 输出 | ✅ | 已处理提示和 JSON 输出隔离，避免交互提示污染机器可读输出 |
| shell completion | ⬜ | PRD 要求的补全尚未实现 |

### 2.5 设备标识与拉取规则

这一项已经在核心设计和代码中考虑，当前规则如下：

1. 每台设备有持久化的设备 ID。它同时参与队列键和远端对象布局，设备 A 上传时只写设备 A 自己的 branch。
2. push 不会因为上传而先列出或下载远端会话，因此设备 A 对自己刚上传的内容不会发生一次无意义的 self-pull。
3. status、list 和显式 pull check 只读取必要的加密元数据，并排除本设备 branch；它们用于发现外部设备的版本变化，不会把全部 shard body 拉下来。
4. 只有用户显式进入 resume/restore 选择流程后，程序才会按选定版本读取远端会话 body，并继续做兼容性、工作区和分叉检查。
5. push-only 和 disabled 设备在 body-read 流程前会被拒绝；因此可以把某设备配置成“只上传、不接收远端会话”。
6. watch 当前只负责本地变化检测和 push，不负责后台自动拉取远端会话。

因此，设备 A 持续对话时，正常的 watch 上传不会触发全量同步；设备 B 或设备 A 需要查看外部变化时，再通过显式 metadata-only check 和 resume 进入读取流程。剩余工作是用真实的 A/B/C 设备场景补齐验收和用户文档，明确 normal、push-only、disabled 的默认行为。

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

状态：⬜ 尚未开始。

需要单独建立 PoC 文档和可重复矩阵，至少覆盖：

- S3 list 延迟、对象刚写入后不可见、分页和临时错误；
- dir Remote 在复制中断、目录不完整、部分对象缺失时的行为；
- metadata 已可见但 shard 尚未可见；
- shard 已存在但 metadata 尚未发布；
- 网络中断、进程中断、重试和重复上传；
- list 结果缺口不能被误判为永久删除；
- 远端恢复、队列重试和重新扫描后的最终一致结果；
- S3 与 dir 的相同场景对照测试。

### 3.4 MVP 端到端验收

状态：🟡。

需要把 PRD §15 的验收固化为脚本或集成测试：

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

状态：⬜ / 🟡。

- 口令更换 CLI：crypto 层已有 ChangePassphrase，但尚无用户可执行的 change-passphrase 命令和完整发布流程；
- Recovery Key 重置 CLI：底层 API 已有，尚未提供清晰的交互入口、备份确认和失败恢复说明；
- 远端删除会话：PRD 要求按会话删除，当前没有对应 CLI；
- 远端删除项目：当前没有对应 CLI；
- 清空整个 Remote：当前没有对应 CLI；
- 上述删除操作需要统一的显式确认、--yes 语义、错误恢复和审计提示；
- history cleanup/prune：当前 history 主要用于读取和展示，尚未提供保留策略和清理命令。

### 4.2 P1 用户体验

状态：⬜ / 🟡。

- 工作区差异上下文注入：PRD §9.5 要求用户继续恢复时把差异说明注入会话上下文；当前有检查和展示，但尚未完成上下文注入；
- shell completion：bash、zsh、fish、PowerShell 补全尚未提供；
- doctor 最近错误：当前 doctor 能检查当前状态，但没有统一的持久化错误历史和可查询报告；
- 失败场景的文档：需要把“只读元数据检查”和“读取远端会话 body”的边界讲清楚；
- 设备模式的默认推荐：需要明确普通设备、只上传设备和禁用设备分别适用的场景。

### 4.3 测试与可观测性闭环

状态：🟡。

- 关键包已有单元测试、集成测试和部分 fuzz 测试；
- 测试代码和数据按当前 .gitignore 规则未纳入提交，需要明确哪些测试必须迁移为仓库内的稳定回归用例；
- 还缺少真实 Agent、真实 Remote、跨系统和故障注入矩阵；
- 还缺少统一的测试报告、失败样本归档和版本兼容记录。

## 5. 工程、发布和文档 TODO

### 5.1 跨平台和 CI

状态：🟡。

- scripts/build.sh 已支持 windows/amd64、windows/arm64、darwin/amd64、darwin/arm64、linux/amd64、linux/arm64。
- 仍需：
  - CI 中自动执行 go test、race、vet、build；
  - Windows、macOS、Linux/WSL 原生矩阵；
  - 真实 Agent 集成测试和本地目录 Remote 测试；
  - 交叉编译产物启动检查；
  - 在 CI 中验证无 cgo 和可复现构建约束。

### 5.2 发布和安装

状态：⬜ / 🟡。

PRD §18 要求目前尚未形成完整交付链：

- GitHub Releases 自动发布 Windows/macOS 预编译包；
- Homebrew formula；
- Scoop manifest；
- go install 的正式文档和版本验证；
- checksum、版本签名或发布校验流程；
- 发布前的升级、回滚和配置兼容性测试。

### 5.3 文档同步

状态：⬜。

- README 仍写着“PoC-1 未开始、没有任何同步能力”，与当前代码和 PoC 记录不一致，需要改为“核心链路已实现，MVP 跨平台验收进行中”；
- README 引用的 docs/archive 目录当前不存在，需要修正链接或补齐目录；
- 多数 docs/specs 文件头仍为 Draft，需要在代码和验收完成后逐项更新状态；
- 需要补一份五分钟可运行指南，覆盖 init、push、list、resume、watch；
- 需要补充 Adapter/Remote 接口的外部实现示例；
- 需要把 PRD §9.3 的跨用户名、跨平台路径改写结论与当前 Adapter 规格保持一致；
- 需要把已知限制集中列出：访问撤销限制、口令和 Recovery Key 丢失、元数据侧信道、Agent 版本变化。

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

1. 完成 PoC-3，先锁定 S3/dir 在最终一致性和部分同步场景下的行为。
2. 组织 Windows ↔ macOS 的真实跨设备验收，并补齐 PoC-1 遗留项。
3. 把 MVP 验收流程固化为可重复的集成测试/验收脚本。
4. 补齐远端删除、历史清理、口令更换/重置 CLI 等安全与生命周期能力。
5. 实现工作区差异上下文注入，并完善 doctor 最近错误。
6. 建立 CI、发布包、安装方式和 README 五分钟指南。
7. 最后推进 shell completion、P2 Remote 和更多 Agent。

## 9. 本轮盘点后的判断

当前项目已经不是 README 所描述的“只有接口脚手架”，而是具备完整核心同步链路的 pre-alpha 实现。下一阶段的主要风险不再是基础算法或目录布局，而是：

- 真实跨平台行为是否与同机模拟一致；
- Remote 最终一致性和部分同步是否会制造错误的缺口判断；
- 用户在失败、分叉和工作区不一致时是否能得到足够清楚且安全的下一步；
- 发布和集成测试能否把这些保证固化下来。

因此，在新增 P2 功能前，应优先完成第 8 节的前 3 项。
