# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D3/D9截图流内失败漏计、发送前重选及.3上线子项已验证完成。
授权：修复/发行/上线；5分钟3次暂停1分钟；不CI，不改人工停用/组别/成本。
源码：发行5ce1d1892；production应用同源，收尾文档随同分支提交同步。
验证：三包race/vet、四流12场景、增量lint0；全包62既存；四阶段副本/线上重建/公网账单通过。
线上：2.21.0-ransub.3，程序ceaef043e52f；公网SSE200/OK/唯一完成，usage41004唯一。
事实：账号10人工停用与所有账号策略保持；PG/Redis/432迁移不变；维护已解除。
剩余：原28项20done/8部分；v2.21.1/PR#3待冲突整合；供应商等外部契约待补。
清理：远端释放190890582bytes；本地3脚本9817bytes删除被策略拦截，见下，不重试。
边界：5/3/1开启，优先调度false；回滚程序、镜像、数据及数据库备份保留。
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
| D3 | in_progress | 状态/文本准入及带内/空流/终态/间隔超时已验；.3流内漏计/初始重选及5/3/1上线，race/副本/公网SSE账单通过；其他能力待验 |
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
| D9 | in_progress | .3自有发行/排空/重建/加载哈希及公网SSE唯一账单已验；原设置持久化/桌面/手机保存保持；完整D8及供应商能力仍待 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为a5116107/ransub2api，upstream为ranxi2001/sub2api且禁推。
- 正式源码根153包已验；unit/integration并发1–2，沿用go.mod/lint规则。Redis竞争用既有5.0.14.1，Redis3缺Lua方法；历史失败不改报成功。
- 图片夹具隔离DATA_DIR；12张历史PNG/1164bytes保留。协议/区域/CLI的实际覆盖与复现命令见DECISIONS，不把本地夹具当真实供应商。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 下一项工作

原28项20 done、8部分（D2–D9）。生产.3/5ce1d1892，截图修复/副本回退/公网唯一账单通过，5/3/1开启。上游v2.21.1在[PR#3](https://github.com/a5116107/ransub2api/pull/3)待冲突整合，不称已集成最新上游；原外部验收保留。

## 外部等待与清理

- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9发布子项已完成；并行混版、非文本能力及缓存迁移净收益仍按原D8依赖验收。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删的候选、overlay、pyc和运维脚本完整路径/大小/证据见DECISIONS；均保留由拥有者处理，不重试或移动；正式.1包仍在-final。
- 本轮`H:/Root/DevCache/tmp/sub2api-stream-health`的ops.py/public-smoke.py/notes.md（3文件/9817bytes）删除在进程创建前被策略拦截，实际删除0；无活跃引用，由拥有者手工清理，不重试或移动。
- 本轮原规则全包lint为62项既存诊断（61项未改文件、1项原行），增量0；保留失败输出，不称整包全绿。
