# Spec：项目识别与跨设备映射（`internal/project`）

对应 PRD §9.3（项目识别与路径改写）、§9.5（工作区一致性检查）、§8.3（标识不可逆）。
前置结论来自 `poc-1-claude-code-cross-device.md` 与 `poc-2-workspace-fingerprint.md`。

---

## 1. 要解决的问题

同一个项目在两台设备上的绝对路径不同，因此**本地路径不能作为跨设备标识**。这一层负责回答三个问题：

1. **这个目录是哪个项目？** —— 给出一个在所有设备上都相同的稳定标识。
2. **那个项目在本机的哪里？** —— 反向映射，供恢复时确定写入位置。
3. **本机的工作区还处在会话当时的状态吗？** —— 一致性检查（§9.5）。

这一层是**唯一知道 Git 存在的包**。`adapter` 只认识磁盘上的 Agent 数据，`remote` 只搬运不透明字节，两者都不得知道仓库、分支或提交。

### 1.1 不属于这一层的东西

* **不做路径改写。** 改写规则由 Adapter 定义并随 Adapter 版本管理（PRD §9.3）。本层只提供 `ProjectRoot`，改写由 `adapter.Canonicalizer` / `adapter.Localize` 完成。
* **不做标识的密码学处理。** 本层产出的是**明文的规范化远端地址**；`crypto.ProjectID` 才把它变成不可逆标识。明文绝不离开本机（P6）。
* **不持久化绑定关系。** 本层定义 `Binding` 的形状与查找逻辑，读写交给 `config`。这样两个包互不依赖。
* **不读会话。** 一致性检查需要的"会话动过哪些文件"由 `adapter.TouchedFiles` 产出，以 `[]string` 传入本层。**`project` 不得 import `adapter`**——两者是同级叶子。

---

## 2. 项目标识

### 2.1 优先级（PRD §9.3）

| 级别 | 依据 | 自动跨设备匹配 |
|---|---|---|
| L1 | 规范化后的 Git Remote | 是 |
| L2 | Git 仓库存在但无远端 | 否，需绑定 |
| L3 | 非 Git 目录 | 否，需绑定 |

L2 与 L3 都返回"没有稳定标识"，**而不是返回错误**。没有远端是一种完全正常的状态（本地实验仓库），把它报成错误会让 `doctor` 变得聒噪。CLI 据此提示用户手动绑定。

### 2.2 远端规范化

必须让 `git@github.com:user/example.git` 与 `https://github.com/user/example.git` 得到同一个标识。规则：

1. **剥离 userinfo**（`https://user:token@host/...` → `https://host/...`）。
   两个理由，任一独立成立：其一，远端 URL 里可能嵌着**访问令牌**，而这个字符串会进入错误信息与 `doctor` 输出（code_style §5.2 禁止其中出现远端地址）；其二，一台设备的 URL 带凭据、另一台不带，两者就永远匹配不上。
2. **SCP 简写展开**：`git@host:path` → `ssh://host/path`。
3. **协议丢弃**：`ssh` / `https` / `git` / `git+ssh` 一律归一，只保留 `host/path`。同一个仓库换协议不是换项目。
4. **主机名小写**，去掉端口中的默认值。DNS 大小写不敏感，这条无争议。
5. **路径**：去掉前导 `/`、尾部 `/` 与尾部 `.git`。
6. **路径大小写保持原样**。

第 6 条是一个明确的取舍。GitHub 对 owner/repo 大小写不敏感，但并非所有 Forge 如此。若统一小写，在大小写敏感的 Forge 上会把**两个不同的仓库并成一个**——别人的会话出现在你的项目里。若保持原样，`Github.com/User/Repo` 与 `github.com/user/repo` 会被当成两个项目，用户看不到自己的会话。

选择保持原样，因为：**并错了是数据混淆，没并上只是没生效**，后者有手动绑定这条明确的补救路径，前者没有。这条限制写进 §7。

### 2.3 选哪个远端

