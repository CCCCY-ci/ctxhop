# PoC-1b：单机模拟第二台设备

| | |
|---|---|
| 状态 | **通过** |
| 日期 | 2026-08-11 |
| 分支 | `feature/adapter-detect-hooks` |
| 对应 PRD | §17 PoC-1 的遗留项 |
| 结论 | 在没有第二台机器的情况下，跨项目路径 + 跨 Agent 数据目录的恢复已端到端验证 |

---

## 1. 背景

PoC-1 用一次性脚本验证了跨路径恢复，但留下三项未验证，且都需要第二台设备：跨操作系统、跨用户名的 Agent 数据目录改写、Agent 写入期间读取。

本次解决其中第二项，并把整条链路改由**生产代码**执行。

关键发现是 **`CLAUDE_CONFIG_DIR` 能整体重定位 Agent 的数据目录**——实测在临时目录下生成了完整的 `projects`/`sessions`/`backups`。这使得"第二台设备"可以在同一台机器上被真实地模拟出来：不同的项目路径 + 不同的数据目录。

---

## 2. 方法

驱动程序 `poc/restore` 直接调用 `internal/adapter`，不再使用一次性脚本，因此验证的是将要发布的代码本身。

1. 在设备 A 的项目路径下用真实 Agent 产生会话，其中一次让它读取 Agent 数据目录下的文件，以便产生 `${AS_AGENT_HOME}` 类路径。
2. `Detect` 识别 Agent 并分级。
3. `ReadSessionFile` 安全读取。
4. 用设备 A 的 `PathSpace` 规范化，统计令牌与未知路径字段。
5. 重复规范化一次，断言字节稳定。
6. 用设备 B 的 `PathSpace` 本地化。
7. **在设备 B 的空间里重新规范化，断言与步骤 4 的结果字节完全相同。**
8. `ReplaceSession` 原子写入设备 B 的布局。
9. 再次读回，断言记录数与完整性。
10. 在目标项目下执行原生 `claude --resume`。

---

## 3. 结果

```
source agent: version="2.1.227" compatibility=CompatLimited
read: 12 records, droppedTail=false
canonicalize: 8 project tokens, 2 agent-home tokens, 0 unknown path fields
canonicalize: stable across repeated runs
localize: cwd="<TARGET_PROJECT>" in 8 records [ok]
round trip: all 12 records canonicalize identically on the target
touched files: 0 (0 written)
read back: 12 records, intact
```

原生 resume 的回答：

> Current working directory: `...\projB`

即目标项目路径。Agent 同时准确复述了本会话的历史，并正确指出另一个会话里的内容**不在**本会话中——说明它读到的确实是被搬运的这一份。

### 3.1 最重要的一条：跨设备往返后规范化字节相同

第 7 步是本次新增的断言，也是整个版本模型的前提：

```
canonicalize(localize(canonicalize(x, A), B), B) == canonicalize(x, A)
```

若不成立，前缀比对会在每条记录上都判定为分叉，快进永远不会触发。12 条真实记录全部通过，且这些记录同时包含项目路径与 Agent 数据目录路径两类改写。

### 3.2 兼容性分级在真实场景中生效了

验证期间 Agent 自行升级到 **2.1.227**（此前为 2.1.226）。分级如设计般降级为 `CompatLimited`——继续备份、恢复需确认——而不是中断。这是一次非计划的真实演练，说明"未知版本不停摆"的取舍是对的。

随后本次端到端验证即构成对 2.1.227 的验证，已加入已验证版本列表。

### 3.3 `TouchedFiles` 返回 0 是正确的

该会话唯一的文件操作指向 Agent 数据目录，位于项目之外，因此不计入工作区一致性检查的范围。

---

## 4. 尚未验证

1. **跨操作系统**（Windows ↔ POSIX）：分隔符、盘符到 POSIX 根的映射、大小写敏感性。需要 WSL 发行版或第二台机器。这是 MVP 验收标准的一部分，但不阻塞开发。
2. **跨用户名**：本次两个数据目录在同一用户下。改写逻辑走的是同一条代码路径，但真实的不同用户名未测。
3. **Agent 写入期间读取**：本次读的是已结束的会话。
4. 长会话、含 subagent 与大量附件的会话。

---

## 5. 附带确认

* `CLAUDE_CONFIG_DIR` 可用于隔离测试，全程未触碰真实数据目录之外的状态；测试产生的项目目录已清理。
* 生产路径上的 `Detect` → `ReadSessionFile` → `Canonicalizer` → `Localize` → `ReplaceSession` → 再读回，全部串通。
