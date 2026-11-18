# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D8.1响应进程CN运行库存子验收本地done。
范围：唯一H盘功能分支提交同步；不CI/PR/生产，不改业务与探测判据。
已决：实际usage、插件自动延期与手动隔离保持，不借票/续绿。
来源：7c1a7726c起点；终稿7da9d5bc，命令/边界见DECISIONS。
已验：服务5540/SQL44/UI49pass；0fail/skip/race；静态/Linux/四视口过。
下一：D8.1其余来源运行观测/全scope覆盖编译；跨节点/生产仍待。
等待：原28项20done/8部分；其余库存/全scope/跨节点/生产/PR3待。
生产最后记录.7/fd83f6c91；本轮不部署；历史拒删保持、不重复试删。
Attempt：90
<!-- resume:end -->

## 目标与入口

更新：2026-11-19。倍率内多供应商自动调度，保护质量、响应和会话缓存，保留自研并同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)：13.3保留审计基线，5.3.1补自动适用声明边界。
- [决策与验证历史](DECISIONS.md)；功能分支未整体验证上线，独立生产热修复已加载。
- 原28项20done/8部分；13.2子项6/7done；分母保持，单分支/单PR。
- R1仅发行production；线上问题按D9.1处理，真实采购价延期。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | D2.6.1–.17已本地验；其余状态转换、受管产品与真实来源仍缺；D2.7待供应商 |
| D2.1 | done | 同步新鲜度/高峰及同源预演、CLI已验 |
| D2.2 | done | 固定组/null/明确0及SQL失效恢复已验 |
| D2.3 | done | 身份统一、证据失效/协议切换/9平台终检已验 |
| D2.4 | done | 探测/授权/库存/绑定/事务的生效地址已验 |
| D2.5 | done | 最终凭据HTTP/WS及权限已验；非实际组/缓存域 |
| D2.6.1–.4 | done | 自动/手工声明、精确预演、New API同源绑定/TTL/重放、SQL CAS、HTTP/WS/UI本地已验；逐项证据见DECISIONS |
| D2.6.5–.12 | done | 原生/固定转换、四WS/两HTTP桥候选已本地验；静态/构建及未变SQL/UI证据见DECISIONS，非生产 |
| D2.6.13 | done | smart原标量/历史及11类逐轮路径已验；race6876，非生产 |
| D2.6.14 | done | Messages→Chat映射/effort/Fast与调度/HTTP已验；race7652，非生产 |
| D2.6.15 | done | Messages→Responses/verbosity、UI及HTTP/11WS已验；race7936，非生产 |
| D2.6.16 | done | 池桥协议、双调度/黏性/槽后与17类物理帧已验；race8214，非生产 |
| D2.6.17 | done | HTTP→原生WS映射/Fast/流式、双调度/槽后/拒发已验；race8378，非生产 |
| D3 | in_progress | .7流内换号/冷却回池/密文绑定已验，5/3/1保持；D3.2.1–.7本地done；其余线路/故障/跨规则/独立基线/非文本仍待 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项/完整回归 |
| D4.2 | done | 配置失败/错组拒绝，HTTP/WS/免费及恢复；默认标签/vet/Linux完整通过 |
| M1–M13 | done | 原清理/分页/依赖及双平台验收保持；逐项源码与验证见DECISIONS |
| D4 | in_progress | 固定售价/新鲜策略/多平台最终门已有本地证据；D4.3采购桶/受管资格/模式与重归类仍未闭环 |
| D5 | in_progress | 逐尝试缓存/等待/输出/入口来源已本地验；实际组/受信外部检测/其余scope/资格及D5.4仍缺 |
| D6 | in_progress | 已配置健康保留/故障避让及恢复不抢回已验；D6.2产品接线仍缺，D6.3软迁移待可信前缀/候选域和净收益 |
| D7 | in_progress | 历史结论/下游原始面板及409已有证据；D7.3真实多维结论、D7.4三步产品配置/监控/预算解释仍缺 |
| D5.1/D6.1 | done | 上下游/账单合同；同run粘性/跨run隔离、Messages独立前缀，race40/vet |
| D5.2 | done | 最终原始摘要、唯一逻辑计数/覆盖、本人筛选/30天与中英390px；非生产证据 |
| D5.3.1–.10 | done | 最终Key/发送scope、HTTP与独立WS等待、逐尝试缓存/输出及来源分桶已本地验；逐项证据见DECISIONS；非实际组/资格/生产 |
| D5.3.11 | done | Messages模型/tier/速度/effort/思考模式及预算独立分桶；PG614/竞态644/计时788/UI87及静态/Linux/四视口通过；仅本地 |
| D5.3.12 | done | Gemini URL/信封模型、实际action/预算模式/等级/思考输出独立分桶；PG666/竞态1074/计时821/UI115及静态/Linux/四视口通过；仅本地 |
| D5.3.13 | done | Responses摘要/加密输出请求/verbosity分桶，旧/非法未知；计时1718/服务5526/SQL428/UI119、静态/四视口已验；非生产 |
| D7.1/D7.2 | done | 结构化结论/覆盖/账号一致性，计费标签/开关/CSV；前端测试/类型/lint/构建及中英手机 |
| D3.1 | done | 原HTTP/Redis本地链五类失效换线/恢复不抢回及失败关闭；race106/1838与独立Key11；非生产 |
| D7.3 | in_progress | 历史/冻结集合、多维详情、期限/轮次/API-Key线路与登记故障已验；独立基线、其余线路/跨规则/故障与资格消费仍缺 |
| D8 | in_progress | 物理/Token/100队列及倍率、账号/分组计划、初始建号容量拒存已本地验；完整scope/其余通道/公平性/生产/严格混版仍缺 |
| D8.1本地子项 | in_progress | 预算/计量/延期、队列公平与计划停机已本地验；CN本机寿命/周期/最近库存/API/UI本地done；其余库存/全scope/多节点/生产仍缺 |
| D9 | in_progress | .7四阶段/16历史、排空/重建、公网SSE/唯一账单/UI已验；旧失败会话完整恢复/D9.2仍待 |
| R1 | done | .7公开latest/固定镜像/线上同源fd83f6c91；仅预构建镜像封装，无编译测试CI，不自动提升功能分支 |