一个仓库可以有多个远端。规则：

1. 有 `origin` → 用 `origin`。
2. 无 `origin` 但只有一个远端 → 用它。
3. 无 `origin` 且有多个远端 → **不自动识别**，要求手动绑定（BR-12：无法确认安全一律中止，不猜）。

Fork 场景（`origin` 是自己的 fork、`upstream` 是上游）在两台设备上若配置不同，标识就不同。仍然选 `origin`：把 fork 当作独立项目是常见且合理的工作方式，工具不该替用户判定"其实是同一个项目"。补救路径同样是手动绑定。

### 2.4 手动绑定的标识

用户为一个目录指定一个名字，标识为 `manual:<name>`。稳定性由用户负责——**两台设备上必须输入同一个名字**。域前缀 `manual:` 保证它不可能与规范化远端相撞。

---

## 3. 反向映射

恢复时需要由标识找到本机的项目根。来源有两个：

1. **已知项目的扫描结果**：对候选目录求标识，命中即映射。
2. **用户绑定**：`Binding{Identity, LocalRoot}`，由 `config` 持久化。

绑定优先于扫描结果——用户的明示胜过工具的推断。

### 3.1 一个标识对应多个本地根

这不是异常，而是 Git 的正常用法：**同一个仓库的多个 worktree 有相同的远端**，因而标识相同、根不同。子模块则相反，各有各的远端，天然是不同项目。

处理方式：映射返回**全部**候选，由调用方决定。当候选多于一个且没有绑定时，**不得任选其一**——那会把会话恢复进用户没在看的那个 worktree。此时中止并要求绑定。

---

## 4. 一致性检查（§9.5）

结论沿用 PoC-2：**以 Git 为锚，不以会话为锚**。会话自报的文件集不足以覆盖用户在 shell 里做的改动。

三层，由粗到细：

| 层 | 内容 | 作用 |
|---|---|---|
| L1 | HEAD commit + 分支名 | 最便宜的判定；HEAD 相同且工作区干净即可直接判定一致 |
| L2 | 脏文件集（`git status --porcelain`） | 覆盖 shell 修改，这是会话看不见的部分 |
| L3 | 逐文件摘要，范围是"会话动过的 ∪ 脏的" | 区分"改了但改回来了"与"真的变了" |

判定三档：`Consistent` / `Explainable`（HEAD 前进但差异都能由提交解释）/ `Divergent`。**`Explainable` 一档也必须列出具体文件**，否则用户无从判断要不要继续。

### 4.1 必须避开的坑

`git status --porcelain` 的输出有三个陷阱，**全部指向同一种失败**：路径解析出来是错的，于是匹配不上，于是改动被静默归类为"干净"。检查照跑，结论照给，只是永远说"没问题"。一个永远说没问题的一致性检查，比没有更危险。

**坑一：`TrimSpace` 吃掉状态列**（PoC-2 实测）。`" M src/a.go"` 变成 `"M src/a.go"`，按固定偏移取路径得到 `"rc/a.go"`。

**坑二：路径会被加引号并做 C 转义**（本 Spec 实测，git 2.55）。默认 `core.quotePath=true` 下：

```
 M "with space.txt"
 M "\344\270\255\346\226\207\345\220\215.txt"
```

非 ASCII 文件名被转成 UTF-8 字节的**八进制转义**。拿这个字符串去比对真实文件名永远不相等——**所有中文、日文、带重音的文件名会被静默判为未改动**。这不是边角情况，是所有非英文用户的默认情况。

**坑三：`-z` 下重命名的两个路径顺序是反的**（本 Spec 实测）。

| 模式 | 输出 |
|---|---|
| 默认 | `R  a.txt -> renamed.txt`（旧 → 新） |
| `-z` | `R  renamed.txt` `\0` `a.txt`（**新在前**，两个 NUL 终止字段） |

