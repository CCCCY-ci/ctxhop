# 远端生命周期命令

## 目标

远端删除是不可逆操作，命令必须先把删除范围转换为 AgentSync 自己生成的对象前缀，再交给 Remote。CLI 不接受任意前缀，因此不会因为路径拼接或相似 ID 删除相邻项目、会话或 keyfile。

## 命令

在已经初始化并且当前配置仍能验证远端 keyfile 的设备上：

~~~text
agentsync remote delete-session [--yes] [--path DIR] SESSION_ID
agentsync remote delete-session [--yes] [--remote-id] [--path DIR] REMOTE_SESSION_ID
agentsync remote delete-project [--yes] [--path DIR]
agentsync remote delete-all [--yes]
~~~

- delete-session 默认把 Agent 原生的 session ID 与当前稳定项目身份通过本地 Identifier Key 派生为不透明 remote session ID。需要直接操作已知不透明 ID 时使用 --remote-id。
- delete-project 从 --path 指向的当前项目生成项目 ID；默认路径为当前目录。没有稳定项目身份时命令拒绝执行。
- delete-all 清空配置 Remote 中可见的所有合法对象，并且包含 v1/keyfile 与设备记录。它不需要项目路径，但仍会先验证配置与远端 keyfile，避免把错误的 backend 配置当成清空成功。
- 未指定 --yes 时会打印范围明确的确认提示；输入 y 或 yes 才会继续。--yes 只跳过交互确认，不放宽 ID 校验或远端身份校验。

## 删除边界

| 命令 | 删除范围 | 保留内容 |
|---|---|---|
| delete-session | v1/projects/<project-id>/sessions/<session-id>/ 下的所有设备分支、meta 和分片 | 同项目其他会话、设备记录、全局 keyfile |
| delete-project | v1/projects/<project-id>/ 下的所有项目对象 | 其他项目、设备记录、全局 keyfile |
| delete-all | 配置 Remote 下的所有对象 | 无 |

所有前缀都带末尾分隔符，避免 p 匹配 p2、s 匹配 s2。删除前会排序并去重 List 结果；Remote 的幂等 Delete 允许重复执行。远端分页或最终一致性导致的 List 缺口不会被客户端伪装成已删除对象，若后端随后又显示对象，应重新执行相同命令。

删除过程中发生错误时，命令返回非零错误，并报告已经删除的对象数；这表示操作可能只完成了一部分，不能被当成完整成功。

## 与设备删除的区别

agentsync device remove 只删除指定设备拥有的设备记录和会话分支，不删除项目的其他设备分支、会话元数据或 keyfile。远端生命周期命令按会话、项目或整个 Remote 操作，适用于用户明确要清理历史或退出存储的场景。
