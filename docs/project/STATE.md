# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D3/D9流内失败漏计及初始发送前失效重选已修复，准备.3发行。
授权：修复并发布上线；熔断5分钟3次、暂停1分钟；不CI、不改人工停用/分组/成本。
源码：production@0f5e31fb2；.3改动未提交；既有service/data保留。
验证：原红测8例失败；三包race通过；四种流路径12场景race通过，静态检查中。
事实：线上仍.2/4f3010130ddf；账号10人工停用保留；流内失败曾漏计。
下一：收取静态结果、提交推送、本地构建/自有发行、副本回退演练、排空上线及实测。
尝试：67
阻塞：原28项20done/8部分；上游v2.21.1待冲突整合；历史拒删资源不重试。
边界：PG/Redis不变；5/3/1开启；优先调度仍false，旧程序与备份保留。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-13。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 保留自研与自有渠道；本轮授权最新版本合并/发行/上线，不CI；真实采购价不做。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | D2.1–D2.4、多出站/持久交接、三节点及视频身份/双预算/原节点结算/竞争、336排空/跨库及原节点期限恢复已验；unknown待原请求关联契约 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 状态/文本准入及带内/空流/终态/间隔超时已验；.2跨请求保护5/3/1上线，race/UI/公网SSE账单通过；其他能力待验 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 6项未引用辅助符号与死计数器删除，unused 6→0 |
| M2 | done | 历史247项与integration101诊断清零，123处断言已核；四包lint零诊断、240顶层race通过，指纹/时钟夹具已验 |
| M3 | done | WinSock拒绝/不可达识别；超时/重置不误判；真实POST及代理/WS组合5轮通过 |
| M4 | done | 三处原子持久化收敛；22项Windows失败修复，双平台回归/三包lint/vet通过 |
| M5 | done | 生产依赖与测试夹具边界明确，三处兼容点说明；六包lint 0及禁止依赖反向验证通过 |
| M6 | done | 删除无调用且假成功的 ProxyService.TestConnection；引用搜索0、gofmt、service编译及vet通过 |
| M7 | done | 删除无调用且假成功的 AccountService.TestCredentials；引用搜索0、gofmt、service编译及vet通过 |
| M8 | done | 分组统计移除 mock 零值；API Key 总数/活跃数分页准确，历史请求和标准成本复用 dashboard 数据，handler race/vet通过 |
| M9 | done | 兑换码统计移除 mock/TODO；真实分页统计状态、类型和已使用面值，admin race、service编译与vet通过 |
| M10 | done | 删除无调用且假成功的 AdminService.RefreshAccountCredentials；接口/实现/stub引用为0，admin race、service编译与vet通过 |
| M11 | done | 用户用量移除 mock 零值；接入既有 UsageLogRepository 聚合、周期校验和缺依赖失败关闭，service/admin race、server编译与vet通过 |
| M12 | done | 代理统计与 dashboard realtime 移除固定零值/100%假成功；兼容路由明确 501，admin race/vet通过 |
| M13 | done | 删除无调用的 GroupService.GetStats 第二套统计入口；group handler race、service编译与vet通过，引用为0 |
| D4 | in_progress | 固定售价、严格请求新鲜策略、7平台及BPS独立发送权威终检已验证；仍依赖D2、区域传播和模型/fast来源 |
| D5 | in_progress | 协议/缓存/TPS/TTFT及入站/线路/effort隔离已验；选定延迟未知/null已验，service662/3193、四包lint0/vet/Linux通过；跨协议/会话估计待证据 |
| D6 | in_progress | 普通保留/硬准入及门票16场景已验，扩大race668/3225、四包vet/lint0及Linux通过；软迁移仍须前缀/候选缓存域与净收益证据 |
| D7 | in_progress | 并发保存/409、重算及候选缺失/无效延迟已验；UI112+支付21、类型/lint/构建/中英390px刷新通过；完整解释待契约 |
| D8 | in_progress | CLI/额度与统一性能已验；生产副本410→432四阶段、旧程序回退及重升通过；并行混版及其他能力待验 |
| D9 | in_progress | .2自有发行/排空/重建/加载哈希、设置持久化、公网SSE账单和桌面/手机保存已验；完整D8及供应商能力仍待 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为a5116107/ransub2api，upstream为ranxi2001/sub2api且禁推。
- 正式源码根153包已验；unit/integration并发1–2，沿用go.mod/lint规则。Redis竞争用既有5.0.14.1，Redis3缺Lua方法；历史失败不改报成功。
- 图片夹具隔离DATA_DIR；12张历史PNG/1164bytes保留。协议/区域/CLI的实际覆盖与复现命令见DECISIONS，不把本地夹具当真实供应商。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 下一项工作

原28项20 done、8部分（D2–D9）。生产.2绑定cc747fb6a，熔断5/3/1已启用；本轮修复发布子项完成。上游v2.21.1在[PR#3](https://github.com/a5116107/ransub2api/pull/3)待冲突整合，不称已集成最新上游；原外部验收保留。

## 外部等待与清理

- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9发布子项已完成；并行混版、非文本能力及缓存迁移净收益仍按原D8依赖验收。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删资源的完整路径/大小及证据见DECISIONS；删除0，拥有者手工处理，不重试/移动。
- `H:/Root/DevCache/tmp/sub2api-candidate-latency`（239文件/9578704bytes）清理被策略拦截、删除0；拥有者手工处理，不重试/移动，最小证据已保留。
- 本轮`H:/Root/DevCache/tmp/sub2api-v221-upgrade/candidate`及两pyc（183686813bytes）清理被策略拦截，删除0；由拥有者手工清理，详见DECISIONS。
- 本地旧候选`H:/Root/DevCache/releases/ransub2api/2.21.0-ransub.1`删除/归整被拦截、未执行；正式包在同级`2.21.0-ransub.1-final`，由拥有者处理旧候选，不重试。
- 本轮`backend/tmp/protocol-cache/ransub-v221-ops.py`及其同目录pyc共60880bytes删除被拦截、删除0；由拥有者处理，不重试。
