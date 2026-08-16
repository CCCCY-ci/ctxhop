# Spec：配置与凭据（`internal/config`）

对应 PRD §9.1（初始化与配置）、§11（doctor）、BR-07、BR-09、BR-12。
密钥模型见 `crypto-spec.md §3`——**本 Spec 的形状是被它决定的**。

---

## 1. 要解决的问题

`agentsync init` 之后，程序要能在**无人值守**的情况下再次找到：后端在哪、凭据是什么、往哪个公钥加密、这台设备叫什么、哪些项目要同步。

这一层只做**读写与校验**，不做任何决策。它不知道会话、不知道 Git、不知道加密算法——它搬运的是结构化的设置。

### 1.1 为什么这一层要单独存在

因为它是**唯一决定"什么东西落在磁盘上"的地方**。凭据、设备标识、pin 住的公钥都在这里，写错的后果是泄露或锁死，所以它必须集中，不能散落在各个命令里。

### 1.2 不属于这一层的东西

* **不做后端连通性检查。** `init` 要求配置完成后立即自检（PRD §9.1），但那是 `remote` 的能力，由 CLI 编排。
* **不定义 `Binding` 的语义。** 类型由 `project` 定义（见 `project-layer-spec.md §1.1`），本层只负责持久化，两个包互不依赖。
* **不解锁 keyfile。** 口令永远不落盘，本层也从不接触它。

---

## 2. 磁盘布局

```
<config dir>/
  config.json     0600   设置。不含任何秘密
  secrets         0600   凭据与标识密钥。加密
  device.key      0600   解开 secrets 的设备密钥
```

配置目录按平台惯例：

| 平台 | 位置 |
|---|---|
| Windows | `%APPDATA%\agentsync` |
| macOS | `~/Library/Application Support/agentsync` |
| Linux/其他 | `$XDG_CONFIG_HOME/agentsync`，回退到 `~/.config/agentsync` |

`AGENTSYNC_CONFIG_DIR` 覆盖以上全部。这不只是为了测试——**Agent 的 hook 继承的环境可能与用户 shell 不同**，需要一个明确的出口（参考 adapter 层踩过的 `CLAUDE_CONFIG_DIR`）。

### 2.1 config.json

```json
{
  "version": 1,
  "device": { "id": "…", "name": "workstation" },
  "remote": { "type": "s3", "endpoint": "…", "bucket": "…", "region": "…", "prefix": "…" },
  "identityPublic": "<pin 住的 X25519 公钥>",
  "projects": { "bindings": [...], "excluded": [...], "pushOnly": [...] },
  "agents": { "claudeCode": { "hookInstalled": true } }
}
```

**这个文件里没有秘密**，但它**不是**可以随便贴出去的：`bindings` 含本机绝对路径，`remote` 含 bucket 名。可以贴出去的是 `doctor` 的输出，那是另一套东西（BR-09）。

### 2.2 secrets

只有两样东西：

| 内容 | 为什么必须能无人值守地读到 |
|---|---|
| 后端凭据 | 不然 hook 连不上存储 |
| `idKey` | 不然 hook 算不出远端路径 |

**私钥不在这里，也永远不会在这里。** 推送只需要公钥（`crypto-spec §3.3`）。这正是这一层敢把东西写到磁盘上的原因——**这两样东西都解不开任何会话内容**。

`idKey` 泄露的后果是：拿到你磁盘的人能确认你的桶里有哪些项目。他已经拿到你的磁盘了，所以他本来就知道。

### 2.3 加密 secrets 到底防什么

诚实地讲清楚，比含糊地宣称"已加密"重要。

**防**：这个文件被单独复制、贴进 issue、进入某个只覆盖部分目录的备份。这是真实且常见的——用户排查问题时会贴配置。

**不防**：能读你 home 目录的人。密钥就在旁边的 `device.key` 里，同样的权限。

这与 PRD §12 的威胁模型一致——**"本机已被攻陷"本就列在不防护项里**。文档与 `doctor` 必须这样表述，不得暗示更强的保证。

> 不做 DPAPI / Keychain / libsecret。它们防的是本 Spec 明确不防的威胁，代价是三套平台特定代码、其中两套在本机无法测试，且 macOS 的 `security` CLI 会把密钥放进进程参数——那恰好违反 code_style §5.3。

### 2.4 环境变量

`AGENTSYNC_ACCESS_KEY_ID` / `AGENTSYNC_SECRET_ACCESS_KEY` / `AGENTSYNC_SESSION_TOKEN` 覆盖 `secrets` 中的凭据。

**环境变量提供的凭据永不落盘**（PRD §9.1）。这是给 CI 和不愿落盘的用户的（§9.1），也是给"临时用另一套凭据跑一次"的人的。