**结论：一律使用 `git status --porcelain -z`。** `-z` 关闭引号与转义，输出原始字节，路径以 NUL 终止——这同时解决坑一（不需要 trim）与坑二（不需要反转义）。代价是必须自己处理坑三：状态码为 `R` 或 `C` 时要消费**两个**字段，且第一个是新路径。

三条都必须有专门测试（§8.3）。

---

## 5. 为什么调用 `git` 而不是用 Go 的 Git 库

调用外部 `git`。理由：

* 一致性检查的结论必须与**用户自己跑 `git status` 看到的**一致。第二套 Git 规则实现（sparse checkout、`.gitignore` 的层叠、`core.ignorecase`、includeIf 的 config）迟早与真实 git 分叉，而分叉的表现是误报或漏报，不是崩溃。
* 依赖数保持在 1（当前只有 `golang.org/x/crypto`）。

代价是要求 `git` 在 PATH 上。**不在时不得崩溃**，而是降级为"没有稳定标识"，并由 `doctor` 明确报出。

### 5.1 执行 `git` 的硬约束

1. **绝不执行任何触网的 git 子命令。** 不得有 `fetch`、`ls-remote`、`pull`、`push`。允许的只有 `rev-parse`、`remote get-url`、`status`、`rev-list`、`cat-file`。这是 P7 的直接推论——除用户配置的存储后端外，程序不得访问任何网络地址，而 `git fetch` 是一次实打实的出网请求。
2. **一律带 `--no-optional-locks`。** `git status` 默认会刷新并写回 `.git/index`。这是在写用户的仓库，与 code_style §3.2「不得锁定、移动或修改正在使用的文件」同理。
3. **禁用一切交互**：`GIT_TERMINAL_PROMPT=0`、`GIT_ASKPASS`/`SSH_ASKPASS` 置空、`GIT_OPTIONAL_LOCKS=0`。否则一个要求输入凭据的仓库会让进程永久挂起。
4. **必须走 `exec.CommandContext` 并有超时**（code_style §4.2、§4.3）。超大仓库上的 `git status` 是已知的主要开销来源（PoC-2 §7）。
5. **只 trim 尾部换行**，绝不 `TrimSpace`（见 §4.1）。列出路径的命令一律加 `-z`，按 NUL 切分，不做任何 trim。

---

## 6. 接口

```go
// 标识
type Identity struct {
    Kind  Kind   // KindRemote | KindManual | KindNone
    Value string // 规范化远端，或 "manual:<name>"；KindNone 时为空
}

func CanonicalizeRemote(raw string) (string, error)
func Identify(ctx context.Context, dir string) (Project, error)

type Project struct {
    Root     string   // 仓库工作树根；非 Git 时为 dir 本身
    Identity Identity
    Reason   string   // 为什么没有稳定标识，供 CLI 提示。不含远端地址
}

// 映射
type Binding struct {
    Identity string
    LocalRoot string
}
func Locate(ctx context.Context, id Identity, candidates []string, bindings []Binding) ([]string, error)

// 一致性（第二阶段）
func Capture(ctx context.Context, root string, touched []string) (Fingerprint, error)
func Compare(ctx context.Context, root string, fp Fingerprint) (Report, error)
```

`Reason` 是给用户看的，因此**不得包含远端地址、项目名或绝对路径**（code_style §5.2）。写"仓库有多个远端且没有 origin"，不写具体是哪些。

---

## 7. 边界与失败行为

| 情况 | 行为 |
|---|---|
| `git` 不在 PATH | `KindNone`，`Reason` 说明；不报错、不崩溃 |
| 目录不存在 / 无权读 | 返回错误 |
| 不是 Git 仓库 | `KindNone` |
| 是 Git 仓库但无远端 | `KindNone` |
| 多个远端且无 `origin` | `KindNone`，要求绑定 |
| 裸仓库（无工作树） | `KindNone`——没有工作区就不是项目 |
| Worktree | 正常识别；同标识多根由 §3.1 处理 |
| 子模块 | 按自己的远端识别，是独立项目 |
| 远端 URL 含凭据 | 剥离后再规范化；凭据不得进入任何输出 |
| 远端 URL 无法解析 | `KindNone`，不猜 |
| `git status` 超时 | 返回错误；**绝不因此判定"一致"** |
| 标识对应多个本地根且无绑定 | 返回全部候选并报错；**绝不任选其一** |

