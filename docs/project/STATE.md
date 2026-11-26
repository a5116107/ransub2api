# 项目状态

## 恢复检查点

<!-- resume:start -->
D7.1/D8.1已验：待对照中性显示+后台SIGTERM排空；2.25.2-ransub.2正式上线。
源aeabbcbc46f7c89f2f73507f047d16670e766222，程序279415426b848c81；唯一production。
UI184/类型lint0/中英双宽28过；Linux重启三轮/完整13父子/vet/lint0。
副本四阶段497迁移/11表指纹通过；生产升级/后台重启/同镜像重建全过。
原PG/Redis/16账号/2组/设置保持；公网SSE1.734秒/唯一completed，仅usage135529。
公网本窗口无待对照样本，匹配构建夹具已验；current/latest=.2，无更新警告。
Release/Sync active；45747279393仅封装成功，其余7job跳过，不CI。
候选/字段删1064386687bytes，本地测试程序删7600766bytes，回滚保留。
原28项20done/8部分；D2–D9契约/非文本/旧长流/缓存净收益原验收保留。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-26。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
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
| D7.1 | done | 待对照结论分层：红4→绿184/类型lint0、中英双宽28、.2同源加载及公网UI已验；单样本真实结果保持 |
| D7 | in_progress | 并发保存/409、重算及候选缺失/无效延迟已验；UI112+支付21、类型/lint/构建/中英390px刷新通过；完整解释待契约 |
| D8.1 | done | 后台重启复用SIGTERM；Linux红→三轮绿/完整13父子、vet/lint0及生产后台重启/同镜像重建已验 |
| D8 | in_progress | CLI/额度/统一性能、v2252副本432→497四阶段及11表指纹、生产排空/同镜像重建已验；完整并行混版等待 |
| D9 | in_progress | .9 Goal/429换线、.2公网SSE唯一用量/UI/排空重建及120/300保持已验；旧Mac会话/BPS长流与完整供应池仍待 |
| R1 | done | 2.25.2-ransub.2/aeabbcbc46f7已发行上线；源码/制品/加载一致，副本回退及公网SSE/UI已验；开发在研保持 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`，当前production；origin为自有ransub2api，upstream为ranxi2001/sub2api且禁推。
- 正式源码根153包已验；unit/integration并发1–2，沿用go.mod/lint规则。Redis竞争用既有5.0.14.1，Redis3缺Lua方法；历史失败不改报成功。
- 图片夹具隔离DATA_DIR；12张历史PNG已核生成哈希并删，9测试无泄漏。协议/区域/CLI的实际覆盖与复现命令见DECISIONS，不把本地夹具当真实供应商。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 下一项工作

D7.1/D8.1修复、回归、发行上线与清理已验；生产/自有Release/稳定应用同源，文档随production同步。原28项20done/8部分保持。续接优先D2/D4来源契约和D3非文本范围，依赖方与条件见下。

## 外部等待与清理

- D8/D9：.2串行升级/回退及生产SSE/UI已验；完整混版、真实旧长会话及全供应池仍待原验收，不关闭父任务。
- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9.1：SSE135529唯一结算；后验22条为21完成/1标注客户端断连，原样留证。旧Mac/BPS长流/全混版及非文本/缓存净收益未关闭。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整repository缺`SeCreateSymbolicLinkPrivilege`，Linux同例已过；管理员提供权限后重跑，未改skip。
- 历史拒删的候选/overlay/pyc/脚本路径与证据见DECISIONS；保留由拥有者处理，未重试或移动。
- 历史拒删scratch（含stream-health/production-failover/release-build/stable-sync）见DECISIONS，拥有者处理，未重试。
- 固定基线不限量lint0/vet0；完整历史失败保留，不称整仓全绿。