---

## 3. 接口

```go
type Config struct {
    Version        int
    Device         Device
    Remote         Remote
    DomainFingerprint string           // init 固化的非秘密同步域指纹
    IdentityPublic []byte              // pin 住的公钥
    Projects       Projects
    Agents         map[string]AgentState
}

func Dir() (string, error)                 // 平台惯例 + 环境变量覆盖
func Load(dir string) (*Config, error)
func (c *Config) Save(dir string) error    // 原子

type Secrets struct {
    Credentials Credentials
    IdentifierKey []byte
}

func LoadSecrets(dir string) (*Secrets, error)   // 环境变量优先
func SaveSecrets(dir string, s *Secrets) error   // 原子，0600

func (c *Config) Redacted() Config          // 供 doctor 使用
```

`Redacted()` 去掉 bucket 名、endpoint 主机、绝对路径与项目名，只留下结构与状态。**doctor 只能拿到它，拿不到原件**——把"记得脱敏"变成"想泄露也拿不到"。

---

## 4. 失败与边界

| 情况 | 行为 |
|---|---|
| 配置目录不存在 | `Load` 返回明确的"尚未初始化"，**不自动创建** |
| config.json 解析失败 | **报错中止**，绝不重建 |
| version 高于本 build | 拒绝并提示升级，**绝不按低版本解释** |
| secrets 存在但 device.key 丢失 | 报错说明凭据无法解开，指向重新 `init` 后端凭据（**不会影响已加密的会话**） |
| device.key 存在但 secrets 丢失 | 同上 |
| 环境变量只提供了一半凭据 | 报错，**绝不与磁盘上的凭据混用**——半套凭据比没有更难排查 |
| 生成 device.key 的随机源失败 | 报错并中止，**不得发布半成品 device.key** |
| 写入过程中断电 | 原子替换，只可能留下旧的或新的 |
| 两个进程同时写 | 后写者胜。绑定丢失是可察觉且可重做的，不做锁 |
| Windows 上 0600 不生效 | 记录在案；不假装权限位在所有平台等效 |

### 4.1 绝不能做的事

* **绝不在解析失败时重建 config.json。** 用户的绑定、排除项、设备名都在里面，"帮你恢复默认"等于**静默删除他配置过的一切**（BR-12）。
* **绝不把任何秘密写进 config.json。** 必须有测试直接断言文件内容里不含凭据。
* **绝不把凭据放进进程参数或日志**（code_style §5.3）。
* **绝不在 `Redacted()` 里保留可反推的东西**——bucket 名会暴露公司名，绝对路径会暴露用户名。

### 4.2 版本策略

与对象格式同一条规则（`crypto-spec §9`）：**只有更高的版本号才是"太新"**，其余是损坏。方向搞反会让用户去追一个不存在的版本。

---

## 5. 测试计划

覆盖率目标 90%（testing.md §1）。全部用 `t.TempDir()`，凭据一律用虚构值。

### 5.1 必测

* **秘密不出现在 config.json**：写入含凭据的完整配置后，直接读文件字节断言不含凭据字符串。这是本层最重要的一条。
* **`Redacted()` 不泄露**：断言输出里没有 bucket 名、endpoint、绝对路径、项目名、凭据。
* 往返：`Load(Save(c)) == c`，含非 ASCII 设备名与含空格的路径。
* **解析失败不重建**：写入损坏的 config.json → `Load` 报错 → **断言文件内容未被改动**。
* 版本：高版本拒绝且提示升级；未知低版本报损坏，不按当前版本解释。
* 环境变量：覆盖磁盘凭据；只提供一半时报错；提供后**断言磁盘上没有被写入**。
* 权限：POSIX 上断言 0600；Windows 上跳过并说明。
* 中断：写入中途失败 → 断言旧内容完好（用 `atomicfile` 已有的注入方式）。
* device.key 与 secrets 各自缺失时的两条错误路径。
* 设备密钥随机源失败：断言返回错误且 device.key 不存在或仍保持旧内容。

### 5.2 不测

不测"加密强度"——`crypto` 已经测过。本层只断言**写出去的字节里没有明文凭据**。

---

## 6. 实现顺序

1. `Dir()` 与平台惯例（纯函数，先钉死）
2. `Config` 的序列化、版本校验、原子写
3. `Redacted()`
4. `Secrets` 的加解密与环境变量优先级
5. 失败路径

---

## 7. 未决

1. 多进程写配置目前是后写者胜。若将来 `watch` 模式常驻，需要重新评估。
2. Windows 上的权限位不等效。若将来把威胁模型扩大到"同机其他用户"，这里需要 ACL，而不是 `chmod`。