### 7.1 绝不能做的事

* **绝不执行触网的 git 命令。**
* **绝不写入用户仓库**——包括 `.git/index` 的顺带刷新。
* **绝不在无法确定项目时猜一个**，宁可要求手动绑定。
* **绝不在检查失败或超时时给出"一致"的结论**——一致性检查的唯一价值就是它说"一致"时可信。

### 7.2 已知限制

1. 路径大小写不同的同一仓库会被当作两个项目（§2.2），补救是手动绑定。
2. Fork 与上游被当作两个项目（§2.3）。
3. 纯格式化改动会被判为不一致（PoC-2 §7.1），属可接受误报。
4. 非 Git 项目无法使用一致性检查，只能退化到最弱的判定。

---

## 8. 测试计划

覆盖率目标 90%（testing.md §1）。夹具一律为**临时目录里现建的真实 Git 仓库**，不使用真实项目。

### 8.1 规范化表驱动测试

必须覆盖并断言等价：SCP 简写、`ssh://`、`https://`、带端口、带 userinfo、带 token、尾部 `.git`、尾部 `/`、主机大小写、非 ASCII 路径、含空格路径、无法解析的输入。

**必须有一条测试断言凭据不出现在结果与错误信息中。**

### 8.2 真实仓库上的集成测试

用 `git init` 在 `t.TempDir()` 里构造：无远端、单远端、多远端有 origin、多远端无 origin、worktree、子模块、裸仓库、非仓库目录。断言各自落到正确的 `Kind`。

### 8.3 一致性检查

* 黄金场景矩阵沿用 PoC-2 §4。
* **porcelain 解析必须有专门用例**，逐条对应 §4.1：状态列不被吃掉；含空格的路径；**非 ASCII 路径**（断言得到的是真实文件名，不是 `\344\270\255…` 这种转义串）；重命名时取到的是**新路径**；含 `->` 字面量的文件名不被误当作重命名。
* 非 ASCII 与含空格的用例必须**在真实仓库上跑**，即真的建出这些文件名。只喂造好的字符串会把 git 的实际转义行为整个绕过去——那正是坑二能藏这么久的原因。
* 超时与 `git` 缺失路径下，断言结论**不是** `Consistent`。

### 8.4 必测的失败路径

* `git` 不存在（用一个只含空 PATH 的环境）。
* 目录在检查过程中被删除。
* 仓库处于 rebase / merge 中间态。
* 一个标识两个 worktree → 断言报错而非任选。

---

## 9. 实现顺序

1. `CanonicalizeRemote` 与表驱动测试（纯函数，最容易先钉死）
2. `git` 调用封装（超时、禁交互、只 trim 尾换行）
3. `Identify` 与真实仓库集成测试
4. `Locate` 与绑定优先级
5. 一致性检查（Capture / Compare），单独一次提交

---

## 10. 未决

1. 一致性检查在超大仓库上的耗时未测（PoC-2 §7.3、§7.4）。若 `git status` 成为主要开销，考虑对 L2 加缓存，但缓存过期判定本身有风险，暂不做。
2. 是否需要为"标识只差大小写"提供诊断提示。当前不做：标识已是 HMAC，无法比较；真要做需要额外派生一个折叠大小写的次级标识，代价与收益不成正比。

---

## 11. Review 中确定的事

以下全部经实测确认（git 2.55, Windows），不是推断。

### 11.1 status 的路径相对于**仓库根**，不是相对于 cwd

在 `sub/` 下执行 `git status --porcelain -z`，输出仍是 `sub/deep/a.txt`。因此把这些路径拼到调用方传入的 `root` 上，只有当 `root` 恰好是工作树顶层时才正确。

