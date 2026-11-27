# 项目状态

## 恢复检查点

<!-- resume:start -->
R1.7 done：.1/337c92a62/76444f64，500迁移/回退/UI/SSE144699已验。
账号15精确400冷却1分钟，回归/vet通过；正例未触发规则。
520地址已确认；入口499/上游reset分开，唯一关联待证。
原28项20done/8部分；Release active/不CI；首页原改动保留。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-27。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 保留自研与自有渠道；本轮授权最新版本合并/发行/上线，不CI；真实采购价不做。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | D2.1–D2.4、共享Key/多出站/区域额度与视频账本已验；unknown待请求关联契约 |
| D2.1 | done | 同步新鲜度/峰值域与在线预演同源，30边界/完整回归/CLI已验 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 统一线路身份/旧证据失效及9平台实际发送前终检已验 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3.1 | done | gpt-6.1-sol真实工具两轮/同账号/每轮唯一用量已验；未改自动监测规则，文本不证明工具能力 |
| D3.2 | done | 独立工具计划/预算/结果卡、UI55/副本回退/.4生产已验；默认不启用，不扩大准入 |
| D3 | in_progress | 文本准入/超时及5/3/1、独立工具检测已交付；工具业务两轮已验，其他能力/插件计量仍待 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 未引用辅助符号及死计数器已删，unused归零 |
| M2 | done | 历史断言/错误处理/静态诊断已修，原回归及lint通过 |
| M2.1 | done | 7368pass/2原skip、race/静态、PR10/.4副本/生产/UI/结算已验 |
| M3 | done | WinSock持久故障识别与5轮真实备用链通过，超时/重置不误判 |
| M4 | done | 三处私有原子持久化收敛；22项Windows失败修复，双平台回归/vet/lint通过 |
| M5 | done | 生产依赖边界保留，兼容测试例外明确；lint及生产负向检查通过 |
| M6 | done | 无调用假成功ProxyService.TestConnection已删，引用0/编译/vet通过 |
| M7 | done | 无调用假成功AccountService.TestCredentials已删，引用0/编译/vet通过 |
| M8 | done | 分组Key分页及历史请求/成本接入真实统计；handler race/vet通过 |
| M9 | done | 兑换码状态/类型/已用面值接入真实统计；回归/编译/vet通过 |
| M10 | done | 假成功AdminService.RefreshAccountCredentials已删；引用0/回归/编译/vet通过 |
| M11 | done | 用户周期用量接入真实聚合，校验和缺依赖拒绝；回归/编译/vet通过 |
| M12 | done | 代理/realtime固定零值及假成功已移除，未支持路由501；回归/vet通过 |
| M13 | done | 重复GroupService.GetStats已删，引用0/handler race/编译/vet通过 |
| M14 | done | 9旧镜像回收+8.04GiB；.7/.6/容器/回滚/数据不变，健康200 |
| D4 | in_progress | 倍率门、固定售价、7平台/BPS发送前终检已验；依赖D2/区域及模型fast来源 |
| D5 | in_progress | v2.25.4退役逐请求诊断，账号usage指标保留；线路来源/缓存域与估计仍待证据 |
| D6 | in_progress | 会话保留/硬准入已验；软迁移待可信前缀、缓存域及净收益证据 |
| D7.1 | done | 中性对照状态、中英/UI与公网加载已验；单样本结论保留 |
| D7.2 | done | .2同源API/PG、中英双宽总览交互/沙箱/失败恢复已验，详见DECISIONS |
| D7 | in_progress | 并发保存/重算及UI构建/多语言已验；完整解释待契约 |
| D8.1 | done | 后台重启复用SIGTERM；Linux红→三轮绿/完整13父子、vet/lint0及生产后台重启/同镜像重建已验 |
| D8.2 | done | .1/.2管理并行/并发通过；旧.9丢cache_weight已实证并在验收中拒绝；84工具测试/清理通过，不冒充完整混版 |
| D8.3 | done | 双版HTTP容量/取消排空/拒绝零计费/唯一结算与清理已验，详见DECISIONS |
| D8.4 | done | .4/.5实际CLI双向WS/HTTP容量、18轮/6续接/6拒绝/唯一结算/回退清理已验 |
| D8.5 | in_progress | 旧.5/.6漏账拒绝混流；修复版双实例4取消/恢复/部分结算及回退已验；旧版对仍不兼容 |
| D8 | in_progress | CLI/管理/HTTP/WS双版容量及同修复版取消已验；旧版/区域/后台全混版待验 |
| D9 | in_progress | .1用量/UI/更新已验；新入口499上传断连与上游reset分开，520/旧长流待验 |
| D9.2 | done | 503精确关联空池/启用时间；后台旧错误由用户确认，线上实际UI/SSE成功及最终计时通过；不推广为全会话恢复 |
| D9.3 | done | 生产TCP reset归因/透传唯一失败终态/中英UI及.6实际加载/公网SSE已验；不等于所有旧会话恢复 |
| D9.4 | done | 11427零记账已证；生产负例移除、正例标记与8离线/9工具/Go回归已验，原记录/线上校验保留 |
| D9.5 | done | Gemini profile/桌面入口已配置；CLI生产终端两轮成功，全局/MCP不变；不推广自动检测 |
| R1 | done | 2.25.5-ransub.1/337c92a62上线，程序/镜像/UI/SSE144699唯一结算一致，开发线保留 |
| R1.1 | done | PR7/官方2.25.3/.2发布及迁移回退/UI已验；原父项保留 |
| R1.2 | done | PR8/官方2.25.4/诊断归档恢复/499迁移回退及.2生产已验 |
| R1.3 | done | PR9/官方70abfd1c5/混合复核、PG race/UI与.3生产已验 |
| R1.4 | done | PR11/becb95d40/.5四阶段回退及生产UI/唯一结算已验 |
| R1.5 | done | PR12/204fe3a93/.6副本回退/生产/UI/唯一结算已验，无迁移 |
| R1.6 | done | PR13/.7同源发行/副本/在线更新重建/UI双宽双语及SSE143809唯一结算通过 |
| R1.7 | done | PR14/.1/337c92a62，500迁移/副本回退/更新重建/UI/SSE144699通过 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin自有ransub2api，官方upstream禁推。
- 测试/静态/协议/区域夹具见DECISIONS；并发1–2，Redis5.0.14.1；夹具不等于供应商验收。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 下一项工作

D9定位520的客户端/Cloudflare上传链；原unknown、缓存域及全混版契约保持待验。

## 外部等待与清理

- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9旧长流/520待客户端或Cloudflare请求关联；入口499与上游reset已分开，不以单例推广。
- D3：独立工具检测及结果卡已交付、默认不启用；其他能力与插件物理计量仍待，不据文本/工具样本推广。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整repository缺`SeCreateSymbolicLinkPrivilege`，Linux同例已过；管理员提供权限后重跑，未改skip。
- 拒删d93及gemini-codex内421MB验证产物由宿主/拥有者释放；完整路径见DECISIONS，不绕行。
- 新凭据等待本机`.codex/secrets/hudy-chatgpt.json`，此前元数据检查不存在；未读聊天token，测试通过后入group2。
