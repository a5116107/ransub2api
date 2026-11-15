# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D2.6.1本地完成：自动适用声明/HTTP与WS终检/结论UI；随本次提交同步。
授权：统一H盘工作树；本地代码/测试/文档及功能分支提交同步；不CI/部署。
验证：race双轮738/SQL30，前端26/类型/ESLint/构建及中英390px；vet/增量fast lint/Linux通过。
实证：模型/协议/档位/effort精确范围、独立TTL、异常/未知/冲突及SQL换Key失效；非供应商或生产证明。
下一动作：D2.6精确scope预演；余限期手工合同及D5.3物理attempt。
等待：供应商实际scope/基线/缓存域；历史fixture/scratch拒删见末节。
Attempt：2
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-16。倍率内多供应商自动调度，保护质量、响应体验和会话缓存，保留自研并便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)：13.3保留审计基线，5.3.1补自动适用声明边界。
- [决策与验证历史](DECISIONS.md)；本轮仅本地，不代表生产加载。
- 原28项20done/8部分；13.2七子项6done/1部分。新缺口ID不增分母；整个功能仍单分支/单完整PR。
- R1此前授权production自动发行，不自动提升功能分支/安装/重启；本轮本地实现/同步，不CI或部署。真实采购价继续延期。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | D2.6.1自动适用声明本地完成；手工/实际供应商合同未齐；D2.7 unknown唯一关联待供应商 |
| D2.1 | done | 同步新鲜度/高峰及同源预演、CLI已验 |
| D2.2 | done | 固定组/null/明确0及SQL失效恢复已验 |
| D2.3 | done | 身份统一、证据失效/协议切换/9平台终检已验 |
| D2.4 | done | 探测/授权/库存/绑定/事务的生效地址已验 |
| D2.5 | done | 最终凭据HTTP/WS及权限已验；非实际组/缓存域 |
| D2.6.1 | done | 自动合同/TTL/冲突/终检/结论UI；race738、SQL30、前端26/构建及中英390px；仅本地 |
| D3 | in_progress | .3漏计/重选及5/3/1历史已上线；本地冷却回池双轮646pass待发布；D3.2多维/基线仍缺，D3.3非文本待scope |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项/完整回归 |
| D4.2 | done | 配置失败/错组拒绝，HTTP/WS/免费及恢复；默认标签/vet/Linux完整通过 |
| M1 | done | 未用符号/死计数删除，unused 6→0 |
| M2 | done | 历史247项/integration101诊断清零，123处断言已核；四包lint/race |
| M3 | done | WinSock拒绝/不可达识别，无超时/重置误判；真实POST/代理/WS五轮 |
| M4 | done | 三处原子持久化统一，22项Windows失败修复；双平台回归/三包lint/vet |
| M5 | done | 生产/夹具依赖边界及兼容点明确；六包lint与禁止依赖反向验证 |
| M6 | done | 删假成功ProxyService.TestConnection；引用0、编译/vet |
| M7 | done | 删假成功AccountService.TestCredentials；引用0、编译/vet |
| M8 | done | 真实Key分页/用量聚合；handler race/vet |
| M9 | done | 真实兑换码分页/面值；admin race及编译/vet |
| M10 | done | 删AdminService.RefreshAccountCredentials接口/实现/stub；admin race/编译/vet |
| M11 | done | 用量原聚合/周期校验/缺依赖关闭；service/admin race及编译/vet |
| M12 | done | 代理/dashboard未支持兼容接口501；admin race/vet |
| M13 | done | 删GroupService.GetStats重复入口；引用0、handler race/service编译/vet |
| D4 | in_progress | 固定售价/新鲜策略/多平台最终门已有本地证据；D4.3采购桶/受管资格/模式与重归类仍未闭环 |
| D5 | in_progress | D5.3.1最终Key/线路/模型原始缓存聚合已验；实际组及同scope物理attempt/延迟仍缺，D5.4缓存域/定向能力待补 |
| D6 | in_progress | 已配置健康保留/故障避让及恢复不抢回已验；D6.2产品接线仍缺，D6.3软迁移待可信前缀/候选域和净收益 |
| D7 | in_progress | 历史结论/下游原始面板及409已有证据；D7.3真实多维结论、D7.4三步产品配置/监控/预算解释仍缺 |
| D5.1/D6.1 | done | 上下游/账单合同；同run粘性/跨run隔离、Messages独立前缀，race40/vet |
| D5.2 | done | 最终原始摘要、唯一逻辑计数/覆盖、本人筛选/30天与中英390px；非生产证据 |
| D5.3.1 | done | 最终凭据/线路/模型/协议/档位分桶，原始加权/覆盖/未知/30天；SQL202、权限29、前端24及中英390px；仅本地 |
| D7.1/D7.2 | done | 结构化结论/覆盖/账号一致性，计费标签/开关/CSV；前端测试/类型/lint/构建及中英手机 |
| D3.1 | done | 原HTTP/Redis本地链五类失效换线/恢复不抢回及失败关闭；race106/1838与独立Key11；非生产 |
| D7.3 | in_progress | 三页历史徽标/冻结集合/列表详情兼容已验；真实向量、基线/指纹和当前资格消费未完成 |
| D8 | in_progress | 两模板各100账号轮转、分页/等待/SQL及30秒网络耗尽恢复已验；D8.1完整预算、D8.2组合矩阵/严格混版仍缺 |
| D9 | in_progress | .3发行/排空/加载哈希/公网SSE唯一用量历史已验；D9.1冷却补丁未有新加载证据，D9.2新应用实发/完整产品仍待 |
| R1 | done | production工具eb0b6243a，自动run43618719372成功；当次无应用变化不重发，镜像匹配.3；非新应用加载证据 |

