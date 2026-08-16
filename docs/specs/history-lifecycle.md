# History cleanup 与 prune

## 命令

~~~text
agentsync history <session-id>
agentsync history cleanup [--yes] [--path DIR] [--remote-id] SESSION_ID
agentsync history prune [--yes] [--path DIR] [--remote-id] --keep N SESSION_ID
agentsync history prune [--yes] [--path DIR] [--remote-id] --before RFC3339 SESSION_ID
~~~

history cleanup 是按会话删除的易发现别名，语义与 agentsync remote delete-session 相同：删除该会话的全部设备分支、元数据和分片，不影响同项目的其他会话。

history prune 在删除前必须读取并验证完整的元数据和分支。它只把“版本”映射为当前解析结果中的 maximal versions，并按设备分支作为删除单位：

- --keep N 按元数据中的会话更新时间从新到旧保留 N 个 maximal versions；
- --before TIME 删除更新时间早于 TIME 的 maximal versions；
- 一个版本由多个设备分支共同提供时，只有整个版本被保留才会保留这些设备分支；
- 不在保留集合中的冗余前缀分支也会删除，因为保留的完整分支已经包含它们的记录；
- 缺少可解析更新时间的版本默认保留，避免把未知时间误判为旧数据；
- 分片仍然不可变，prune 不会改写剩余分片，也不会在没有显式命令时自动删除任何分支。

删除每个设备分支时会同时删除它的 meta 与所有 shard。操作按分支顺序执行；中途失败会返回已删除对象数和已完成分支数，不能被误报成完整成功。--yes 只跳过确认，不跳过 keyfile 身份校验、完整性检查或口令输入。