用户把项目绑定到子目录（`Binding.LocalRoot` 是用户给的，`Locate` 原样返回）时，所有脏路径都会拼成不存在的路径 → 两侧都是 `absent` → 相等 → **判定为一致**。这正是 §4.1 说的"永远说没问题的检查"。

修法：`Capture` / `Compare` 自己解析 `--show-toplevel`，一切以顶层为基准。**不接受调用方保证 root 是顶层**——这种保证迟早有人违反，而违反的表现是静默。

### 11.2 摘要必须用 git 的内容口径，不能哈希原始字节

`core.autocrlf=true` 是 Git for Windows 的安装默认值。同一个 commit 在 Windows 检出为 CRLF、在 Linux 为 LF，**原始字节不同**：

```
working tree:  line1\r\nline2\r\n
sha256(raw):   4ad3ef64...          ← 跨平台不同
git hash-object: c0d0fb45...        ← 与索引里的 blob 完全一致
```

哈希原始字节意味着：一台机器上采的指纹拿到另一台比对，**每个文本文件都会报"已修改"**——而跨设备正是这一层存在的全部理由。

改用 `git hash-object --stdin-paths`：它应用与 git 相同的过滤器，答案等于索引中的 blob，跨平台稳定。这也是语义上更正确的定义——**如果 git 认为文件没变，那它就是没变**。

代价：`--stdin-paths` 以换行分隔，文件名含换行的路径无法通过它传递，这类路径退回原始哈希（此类文件名只能存在于 Windows 之外，因而不涉及 CRLF 问题）。

### 11.3 未跟踪目录会被折叠成一条

默认输出把整个未跟踪目录折叠为 `?? sub/untracked/`。于是目录内的所有文件被当作**一个**"directory" 值，其中的新增、修改、删除永远比对相等。必须加 `-uall` 逐个列出。

### 11.4 空仓库里 `rev-parse HEAD` 退出 128

一个刚 `git init`、已配好 origin 的项目能拿到稳定标识，但每次 `Capture` 都会失败——**新项目的第一个会话永远无法采指纹**。改用 `rev-parse --verify HEAD`，失败时 head 记为空；分支用 `git branch --show-current`（空仓库下仍能返回，且分离 HEAD 时返回空而不是字面量 "HEAD"）。

head 为空时不做祖先判定，只比对文件摘要。

### 11.5 pathspec 默认按 glob 匹配

`git diff -- 'rep[1].md'` 会匹配 `rep1.md`。若某个提交改动了 glob 邻居，真实的 divergent 会被降级成 explainable。所有 git 调用统一加 `--literal-pathspecs`：**来自工作区的路径是数据，不是模式**。

### 11.6 "没看成"不等于"不在这里"

`Locate` 的候选扫描原先对所有 `Identify` 错误一律 `continue`，于是取消或超时的扫描最终返回 `ErrProjectNotHere`——对一批根本没看成的候选下"这里没有"的断言。与 §7.1 同一条原则，改为遇到 `unanswered` 立即中止。

### 11.7 `*fs.PathError` 会把绝对路径带进用户可见的错误

`%w` 包装 `os.Stat` 的错误，消息里就有 `stat C:\Users\<用户>\projects\<项目>: ...`——同时暴露了用户是谁和在做什么，违反 BR-09。加 `pathSafe()`：剥掉路径、保留成因，`errors.Is` 仍然可用。

### 11.8 `safe.directory` 拒绝与"不是仓库"退出码相同

两者都是 128。把前者报成"这不是一个 git 仓库"会把用户推向手动绑定，而真正的修法是加一条 `safe.directory` 配置。改为检查 stderr 内部特征（**不回显其内容**）后给出可操作的提示。

### 11.9 非常规文件不得直接打开

工作区里可能存在 FIFO（构建工具会留下），`os.Open` 会阻塞到有写入方出现，且这一步没有超时。改为先 `Lstat`，非常规文件记为 `kind:irregular`——**路径的类型变了本身就是差异，不是一次失败的读取**。
