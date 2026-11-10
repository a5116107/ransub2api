# 区域调度详情与最新可用性

区域 `GetAccount`、`GetAccounts`、`GetGroupByID*` 与 OpenAI HTTP/WS 的可用性终检均由 `RegionalSchedulerReader` 读取经过认证、加密、持久化且未过期的完整快照。缺项、过期或持久化失败直接拒绝，不向主站 Redis/PostgreSQL 补全。重复账号在不同桶的凭证或策略不一致时拒绝整份快照。

调度文档 v2 同时携带账号、分组和按账号分配的最新可用性报告。发布端复用原 SQL repeatable-read 事务读取规则、线路、事件版本和检测结果；普通主站读取仍使用原独立只读事务。区域不另开数据库连接，不把缺失报告当作没有规则，也不将某条托管连接的通过扩大为整个账号健康。

HTTP 最终发送与原生 WS 每轮发送复用同一准入函数。区域先核对已构建请求的线路，再按实际模型、协议、stream、服务档和推理强度匹配有效报告。报告保留原绝对有效期，读取时计入投递和存储时间；新发布、重读与重启均不延长旧通过。暂停、故障、撤销和恢复随原后台快照传播，不承诺跨区域零延迟。

v1 文档仍可用于候选与详情恢复，缺少 v2 可用性策略时拒绝业务发送。只有候选能力的 `LocalSchedulerSnapshotStore` 不授权详情或可用性，缺少接口时拒绝且不回源。升级与回退、同步间隔及生产验收边界见[业务同步说明](regional-scheduler-sync.md)、[运行时说明](regional-scheduler-runtime.md)与[唯一设计](project/monitor-driven-scheduling-design.md)。
