# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D4.3.10批准来源分组动态硬门本地done；沿当前功能分支交付。
授权：统一H盘实现/提交同步；不CI/PR/合并/部署/生产。
验证来源：3471a2cdb基础；Go/SQL LFc56f1b84fbb3，UI78e69bb9ec23。
验证命令：unit/真实PG双轮race、vet/lint、UI及Edge四组合/Linux嵌入。
验证结果：528/166/默认133pass，0fail/race；UI204/浏览器48项，静态/嵌入0。
下一动作：D4.3受管模式/拥有者与关系重算；D6.2产品选路接线。
尝试：111
阻塞：供应商/缓存域/参考/多节点/PR3及清理，原父验收保持。
责任方：供应商/开发方/宿主用户，详下文。
解除条件：提供合同/验收环境、解决冲突及解除删除策略。
生产.7/fd83f6c91未变；新scratch142733451B拒删见DECISIONS。
补验：D3.2.3判题UI312/浏览器20；线上只读，独立池及发布待处理。
<!-- resume:end -->

## 目标与入口

更新：2026-11-20。倍率内自动调度，保护质量、响应/缓存及自研。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)：13.3审计、5.3.1声明边界。
- [决策与验证历史](DECISIONS.md)；功能分支未上线，热修复已加载。
- 原28项20done/8部分；13.2为6/7done；单分支/单PR，不加分母。
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
| D2.6.1–.4 | done | 自动/手工声明、精确预演、New API绑定/TTL/重放、SQL CAS及HTTP/WS/UI本地已验 |
| D2.6.5–.12 | done | 原生/固定转换、四WS/两HTTP桥及静态/构建已验；SQL/UI见DECISIONS，非生产 |
| D2.6.13 | done | smart标量/历史及11类逐轮路径已验，非生产 |
| D2.6.14 | done | Messages→Chat映射/effort/Fast及调度/HTTP已验，非生产 |
| D2.6.15 | done | Messages→Responses/verbosity、UI及HTTP/11WS已验，非生产 |
| D2.6.16 | done | 池桥协议、双调度/黏性/槽后与17类物理帧已验；race8214，非生产 |
| D2.6.17 | done | HTTP→原生WS映射/Fast/流式、双调度/槽后/拒发已验；race8378，非生产 |
| D3 | in_progress | .7/5/3/1保持，D3.2.1–.8本地done；判题循环依赖已查/未知UI已修；独立判题池配置、其余scope/独立基线/生产待验 |
| D3.2.8 | done | 同scope多规则/全样本、SQL/HTTP/有界100范围与中英手机已验；仅答题证据，模型身份/资格未知 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项/完整回归 |
| D4.2 | done | 配置失败/错组拒绝，HTTP/WS/免费及恢复；默认标签/vet/Linux完整通过 |
| M1–M13 | done | 原清理/分页/依赖及双平台验收保持；逐项源码与验证见DECISIONS |
| D4 | in_progress | D4.3.1–.10本地已验；受管模式/拥有者/关系重算仍缺 |
| D4.3.1–.6 | done | 区间/目录/来源/产品预演/允许桶/严格JSON及CAS、SQL/终检/UI已验；逐项见原设计与DECISIONS，非完整资格/生产 |
| D4.3.7 | done | v4/SQL/候选黏性/槽后/HTTP与WS实帧拒发恢复及中英四视口已验；非完整资格/生产 |
| D4.3.8 | done | v5最新可用门/原生与受管/实帧/SQL及中英UI已验；8892/716/93/UI133；非完整资格/生产 |
| D4.3.9 | done | 配置门交集/独立到期及SQL/UI已本地验；328/148/24/UI143+i18n3/浏览器36，非完整资格/生产 |
| D4.3.10 | done | v6当前来源/同快照、候选/终检/区域/SQL/UI已验；528/166、UI204/浏览器48，非自动关联/生产 |
| D5.4.1 | done | 原生OpenAI Key/模型HTTP同路探测及中英UI本地已验；非缓存域/严格资格 |
| D5 | in_progress | 逐尝试缓存/等待/输出/来源及同路探测已本地验；实际组/其余scope/缓存域/存活与资格仍缺 |
| D6 | in_progress | 已配置健康保留/故障避让及恢复不抢回已验；D6.2产品接线仍缺，D6.3软迁移待可信前缀/候选域和净收益 |
| D7 | in_progress | 历史结论/下游原始面板及409已有证据；D7.3真实多维结论、D7.4三步产品配置/监控/预算解释仍缺 |
| D5.1/D6.1 | done | 上下游/账单合同；同run粘性/跨run隔离、Messages独立前缀，race40/vet |
| D5.2 | done | 最终原始摘要、唯一逻辑计数/覆盖、本人筛选/30天与中英390px；非生产证据 |
| D5.3.1–.10 | done | 最终Key/scope、HTTP/WS等待、逐尝试缓存/输出/来源已验；非实际组/资格/生产 |
| D5.3.11 | done | Messages速度/思考/预算分桶，PG/race/计时/UI/静态/Linux/四视口已验 |
| D5.3.12 | done | Gemini模型/action/预算/等级/思考分桶，PG/race/计时/UI/静态/Linux/四视口已验 |
| D5.3.13 | done | Responses摘要/加密/verbosity分桶，非法未知；计时/服务/SQL/UI/静态/四视口已验 |
| D7.1/D7.2 | done | 结论/覆盖/账号一致性、计费标签/开关/CSV及前端静态/构建/中英手机已验 |
| D3.1 | done | 原HTTP/Redis本地链五类失效换线/恢复不抢回及失败关闭；race106/1838与独立Key11；非生产 |
| D7.3 | in_progress | 历史/多维详情与同scope答题汇总已验；独立基线、其余线路/跨规则真实覆盖/故障与资格消费仍缺 |
| D8 | in_progress | 物理/Token/100队列及倍率/V1站点与已知Key日下界已本地验；全scope/其余通道/公平性/生产/严格混版仍缺 |
| D8.1本地子项 | in_progress | 预算/队列/倍率/V1归桶、共Key拒发/恢复及CN/Ollama/Responses本地API/UI已验；全scope/库存/多节点/生产待验 |
| D9 | in_progress | .7四阶段/16历史、排空/重建、公网SSE/唯一账单/UI已验；旧失败会话完整恢复/D9.2仍待 |
| R1 | done | .7公开latest/固定镜像/线上同源fd83f6c91；仅预构建镜像封装，无编译测试CI，不自动提升功能分支 |