## 剩余执行顺序与等待

- 本地可执行：D2.6手工限期声明/精确预演及New API来源→D5.3物理attempt/延迟→D3.2/D8.1→D4.3→D6.2→D7.3/D7.4→D8.2；高缓存产品仍等D5.4。不因缺供应商数据停做本地合同。
- 独立发布：D9.1先核当前生产/可部署差异，按原发布边界交付冷却补丁；不可整分支带入未验功能。本轮不发布。
- 外部数据owner为供应商/接入方：实际组/模型/fast、独立参考、缓存域/存活和unknown请求唯一关联；开发方接入验证。取得可核对scope/证据后释放相应验收。
- D3.3首批非文本能力仍待明确供应商/模型/协议；D6.3真实迁移净收益仍待前缀与候选域。文本通过不外推工具/媒体/语音。
- D8.2旧版重订阅对照已失败，同候选恢复通过不算严格混版；统一排空升级是另一交付路径，不更名关闭原验收。D9.2仍依赖完整本地与实际启用范围证据。

## 工作区与既有环境限制

- 唯一工作树：H:/Root/jiaoben/01_AI接口与网关/sub2api；origin=a5116107/ransub2api；upstream=ranxi2001/sub2api且禁推。统一H盘，不建额外worktree。
- 历史153包证据及本轮命令见DECISIONS；并发1–2、共享Go缓存，不代表真实供应商。
- 12个2×3测试PNG/1164bytes已由源码生成匹配SHA256；data/删除被执行策略拦截，保留原路径不换工具。
- backend/tmp/availability-non5xx-regression历史overlay拒删，根go test ./...会误编译；正式源码153包已验，未删改目录。
- Windows完整repository缺SeCreateSymbolicLinkPrivilege，Linux同例通过；系统管理员提供符号链接权限进程后重跑，未改skip。
- 历史拒删候选/overlay/pyc及空目录按DECISIONS保留原owner/路径；不换工具重试/移动，.1包保留。
- H:/Root/DevCache/tmp/sub2api-cooldown-recovery同被blocked by policy；已有0文件/0字节、非重解析点证据，删除0，待拥有者清理。
- D5.3旧timing/lint/vet/Linux证据复用；PATH旧lint不使用。独立PG/Redis/预览已停；raw-cache原scratch仍待删除策略解除。
- D2.6.1全规则lint耗时/内存超预算已停，未报通过；增量fast lint及vet通过，证据在DECISIONS。
