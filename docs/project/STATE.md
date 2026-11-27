# 项目状态

## 恢复检查点

<!-- resume:start -->
R1.3进行中：70abfd1c5已集成，质量单测/UI39/类型/PG双轮race已验；静态/副本/单PR/发行上线待验，不CI。
R1.2已验：正式2.25.4-ransub.2/应用源1c9955ba0，生产加载090bb3bebbef/固定镜像4e74b33ef5c9。
PR8已合并；两轮副本498→499→498→499及诊断历史恢复通过，原PG/Redis/16账号与历史保持。
生产在线更新/优雅重启/同镜像重建、公网中英双宽/流量/总览/工具/沙箱已验。
唯一SSE/usage139546与claim/quote各1、金额一致；UI186/PG195及专项race/vet/lint已验，非长期SLA。
完整service通过；handler原失败/43skip保留，本轮补完整复验。预编译镜像run45877105396成功，七源码作业skipped。
下一R1.3复测与耗时标签回归、生产副本、正式.3发布；原来源/fast/插件计量/缓存域/旧客户端及混版仍开放。
原28项20done/8部分；凭据未提供，受保护旧scratch/首候选54247114B不绕行。
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
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3.1 | done | gpt-6.1-sol真实工具两轮/同账号/每轮唯一用量已验；未改自动监测规则，文本不证明工具能力 |
| D3.2 | done | 计划/预算/结果卡及service/UI55/类型lint/副本四阶段、.4生产加载/API/入口已验；默认不启用，不扩大调度许可 |
| D3 | in_progress | 文本准入/超时及5/3/1、独立工具检测已交付；工具业务两轮已验，其他能力/插件计量仍待 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 未引用辅助符号及死计数器已删，unused归零 |
| M2 | done | 历史断言/错误处理/静态诊断已修，原回归及lint通过 |
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
| D4 | in_progress | 倍率门、固定售价、7平台/BPS发送前终检已验；依赖D2/区域及模型fast来源 |
| D5 | in_progress | v2.25.4退役逐请求诊断，账号usage指标保留；线路来源/缓存域与估计仍待证据 |
| D6 | in_progress | 会话保留/硬准入已验；软迁移待可信前缀、缓存域及净收益证据 |
| D7.1 | done | 中性对照状态、中英/UI与公网加载已验；单样本结论保留 |
| D7.2 | done | .2同源API/PG/中英1440/390、过滤/编辑/原作品沙箱/TTL/失败恢复与字形已验 |
| D7 | in_progress | 并发保存/重算及UI构建/多语言已验；完整解释待契约 |
| D8.1 | done | 后台重启复用SIGTERM；Linux红→三轮绿/完整13父子、vet/lint0及生产后台重启/同镜像重建已验 |
| D8.2 | done | .1/.2管理并行/并发通过；旧.9丢cache_weight已实证并在验收中拒绝；84工具测试/清理通过，不冒充完整混版 |
| D8 | in_progress | CLI/额度/统一性能及副本排空重建已验；完整并行混版待验 |
| D9 | in_progress | .1唯一SSE用量/管理/UI/在线更新/排空重建已验；旧Mac/BPS长流与供应池待验 |
| R1 | done | 2.25.4-ransub.2/1c9955ba0已发行上线，程序/镜像/管理/UI及唯一SSE结算一致，开发线保持 |
| R1.1 | done | PR7冲突/官方2.25.3/400迁移回退及.2发布/运行界面验收完成；原父项不关闭 |
| R1.2 | done | PR8/官方2.25.4、诊断备份/恢复及499迁移回退、.2同源发行/实际加载/公网唯一结算已验 |
| R1.3 | in_progress | 质量单测/PG双轮race及UI39/类型通过；旧夹具补租约栅栏，静态/副本/单PR/发行上线待验 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`，当前integration/upstream-stable，验后快进production；origin自有ransub2api，官方upstream禁推。
- 正式测试/lint/vet及失败证据见DECISIONS；unit/integration并发1–2，Redis竞争用5.0.14.1，失败不改报通过。
- 协议/区域/CLI及图片夹具的覆盖、复现和清理见DECISIONS；本地夹具不等于供应商验收。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 下一项工作

R1.3先完成复测/耗时回归及生产副本升级回退，再单PR发行上线；原来源/计量/缓存域、旧客户端及混版验收仍保留。

## 外部等待与清理

- D8/D9：.4自研.2串行升级/诊断恢复回退和生产界面已验；完整混版、旧长会话/供应池仍待。
- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9.1：新版SSE139546及原样本仅证明各自完整/唯一结算；旧Mac/BPS长流/全混版、非文本/缓存净收益未关闭。
- D3：独立工具检测及结果卡已交付、默认不启用；其他能力与插件物理计量仍待，不据文本/工具样本推广。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整repository缺`SeCreateSymbolicLinkPrivilege`，Linux同例已过；管理员提供权限后重跑，未改skip。
- 历史拒删的候选/overlay/pyc/脚本路径与证据见DECISIONS；保留由拥有者处理，未重试或移动。
- 旧dist/pyc及首候选54247114B受策略保护；路径见DECISIONS，由拥有者处理，不绕行。
- 凭据导入等待轮换后本机`.codex/secrets/hudy-chatgpt.json`；2026-11-26只查元数据时不存在，未读聊天token。有效性通过后入group2。