## 剩余执行顺序与等待

- 本地顺序：D2.6/D5.3→D3.2/D8.1→D4.3→D6.2→D7.3/D7.4→D8.2；D5.4独立接入，严格高缓存资格仍依赖它。
- D9.1生产.7已验；PR3/v2.21.5冲突未解；未验功能不发布，见DECISIONS。
- 外部数据owner为供应商/接入方：实际组/模型/fast、独立参考、缓存域/存活和unknown请求唯一关联；开发方接入验证。取得可核对scope/证据后释放相应验收。
- D3.3首批非文本能力仍待明确供应商/模型/协议；D6.3真实迁移净收益仍待前缀与候选域。文本通过不外推工具/媒体/语音。
- D8.2旧版重订阅对照已失败，同候选恢复通过不算严格混版；统一排空升级是另一交付路径，不更名关闭原验收。D9.2仍依赖完整本地与实际启用范围证据。

## 工作区与环境限制

- 工作树：H:/Root/jiaoben/01_AI接口与网关/sub2api；origin=a5116107/ransub2api；upstream=ranxi2001/sub2api且禁推。统一H盘，无额外worktree。
- 历史12张PNG/1164bytes及data/拒删原位保留。
- backend/tmp/availability-non5xx-regression拒删，根go test ./...会误编译；正式153包已验。
- Windows完整repository缺SeCreateSymbolicLinkPrivilege；Linux同例通过。owner系统管理员，提供权限进程后重跑，未改skip。
- 既有PG55441、Redis56441及quality-batch/quality-validity拒删，owner宿主用户；路径/解除条件见DECISIONS。
- 历史拒删620274603bytes、overlay49259bytes、构建/临时222539771bytes原位；宿主解除后再清理，详DECISIONS；回收0。
