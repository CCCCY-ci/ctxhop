# PoC-3：Remote 最终一致性与部分同步

| 项目 | 状态 |
|---|---|
| dir/S3 Remote 合约 | ✅ 已有统一契约测试和故障映射 |
| 连续分片缺口保护 | ✅ 已有 AssembleBranch 校验 |
| 尾部分片暂时不可见保护 | ✅ 已接入恢复路径 |
| metadata 与 shard tip 一致性 | ✅ 已实现 FetchCompleteBranches |
| dir 根目录消失/原子写入 | ✅ 已有实现和故障测试 |
| S3 分页/截断/错误映射 | ✅ 已有实现和伪 S3 测试 |
| 真实 S3 服务矩阵 | 🟡 需要真实 bucket 或兼容服务验收 |
| 第三方目录同步工具部分同步 | 🟡 需要外部工具验收 |

## 1. 目标

PoC-3 验证以下风险不会造成静默数据丢失：

- S3 或目录同步工具的 List 结果暂时落后于对象写入；
- metadata 已可见，但一个或多个 shard 尚未可见；
- shard 已可见，但 metadata 尚未发布或暂时不可见；
- List 返回部分对象、分页异常或中途失败；
- dir Remote 的根目录消失、复制中断或目录不完整；
- 同步重试不会把缺口当成永久删除，也不会把可见前缀当成完整会话恢复。

Remote 层继续保持哑管道边界：不解析会话明文，不判断版本关系。完整性判断由 syncer 使用加密后的 metadata 和 shard 完成。

## 2. 已实现的安全规则

### 2.1 连续性不是完整性的充分条件

FetchBranches 负责证明同一设备的 shard 序列从 1 开始、没有中间空洞，并验证每个 shard 的 base 和 prefixDigest。

但是，如果 List 暂时只返回前几个连续 shard，单靠这个检查仍然无法知道末尾是否还有未列出的 shard。因此恢复流程不能只使用 FetchBranches。

### 2.2 metadata tip 是完整性上界

每个设备发布的 metadata 包含：

- RecordCount；
- HeadDigest；
- 加密的会话摘要 payload。

FetchCompleteBranches 会：

1. 读取并验证每个设备的 metadata；
2. 读取并组装每个可见设备 branch；
3. 要求 metadata 和 branch 的设备集合完全一致；
4. 要求 RecordCount 和 HeadDigest 完全一致；
5. 任一集合缺失、记录数不一致或摘要不一致时返回 ErrIncompleteRemoteSession。

FetchRestorePlan 已改为调用 FetchCompleteBranches。因此设备 A 的 metadata 已显示有 100 条记录、但 List 只看见前 80 条时，不会把 80 条恢复到本地。

### 2.3 不把暂时缺失当成永久删除

以下情况均会失败并要求上层重试或提示用户稍后再试：

| 观察结果 | 处理 |
|---|---|
| metadata 有、shard branch 没有 | incomplete |
| shard 有、metadata 没有 | incomplete |
| metadata 记录数大于可见 branch | incomplete |
| metadata 摘要与 branch 摘要不同 | incomplete |
| branch 中间缺 shard | incomplete branch |
| List/读取发生传输错误 | 保留原始错误，不映射成 ErrNotFound |
| Remote 根目录消失 | 作为存储不可用报错，不当成空 Remote |

## 3. 自动化验收场景

当前仓库的本地回归覆盖以下场景：

1. dir 和伪 S3 使用同一套 Remote 合约；
2. S3 多页 List；
3. S3 截断但无 continuation token；
4. S3 500、权限错误、超时等不映射为 ErrNotFound；
5. dir 根目录消失；
6. dir 原子写入和临时文件清理；
7. shard 缺第一片、缺中间片、重复片；
8. metadata tip 完整且 shard 列表完整，恢复成功；
9. metadata tip 记录数大于 stale List 暴露的 shard，恢复失败；
10. metadata 与 shard 设备集合不一致，恢复失败；
11. context 取消不会返回一个看似完整的短结果；
12. 远端对象缺失、损坏或超限时停止恢复。

可运行的验证命令：

~~~text
go test ./internal/remote ./internal/syncer ./internal/syncflow
go test ./...
~~~

测试文件目前遵循仓库既有策略，不纳入 Git 提交；PoC 的生产保护位于 internal/syncer 和 internal/syncflow，测试夹具用于本地回归。

## 4. 仍待真实环境验证的场景

### 4.1 S3 或兼容服务

本地回归覆盖可用以下命令重复运行；它不会访问外部服务：

```bash
go test ./internal/remote -run 'TestS3|TestDir' -count=1
```

真实 S3/R2 验收使用显式开启的 `TestS3Integration`。它只写合成数据到
本次运行生成的唯一前缀，并且清理时只删除本次成功写入并记录的 key：

```bash
AGENTSYNC_S3_INTEGRATION=1 \
AGENTSYNC_S3_ENDPOINT=https://<ACCOUNT_ID>.r2.cloudflarestorage.com \
AGENTSYNC_S3_BUCKET=<BUCKET_NAME> \
AGENTSYNC_S3_REGION=auto \
AGENTSYNC_S3_ACCESS_KEY_ID=<SHORT_LIVED_KEY> \
AGENTSYNC_S3_SECRET_ACCESS_KEY=<SHORT_LIVED_SECRET> \
go test ./internal/remote -run '^TestS3Integration$' -count=1
```

`AGENTSYNC_S3_SESSION_TOKEN` 和 `AGENTSYNC_S3_PATH_STYLE=true` 只在临时凭据或
目标服务需要时设置；`AGENTSYNC_S3_INTEGRATION_PAGINATION=1` 会额外写入
1001 个合成对象验证多页列表。不要在 shell 历史、CI 日志或仓库中保存真实
凭据。

仍需要在至少一个真实 S3 兼容服务上执行：

- PutObject 后立即 ListObjectsV2；
- 多页列表和 continuation token；
- 临时 5xx、连接重置、超时；
- metadata 与 shard 分阶段写入；
- 重试后对象集合最终收敛；
- 恢复前后比较记录数、摘要和本地结果。

验证时不应使用真实会话明文或真实凭据；使用专门的测试 bucket、短期凭据和合成 canonical records。

### 4.2 第三方目录同步工具

需要选择一个支持按目录同步的工具，验证：

- shard 正在复制时目标设备是否能看到临时文件；
- 原子 rename 后目标是否只看到完整对象；
- metadata 先到、shard 后到；
- shard 先到、metadata 后到；
- 部分同步/按需下载使对象暂时不可见；
- 目标目录断开、重新连接后能否重新完成恢复；
- 不会因为一次缺失把对象删除或会话标记为永久不存在。

### 4.3 通过条件

PoC-3 完整通过需要同时满足：

- 上述真实环境场景都有结果记录；
- 任意暂时缺失都只能得到安全失败或稍后重试，不得到静默截断恢复；
- dir 与 S3 的错误分类一致；
- 重试后完整对象集合可以正常恢复；
- 记录一份真实服务、版本、配置和结果摘要，但不提交凭据、路径或会话内容。

## 5. 当前结论

代码层面的 PoC-3 基础保护已经完成，当前剩余工作是外部环境验收，而不是继续扩大 Remote 接口。下一项应把这些场景固化成可重复的 MVP 端到端脚本，并继续补齐真实跨平台恢复矩阵。
