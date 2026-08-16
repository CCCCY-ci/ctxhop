# MVP 端到端验收矩阵

| 范围 | 状态 |
|---|---|
| 合成 dir Remote + 生产同步链路 | ✅ |
| 设备 A durable push | ✅ |
| metadata-only foreign check | ✅ |
| stale List / 截断恢复保护 | ✅ |
| 完整 branch restore plan | ✅ |
| fork 保留与显式选择 | ✅ |
| context cancellation fail-closed | ✅ |
| 真实 Windows ↔ macOS/Linux | 🟡 环境待验收 |
| 真实 S3/兼容服务 | 🟡 环境待验收 |
| 真实 Claude Code 会话 | 🟡 依赖本机 Agent 和安全样本 |

## 1. 运行方式

在仓库根目录执行：

~~~text
go run ./poc/mvp
~~~

工具使用临时目录和生成的加密身份，只运行本地 dir Remote，不接触用户配置、真实 Agent 数据、真实凭据或网络服务。输出只包含场景名称和 PASS/FAIL，不输出会话记录、路径和密钥。

完整 Go 回归仍使用：

~~~text
go test ./...
~~~

## 2. 当前自动化场景

| 场景 | 验收内容 |
|---|---|
| device A durable push and metadata | 使用 AppendExecutor、CursorStore、分片写入和 metadata 发布 |
| metadata-only foreign-device check | 通过 FetchPullPlan 检查外部设备 tip，断言没有读取 shard body |
| stale List cannot restore a truncated branch | 隐藏最后一个 shard 的 List 结果，断言 FetchCompleteBranches 返回 incomplete |
| complete branch restore planning | 读取完整 metadata/shard，执行生产 FetchRestorePlan 和路径本地化 |
| fork is preserved and requires explicit selection | 设备 B 发布同前缀分叉，断言未选择时拒绝，显式选择后成功 |
| cancelled remote read fails closed | 取消 context 后不产生成功的短结果 |

## 3. 真实环境补充矩阵

### 3.1 跨平台

至少需要两台设备或 Windows + WSL/虚拟机组合：

- Windows ↔ macOS/Linux；
- 不同用户名和 home 目录；
- Windows 盘符、分隔符、大小写敏感目录；
- 非 ASCII、空格和长路径；
- Agent 写入期间的快照；
- 长会话、附件、plan、subagent；
- 恢复后继续 push 和重复恢复。

每个场景都要记录：Agent 版本、系统和架构、结果 verdict、记录数量、是否产生 fork，以及是否有错误输出泄露会话内容或路径。

### 3.2 Remote

按 docs/specs/poc-3-remote-consistency.md 执行：

- S3/兼容服务的多页列表、延迟、5xx、超时；
- metadata 与 shard 分阶段可见；
- dir 第三方同步工具的部分同步和断开重连；
- 重试后对象集合收敛；
- 不把一次缺失误判为永久删除或空会话。

### 3.3 安全和卸载

- 口令错误、Recovery Key 错误和远端对象损坏；
- 恢复失败不写入半个本地会话；
- 用户取消不会修改 Agent 数据；
- 删除/卸载 AgentSync 不删除 Agent 原有会话；
- 输出和错误不包含口令、Recovery Key、会话正文或完整本地路径。

## 4. 完成定义

MVP 端到端验收不能只以 poc/mvp 通过为完成。完整通过需要：

1. 合成矩阵持续通过；
2. dir 与至少一个真实 S3 兼容服务矩阵通过；
3. Windows、macOS/Linux 或 WSL 的跨设备恢复通过；
4. 失败、取消、分叉、工作区差异和重试结果都有记录；
5. 结果不包含敏感内容；
6. 验收记录中的 Agent/Remote 版本和限制同步回 TODO 与发布文档。
