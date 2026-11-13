# 项目状态

## 恢复检查点

<!-- resume:start -->
Task: D8/v2.21.1来源冲突整合及嵌套身份分进程验收完成。
Scope: 单功能分支/单PR本地实施验证及自有分支同步；不CI、不改生产。
Source: 7054ef6c3已同步origin功能分支；保留90754b9aa并合入db1c6ecbb/11f250894。
Verified: 前端714/类型/lint/构建/UI输入未变；SQL96及428→431三阶段通过。
Verified: 增量lint/errcheck 0；BPS/支付双轮1966、真实PG风控98、断言80通过。
Verified: POSIX权限/清理5例双轮通过（Go1.26.6）。
Verified: 归属634/身份叶10通过；嵌套身份280场景双轮560叶通过，29进程全exit0。
Verified: 后端160/前端71输入匹配；fast lint 0 issues/vet exit0。
Next: 接入方补实际scope/独立基线及首批非文本契约后，恢复D2–D9相应依赖验收。
Attempt: 4
Blocker: 供应商事实/独立基线；混版待前置验收；拒删文件由拥有者清理，不重试。
Production: 本轮未连接/变更；原.3/5/3/1/优先调度false记录保留。
Remaining: 原28项20done/8部分；7子项6done/1部分，供应商/生产未关闭。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-14。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 保留自研/自有渠道；本轮仅单PR本地实施验证，不发布/CI/真实采购价。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | D2.1–D2.5及多出站/持久交接/三节点/视频/排空恢复已验，细节见DECISIONS；unknown仍待原请求关联契约 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D2.5 | done | HTTP/WS最终凭据、重试及快照/观察员隔离已验；实际组/缓存域仍待证明 |
| D3 | in_progress | 状态/文本准入及空流/终态/间隔超时已验；.3漏计/初始重选与5/3/1上线，race/副本/公网SSE账单通过；其他能力待验 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 未用符号/死计数删除，unused 6→0 |
| M2 | done | 历史247项/integration101诊断清零，123处断言已核；四包lint及race通过 |
| M3 | done | WinSock拒绝/不可达识别，无超时/重置误判；真实POST/代理/WS组合5轮通过 |
| M4 | done | 三处原子持久化统一，22项Windows失败修复；双平台回归/三包lint/vet通过 |
| M5 | done | 生产/夹具依赖边界及三处兼容点明确；六包lint 0与禁止依赖反向验证通过 |
| M6 | done | 删假成功ProxyService.TestConnection；引用0、编译/vet通过 |
| M7 | done | 删假成功AccountService.TestCredentials；引用0、编译/vet通过 |
| M8 | done | 分组统计接真实Key分页/活跃量和dashboard请求/成本；handler race/vet通过 |
| M9 | done | 兑换码真实分页状态/类型/已用面值；admin race、service编译/vet通过 |
| M10 | done | 删假成功AdminService.RefreshAccountCredentials；接口/stub引用0，admin race、编译/vet通过 |
| M11 | done | 用量接UsageLogRepository聚合、周期校验/缺依赖关闭；service/admin race、server编译/vet通过 |
| M12 | done | 删代理/dashboard假零值和100%；兼容路由501，admin race/vet通过 |
| M13 | done | 删GroupService.GetStats重复入口；引用0，handler race、service编译/vet通过 |
| D4 | in_progress | 固定售价、严格请求新鲜策略、7平台及BPS独立发送权威终检已验证；仍依赖D2、区域传播和模型/fast来源 |
| D5 | in_progress | 下游原始聚合见D5.2；协议/同层计时及scope隔离已验，供应商scope/会话估计待证据 |
| D6 | in_progress | 健康保留/硬准入及门票已验；软迁移仍须前缀/候选缓存域与净收益证据；历史race668/3225等见DECISIONS |
| D7 | in_progress | D5.2下游原始面板及并发保存/409已验；完整解释及供应商真实scope待合同 |
| D5.1/D6.1 | done | 上下游及账单合同；同run粘性/跨run隔离、Messages独立前缀，race40与vet通过 |
| D5.2 | done | 原始终态摘要、唯一逻辑计数/覆盖、本人筛选、30天边界及中英390px通过；非生产 |
| D7.1/D7.2 | done | 结论/覆盖/账号一致性、计费标签/开关/CSV；148前端与末改53复验、类型/lint/构建及中英390px通过 |
| D3.1 | done | 本地原HTTP/Redis适配器链5类失效切换/恢复不抢回及失败关闭；race106/1838、独立Key11通过，非生产证据 |
| D7.3 | in_progress | 三页统一历史结论/覆盖/年龄/身份未知；87项/类型/lint/构建/中英390px已验；分维度/基线待接入方 |
| D8 | in_progress | v2.21.1来源整合/SQL及428→431通过；280嵌套身份场景双轮全覆盖；旧副本410→432四阶段保留；并行混版及其他能力待验 |
| D9 | in_progress | .3自有发行/排空/重建/加载哈希及公网SSE唯一账单已验；原设置持久化/桌面/手机保存保持；完整D8及供应商能力仍待 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为a5116107/ransub2api，upstream为ranxi2001/sub2api且禁推。
- 正式源码153包历史已验；测试并发1–2。Redis沿用5.0.14.1；Redis3缺Lua，失败记录保留。
- 夹具隔离DATA_DIR；历史12PNG/1164bytes保留；协议/区域/CLI命令见DECISIONS，非真实供应商证据。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 外部等待与清理

- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9发布子项已完成；并行混版、非文本能力及缓存迁移净收益仍按原D8依赖验收。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删候选/overlay/pyc/脚本及stream-health保留，详情见DECISIONS；拥有者清理，不重试/移动；.1包保留。
- 历史`H:/Root/DevCache/tmp/sub2api-cache-conclusions`拒删，256文件9744426bytes、删除0；拥有者清理，不重试/移动。
- 历史大包lint限制保留，既有局部结果不冒充整合后全仓通过。
- 本轮browser-check.mjs清理被blocked by policy拦截，删除0；位于H:/Root/DevCache/tmp/sub2api-v2211-verify，8305bytes，由拥有者手工清理，不重试/移动。
- 同目录handler-identity.test.exe共342989777bytes，删除命令同样启动前被拒；无活跃测试进程，删除0，由拥有者清理，不重试/移动。
