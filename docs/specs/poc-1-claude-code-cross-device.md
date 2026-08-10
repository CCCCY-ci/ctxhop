# PoC-1：Claude Code 跨路径恢复可行性验证

| | |
|---|---|
| 状态 | **通过（部分范围）** |
| 日期 | 2026-08-10 |
| 分支 | `poc/claude-code-cross-device` |
| 对应 PRD | §17 PoC-1 |
| 结论 | 产品的核心前提成立：会话可以被移动到另一个项目路径并被原生 `--resume` 正常继续 |

---

## 1. 验证了什么

PRD §17 把 PoC-1 定为"不通过产品就不成立"的门控项。要回答的问题是：

> 一个 Claude Code 会话，被搬到项目绝对路径不同的位置后，还能不能被原生 Resume 认出并延续上下文？

**答案是可以。**

---

## 2. 方法

1. 取一个真实会话副本（108 条记录，345KB），放在 `.gitignore` 覆盖的 `testdata/real/`。
2. 用 `poc/pathscan` 枚举所有与绝对路径绑定的字段（只输出字段路径与计数，不输出值）。
3. 用 `poc/rewrite` 按字段白名单改写路径，输出到新文件。
4. 把改写结果安装到 `~/.claude/projects/<目标路径 slug>/<原 session id>.jsonl`。
5. 在目标目录执行 `claude -p --resume <session-id> "..."`，要求它仅凭历史回答任务内容与当前工作目录。
6. 比对 resume 前后的文件，判断写入语义。
7. 清理全部测试痕迹。

---

## 3. 关键发现

### 3.1 项目目录名是绝对路径的编码

会话文件位于 `~/.claude/projects/<slug>/<session-id>.jsonl`，其中 slug 由项目绝对路径编码而来：

```
D:\CodeWorkSpace\VSCodeProjects\ElsevierTrackor
→ D--CodeWorkSpace-VSCodeProjects-ElsevierTrackor
```

规则：`:` 与 `\` 均替换为 `-`（因此 `D:\` 产生连续两个 `-`）。

**该 slug 不出现在文件内容中**（扫描命中数为 0）。这比预期简单：slug 纯粹是文件系统层面的问题，恢复时重命名目录即可，无需在内容里查找替换。

### 3.2 路径绑定分三类

扫描 108 条记录、156 个字段后的结果：

| 类别 | 字段 | 出现 | 处理 |
|---|---|---|---|
| **① 项目路径** | `cwd` | 79 | 改写 |
| | `message.content[].input.file_path` | 5 | 改写 |
| | `toolUseResult.file.filePath` | 3 | 改写 |
| | `toolUseResult.filePath` | 2 | 改写 |
| | `attachment.planFilePath` | 1 | 改写 |
| | `trackingPath` | 1 | 改写 |
| **② Agent 数据目录路径** | `backup.realParentDir` | 2 | 改写 |
| | `snapshot.trackedFileBackups` 的 **key** | 1 | 改写 |
| **③ 自由文本中的路径** | `message.content[].content` | 2 | **不改写** |
| | `toolUseResult.stderr` | 2 | **不改写** |
| | `toolUseResult.stdout` | — | **不改写** |

本次共替换 94 处。

### 3.3 路径也出现在 JSON 的 key 里

`snapshot.trackedFileBackups` 是一个以绝对路径为键的对象。只改写 value 的实现会漏掉它。改写器必须同时处理 map key。

### 3.4 第 ③ 类不改写，验证下来没有造成困惑

自由文本（对话正文、stdout、stderr）中嵌有源设备路径。本次**刻意不改写**，理由是：这些内容是"在另一台机器上发生过什么"的历史记录，改写它等于篡改对话语义；且字段最大 50KB，混杂文件内容与命令输出，误改风险高。

实测中 Agent 正确报告了自己位于**新路径**，没有被历史文本里的旧路径带偏。

该结论的置信度有限，样本只有一个会话。若后续出现 Agent 依据历史文本中的旧路径去访问文件，需要重新评估。§9.5 的工作区一致性检查是这一风险的兜底机制。

### 3.5 Resume 是纯追加，历史不被改写

Resume 前 108 条记录，之后 119 条，**同一文件、同一 session id**，且前 200KB 字节完全一致。

这从经验上验证了 PRD §9.6 的版本模型：会话是 append-only 日志，因此"本地是远端的严格前缀 → 快进，否则分叉"的判定规则是成立的，不需要引入额外的版本号机制。

### 3.6 Session ID 可跨位置保留

改写后沿用原 session id，Resume 正常。跨设备同步不需要重新分配 ID。

### 3.7 Resume 会创建 `memory/` 子目录

Resume 后项目目录下出现了空的 `memory/`。本次为空，用途未确认。属于会话相邻状态，需要在 Adapter 中持续观察其是否会承载必须同步的内容。

---

## 4. 验证结果

在目标目录执行：

```bash
claude -p --resume <session-id> "…仅凭本次会话历史回答：我们在做什么任务，你当前所在的项目目录是什么？"
```

Agent 正确回忆起原任务内容，并报告当前工作目录为**改写后的新路径**。

即：**会话被识别、上下文完整延续、路径改写生效**。

---

## 5. 尚未验证的部分

本次是同机、同用户、同操作系统的跨路径验证。以下仍需在拿到第二台设备后补验：

1. **跨操作系统**（Windows ↔ macOS）：分隔符、盘符到 POSIX 根的映射尚未实测。
2. **跨用户名**：第 ② 类路径含 `C:\Users\<name>\.claude`，本次源与目标相同，等于没有真正改写。跨设备时用户名必然不同。
3. **大小写敏感性**：Windows 不敏感、macOS 默认不敏感但可配置、Linux 敏感。
4. **非 ASCII 与含空格路径**：slug 编码规则在这类路径下的行为未知。
5. **Agent 运行期间读取**：本次读的是静态副本，未验证会话正在被写入时的读取安全性。
6. **更长、更复杂的会话**：单一样本，且未覆盖含 subagent、plan mode、大量附件的会话。

---

## 6. 对 PRD 的影响

| 条款 | 影响 |
|---|---|
| §9.3 路径改写 | 需补充：slug 编码规则、**map key 也需改写**、三类路径的分类处理策略 |
| §9.6 版本关系 | append-only 假设已验证，无需修改 |
| §11.3 目标体验 | `agentsync resume` → `claude --resume` 两条命令的路径已确认可达 |
| §17 PoC-1 | 主要问题已回答；剩余项转为"拿到第二台设备后补验" |

---

## 7. 下一步

1. 补充 PRD §9.3 的改写规则细节。
2. 将白名单改写逻辑从 `poc/rewrite` 沉淀为 `internal/adapter` 的 Claude Code 实现，并配黄金文件测试（testing.md §3.2）。
3. 构造合成会话作为测试夹具——真实会话永不入库。
4. 开始 PoC-2（工作区一致性指纹）。
5. 拿到第二台设备后补验第 5 节列出的项目。