## 剩余执行顺序与等待

- 本地顺序：D2.6/D5.3→D3.2/D8.1→D4.3→D6.2→D7.3/D7.4→D8.2；D5.4独立接入，严格高缓存资格仍依赖它。
- D9.1生产.7已验；PR3/v2.21.5冲突未解，未验功能不发布，历史证据见DECISIONS。
- 外部数据owner为供应商/接入方：实际组/模型/fast、独立参考、缓存域/存活和unknown请求唯一关联；开发方接入验证。取得可核对scope/证据后释放相应验收。
- D3.3首批非文本能力仍待明确供应商/模型/协议；D6.3真实迁移净收益仍待前缀与候选域。文本通过不外推工具/媒体/语音。
- D8.2旧版重订阅对照已失败，同候选恢复通过不算严格混版；统一排空升级是另一交付路径，不更名关闭原验收。D9.2仍依赖完整本地与实际启用范围证据。

## 工作区与既有环境限制

- 唯一工作树：H:/Root/jiaoben/01_AI接口与网关/sub2api；origin=a5116107/ransub2api；upstream=ranxi2001/sub2api且禁推。统一H盘，不建额外worktree。
- 历史12个2×3测试PNG/1164bytes匹配源码SHA256；data/原生删除被宿主拦截，保留原路径。
- backend/tmp/availability-non5xx-regression历史overlay拒删，根go test ./...会误编译；正式源码153包已验，未删改目录。
- Windows完整repository缺SeCreateSymbolicLinkPrivilege；Linux同例通过。owner系统管理员，提供权限进程后重跑，未改skip。
- 私有PG55441/PID10868、Redis56441/PID46228、quality-batch-{build,runner.py}及`H:/Root/DevCache/tmp/sub2api-quality-validity`受宿主拒删；预览已停；owner宿主用户，路径/解除条件见DECISIONS。
- 历史拒删620274603bytes及D2.6日志原位保留；路径/命令/owner见DECISIONS，待宿主解除策略，不绕过。
- 本轮四包vet/增量fast/Linux及前端类型/lint/构建通过；非全仓lint。
- 历史构建213260543bytes及本轮临时产物9279228bytes受宿主拒删；路径/owner/解除条件见DECISIONS；未绕行，回收0。
