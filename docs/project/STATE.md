# 项目状态

## 恢复检查点

<!-- resume:start -->
授权：稳定线提交/同步/发行/部署；不编译测试CI、不新工作树；唯一production。
D7.2/R1.1已验：最终2.25.3-ransub.2/应用源463bf7a00，生产加载67e330aebdf9/镜像cee1c73e4894。
PR7合并，9文件冲突已解；官方实际c95ce812、追踪cd38f119c为祖先；Latest与在线当前一致。
.4→.1迁移497→498/回退与.1→.2/回退四阶段；11历史摘要及原PG/Redis/16账号保持。
真实在线更新/优雅重启/同镜像重建/verify及cleanup均0；7个云端源码作业skipped，自动发布恢复。
.2公网中英1440/390、16账号、过滤/下钻、编辑预填、授权、原作品沙箱、失败恢复/键盘/字形已验。
复用未变后端.1 SSE唯一完成/usage138899/账号14；补丁只改前端字符/VERSION，未重复付费请求。
前端114/331/22/19分范围、类型/eslint，PG联合77s/race双轮12s、service/admin/vet和兼容lint0已验。
新候选及字段scratch累计清513373182B；原备份/镜像与复用缓存保留，旧受保护scratch不绕行。
下一：等待供应商/接入方的关联与缓存域契约、插件计量及真实旧客户端证据；原28项20done/8部分不关闭。
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
| M1 | done | 未引用辅助符号及死计数器已删，unused归零；证据见DECISIONS |
| M2 | done | 历史断言/错误处理/静态诊断已修，原回归及lint通过；证据见DECISIONS |
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
| D5 | in_progress | 协议/缓存/TPS/TTFT与未知值隔离已验；跨协议和会话估计待证据 |
| D6 | in_progress | 会话保留/硬准入已验；软迁移待可信前缀、缓存域及净收益证据 |
| D7.1 | done | 中性对照状态、中英/UI与公网加载已验；单样本结论保留 |
| D7.2 | done | .2同源API/PG/中英1440/390、过滤/编辑/原作品沙箱/TTL/失败恢复与字形已验 |
| D7 | in_progress | 并发保存/重算及UI构建/多语言已验；完整解释待契约 |
| D8.1 | done | 后台重启复用SIGTERM；Linux红→三轮绿/完整13父子、vet/lint0及生产后台重启/同镜像重建已验 |
| D8.2 | done | .1/.2管理并行/并发通过；旧.9丢cache_weight已实证并在验收中拒绝；84工具测试/清理通过，不冒充完整混版 |
| D8 | in_progress | CLI/额度/统一性能及副本排空重建已验；完整并行混版待验 |
| D9 | in_progress | .1唯一SSE用量/管理/UI/在线更新/排空重建已验；旧Mac/BPS长流与供应池待验 |
| R1 | done | .2/463bf7a00已发行上线，程序/镜像/管理/UI一致；复用未变后端SSE，开发线保持 |
| R1.1 | done | PR7冲突/官方2.25.3/400迁移回退及.2发布/运行界面验收完成；原父项不关闭 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`，当前production；origin自有ransub2api，官方upstream禁推。
- 正式测试/lint/vet及失败证据见DECISIONS；unit/integration并发1–2，Redis竞争用5.0.14.1，失败不改报通过。
- 图片夹具隔离DATA_DIR；12张历史PNG已核生成哈希并删，9测试无泄漏。协议/区域/CLI的实际覆盖与复现命令见DECISIONS，不把本地夹具当真实供应商。
- 不增平行实现/台账；C盘effb8271不计主线，开发统一H盘。

## 下一项工作

D7.2/R1.1完成。待D2/D4来源契约、D5/D6缓存域及真实旧客户端证据，继续原依赖验收。

## 外部等待与清理

- D8/D9：.1/.2串行升级/回退和生产界面已验；完整混版、真实旧长会话及全供应池待原验收。
- D2/D4：供应商或接入方补unknown关联查询及模型/fast倍率来源契约；不恢复真实采购价格任务。
- D9.1：SSE135529唯一结算；后验22条为21完成/1标注客户端断连，原样留证。旧Mac/BPS长流/全混版及非文本/缓存净收益未关闭。
- D3：独立工具检测及结果卡已交付、默认不启用；其他能力与插件物理计量仍待，不据文本/工具样本推广。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- `backend/tmp/availability-non5xx-regression`历史overlay曾拒删，导致根`go test ./...`误编译；正式源码目录153包通过，未改删该目录。
- Windows完整repository缺`SeCreateSymbolicLinkPrivilege`，Linux同例已过；管理员提供权限后重跑，未改skip。
- 历史拒删的候选/overlay/pyc/脚本路径与证据见DECISIONS；保留由拥有者处理，未重试或移动。
- 旧dist/历史ops.pyc(19355B)及scratch受策略保护；路径见DECISIONS，由拥有者处理，不绕行。
- 历史验证/失败与清理证据见DECISIONS，不称整仓全绿。
- 凭据导入等待轮换后本机`.codex/secrets/hudy-chatgpt.json`；2026-11-26只查元数据时不存在，未读聊天token。有效性通过后入group2。
