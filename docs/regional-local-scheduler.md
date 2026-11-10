# 区域调度快照本地读取（PR 2）

本文描述底层候选专用 `LocalSchedulerSnapshotStore`。完整生产读取器的账号/分组/可用性接线见[业务同步](regional-scheduler-sync.md)与[详情读取](regional-scheduler-details.md)。

前置协议见 [区域数据面协议](regional-data-plane-contracts.md)，PR #419 已合并。

本次增加进程内 `LocalSchedulerSnapshotStore` 和 `LocalSchedulerSnapshotReader`。同步层可按 bucket 发布完整候选列表；发布时检查正版本号、有效期、账号 ID 完整性与唯一性，持锁比较版本并原子替换。较旧或相同版本、非法载荷不覆盖已接受视图。候选顺序保持，删除通过新版本完整替换表达；空候选列表是有效快照。

账号以私有 JSON 字节保存，每次读取解码独立对象，防止嵌套 Credentials、Extra、指针与切片被调用者修改。服务 Account 中不导出的运行缓存不进入快照。此方法保留现有账号 JSON 表达，但序列化存在 CPU/分配成本，尚未通过生产规模性能测试；不能据此声称提速。

`SchedulerSnapshotService.SetLocalSnapshotReader` 必须在启动服务/并发请求前调用。候选专用读取器接管 `ListSchedulableAccounts`；详情查询要求 `LocalSchedulerDetailReader`，可用性终检要求 `AccountAvailabilityReader`，接口或数据缺失时直接拒绝，不调用 Redis、数据库或发布 token。完整 `RegionalSchedulerReader` 已提供这些接口。没有配置本地读取器的服务继续走原 Redis/回源路径。

## 边界和后续依赖

- 此存储只提供候选列表，不授权凭证补全、分组读取或可用性。完整区域读取与鉴权、额度、资金协议另有实现和验收；不能仅凭候选存储开放生产入口。
- 存储不接收网络请求。发布参数必须来自同步层验证过的 #419 信封；来源认证、region 校验、最大年龄、载荷大小与平台/分组归属验证由后续同步层负责，不能将裸 Publish 当作可信网络接收接口。
- #419 的 revision 单调性按区域与 kind 定义；未来同步适配器须先验证该全局流版本，再将版本投影到更新的 bucket。全量同步中删除 bucket 需发布更高版本空列表；不能仅删除内存记录并丢弃版本下限。
- 缺失/过期始终拒绝候选读取；过期数据保留版本下限，不能用较旧发布复活。相同版本重试不延长有效期。
- 每次读检查过期，解码后再次检查有效期及取消。已经获得完整旧视图的请求允许完成本次选候选；账号最终准入还需要后续本地鉴权/状态校验。
- 当前数据和版本仅在进程内保存；重启后为空，默认拒绝。持久版本恢复、同步启动屏障和重放保护由 PR 3 提供。此 PR 不承诺重启恢复或多 Pod 一致性。
- 本地冷却、last-used、账号区域所有权与 OAuth 刷新协调尚未接入。禁止仅打开此读取器便认为已移除所有跨区依赖。

## 测试

`local_scheduler_snapshot_test.go` 覆盖嵌套数据隔离、过期边界、空候选、取消、不回源、倒退版本、非法/重复账号、失败发布保留旧视图、删除账号以及并发读取的完整性。测试使用虚构账号，不访问 Redis/PostgreSQL/真实模型。
