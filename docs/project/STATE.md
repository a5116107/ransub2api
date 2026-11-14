# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D2/D4 New API的null不再误报免费；预览/探测/SQL恢复已验。
授权：单分支本地修复/验证/提交/自有分支同步；不CI/生产。
验证命令：go test -json -race -vet=off -tags unit,integration -p 1 ./internal/service ./internal/repository -run ^TestNewAPI -count=2 -timeout=6m
验证结果：135叶双轮270次通过，fail/skip0；两包vet/增量fast lint0，UI17通过。
验证来源：573b870b3基线；newapi-null-rate-final；15项输入匹配。
下一动作：补真实scope/独立参考及缓存域证据，续验原D2–D9。
尝试：1
阻塞：供应商scope/独立基线、严格混版及受限清理。
责任方：接入方、部署及宿主资源拥有者。
解除条件：提供真实scope/独立参考；统一节点版本；拥有者清理。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-14。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 保留自研/自有渠道；本轮仅单PR本地实施验证，不发布/CI/真实采购价。
- 原28项20done/8部分，7子项6done/1部分；外部验收不因本地子修复关闭。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | D2.1–D2.5及多出站/持久交接/三节点/视频/排空恢复已验，细节见DECISIONS；unknown仍待原请求关联契约 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API固定组及null/明确0区分、预览/探测同源；SQL旧证据失效/恢复、绑定/成本保持与回归通过 |
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
| M8 | done | 真实Key分页/用量聚合；handler race/vet通过 |
| M9 | done | 真实兑换码分页/面值；admin race及编译/vet通过 |
| M10 | done | 删假成功AdminService.RefreshAccountCredentials；接口/stub引用0，admin race、编译/vet通过 |
| M11 | done | 原用量聚合/周期校验/缺依赖关闭；service/admin race及编译/vet通过 |
| M12 | done | 代理/dashboard兼容接口501；admin race/vet通过 |
| M13 | done | 删GroupService.GetStats重复入口；引用0，handler race、service编译/vet通过 |
| D4 | in_progress | 固定售价、严格请求新鲜策略、7平台及BPS独立发送权威终检已验证；仍依赖D2、区域传播和模型/fast来源 |
| D5 | in_progress | 下游原始聚合见D5.2；协议/同层计时及scope隔离已验，供应商scope/会话估计待证据 |
| D6 | in_progress | 健康保留/硬准入及门票已验；软迁移仍须前缀/候选缓存域与净收益证据；历史race668/3225等见DECISIONS |
| D7 | in_progress | D5.2下游原始面板及并发保存/409已验；完整解释及供应商真实scope待合同 |
| D5.1/D6.1 | done | 上下游及账单合同；同run粘性/跨run隔离、Messages独立前缀，race40与vet通过 |
| D5.2 | done | 原始终态摘要、唯一逻辑计数/覆盖、本人筛选、30天边界及中英390px通过；非生产 |
| D7.1/D7.2 | done | 结论/覆盖/账号一致性、计费标签/开关/CSV；148前端与末改53复验、类型/lint/构建及中英390px通过 |
| D3.1 | done | 本地原HTTP/Redis适配器链5类失效切换/恢复不抢回及失败关闭；race106/1838、独立Key11通过，非生产证据 |
| D7.3 | in_progress | 原入口/三页及模型类型、冻结集合、列表/详情兼容已验；188叶双轮/race/vet通过，分维度/独立基线待接入方 |
| D8 | in_progress | 两格式各100账号轮转，New API分页/等待/重建退避/到期恢复及原升级已验；严格混版/完整预算待验 |
| D9 | in_progress | .3自有发行/排空/重建/加载哈希及公网SSE唯一账单已验；原设置持久化/桌面/手机保存保持；完整D8及供应商能力仍待 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为a5116107/ransub2api，upstream为ranxi2001/sub2api且禁推。
- 正式源码153包历史已验；测试并发1–2。Redis沿用5.0.14.1；Redis3缺Lua，失败记录保留。
- 夹具隔离DATA_DIR；历史12PNG/1164bytes保留；协议/区域/CLI命令见DECISIONS，非真实供应商证据。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 外部等待与清理

- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9既有发布子项已完成；共享配置、在线失效及双候选漏收后恢复已验；旧版重订阅失败，严格混版、非文本及缓存净收益保留原D8/D9。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删候选/overlay/pyc/脚本及stream-health保留，详情见DECISIONS；拥有者清理，不重试/移动；.1包保留。
- 历史`H:/Root/DevCache/tmp/sub2api-cache-conclusions`拒删，256文件9744426bytes、删除0；拥有者清理，不重试/移动。
- 历史大包lint失败保留；本轮仅增量范围。
- 历史H:/Root/DevCache/tmp/sub2api-v2211-verify内browser-check.mjs（8305bytes）和handler-identity.test.exe（342989777bytes）删除被blocked by policy拦截，删除0；拥有者手工清理，不重试/移动。
- 历史mixed-candidate-sub2api（240521376bytes）保留于上述v2211-verify目录；原拒删不重试。
- 本轮同目录auth-recovery-sub2api（240525472bytes）及空H:/Root/DevCache/tmp/sub2api-auth-recovery-build删除被blocked by policy在启动前拦截，删除0；拥有者手工清理后解除。
- H:/Root/DevCache/tmp/下sub2api-newapi-retry及sub2api-newapi-bindings空目录均被宿主启动前以blocked by policy拒删；各0文件/0bytes、无活动引用，删除0。拥有者清理，不重试/移动。
