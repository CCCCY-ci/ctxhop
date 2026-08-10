# 测试规范（AgentSync）

> [!IMPORTANT]
> **测试代码必须提交到 Git。**
> 这是本项目与其他项目规则的一处明确差异。原因：
> 1. 这是 Apache-2.0 公开仓库，外部贡献者必须能验证自己的改动；
> 2. PRD §18 要求跨平台矩阵的集成测试自动化，测试不进仓库就没有 CI；
> 3. 本项目最大的风险是写坏用户的 Agent 数据，测试是唯一的防线。

---

## 1. 覆盖率要求

| 范围 | 要求 |
|---|---|
| `internal/adapter`、`internal/crypto`、`internal/syncer` | **95%+**。这三个包直接决定数据是否会被写坏或泄露 |
| `internal/remote`、`internal/project`、`internal/config` | 90%+ |
| `cmd/agentsync` | 不设硬性要求，但每个命令至少有一个端到端用例 |

```bash
go test -race -cover ./...
go test -coverprofile=coverage.out ./... && go tool cover -html=coverage.out
```

**`-race` 是必须的**，不是可选项。

### 1.1 `-race` 的工具链前提

`-race` 需要 cgo，cgo 需要本机有 C 编译器。开发机上已安装 MinGW-w64 GCC（WinLibs，POSIX 线程 + UCRT），通过 `winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT` 获得，已在用户级 PATH 中。

若 `go test -race` 报 `cgo: C compiler "gcc" not found`，说明 shell 继承的是安装前的旧环境，重开终端即可。

注意这与 `CGO_ENABLED=0` 不冲突：那条约束针对**发布的二进制**（保证静态链接与一条命令交叉编译），不针对测试运行。发布产物仍必须以 `CGO_ENABLED=0` 构建。

---

## 2. 测试数据：绝对禁止使用真实会话

> [!WARNING]
> **不得把任何真实的 Agent 会话文件放进 `testdata/`，即使是自己的。**
> 会话正文可能包含源代码、终端输出、内网地址、API Key。一旦提交进公开仓库，就是永久泄露。

规则：

1. `testdata/` 中的会话必须是**手工构造的合成数据**，结构真实、内容虚构。
2. 需要基于真实会话调查结构时，工作副本放在 `.gitignore` 覆盖的 `/testdata/real/` 或临时目录，**永不提交**。
3. 提交前检查：`git diff --cached` 里不得出现真实路径、真实项目名、真实对话内容。

---

## 3. 测试类型

### 3.1 单元测试

标准库 `testing`；断言可用 `stretchr/testify`。外部依赖通过接口注入假实现，不 mock 文件系统本身——用真实的临时目录（`t.TempDir()`）。

### 3.2 黄金文件测试（路径改写）

跨设备路径改写（§9.3）是最容易悄悄出错的部分，必须用黄金文件覆盖：

* 输入：某平台上的合成会话 + 源/目标项目路径
* 输出：改写后的会话，与 `testdata/golden/` 比对
* 必须覆盖：Windows↔POSIX 分隔符、盘符、大小写差异、含空格路径、非 ASCII 路径、项目外的绝对路径（必须保持原样不被改写）

### 3.3 集成测试

用 `dir` 后端（本地目录）作为测试基础设施，模拟完整的多设备流程：

* 设备 A 推送 → 设备 B 拉取 → 校验内容一致
* 两设备并发从同一基点继续 → 校验自动 Fork 且两侧内容都在
* 设备 B 落后 → 校验快进而非分叉
* 中途中断 → 校验 Agent 目录没有半写入状态

### 3.4 属性/模糊测试

* 会话解析器必须有 `go-fuzz` 风格的 `FuzzXxx`，保证任意畸形输入不 panic、不写盘。
* 加解密往返：`Decrypt(Encrypt(x)) == x`，且密文被篡改一位必须解密失败。

---

## 4. 必测的边界场景

每个功能的测试必须显式覆盖以下类别（源自 PRD §13）：

* **中断**：写入过程中进程被杀 → 不得留下半写入状态
* **并发**：Agent 正在写会话时读取 → 不得读到截断记录
* **后端故障**：不可达、超时、凭据失效、权限不足、对象被外部删除
* **数据异常**：会话损坏、结构无法解析、版本未知
* **环境差异**：项目不存在、Git 无远端、路径含特殊字符、大小写冲突
* **拒绝写入**：兼容性降级到 `CompatStopped` 时，必须验证**没有发生任何写入**

---

## 5. `.gitignore` 中与测试相关的条目

只忽略**产物**，不忽略测试代码：

```text
*.test          # 编译出的测试二进制
*.out           # 覆盖率数据
coverage.html
/testdata/real/ # 真实会话的本地工作副本，永不提交
```

`*_test.go`、`testdata/`（合成部分）、`golden/` **必须提交**。
