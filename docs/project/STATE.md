# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D5.3.1最终Key/线路/模型原始缓存本地已验，提交同步。
授权：统一H盘工作树；本地代码/测试/文档及功能分支提交同步；不CI/部署。
验证：timing race双轮1234、SQL双轮202、权限/API等29通过事件；前端24/类型/lint/构建及中英390px通过。
实证：最终凭据/线路分桶、原始加权/覆盖/未知/30天及SQL失败恢复已验；不补历史或冒充实际组。
下一动作：同步本子项；D2.6→D5.3同scope物理attempt/延迟→D3.2/D8.1。
等待：供应商实际scope/基线/缓存域；本轮fixture和scratch删除被策略拦截，见末节。
Attempt：1
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-16。倍率内多供应商自动调度，保护质量、响应体验和会话缓存，保留自研并便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)：13.3保留c03a72245审计基线，并补D5.3.1实现边界；历史参考仓未重审。
- [追加式决策与验证历史](DECISIONS.md)。本轮原始缓存本地回归通过；未核验生产或Release，历史结果保留原边界。
- 原28项20done/8部分；13.2七子项6done/1部分。新缺口ID不增分母；整个功能仍单分支/单完整PR。
- R1此前授权production自动发行，不自动提升功能分支/安装/重启；本轮本地实现/同步，不CI或部署。真实采购价继续延期。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置、倍率口径、共享Key缺口和补丁边界已记录 |
| D2 | in_progress | 绑定/多出站/区域/持久交接已有证据；D2.6模型/fast声明合同仍缺本地实现，D2.7 unknown唯一关联待供应商 |
| D2.1 | done | 自动同步新鲜度、高峰与预演同源，边界/完整回归和CLI已验 |
| D2.2 | done | New API固定组、null/明确0、预览/探测同源及SQL失效/恢复，绑定/成本保持 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一身份；旧证据失效、协议切换及9平台发送前拦截 |
| D2.4 | done | 探测及New API授权/库存/绑定/事务使用生效地址；旧源码反例和修复回归已验 |
| D2.5 | done | HTTP/WS最终凭据摘要、重试与快照/观察员隔离；不证明实际组或缓存域 |
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

- 本地可执行：D2.6/D5.3剩余同scope物理attempt与延迟→D3.2/D8.1→D4.3→D6.2→D7.3/D7.4→D8.2；D5.4在输入稳定后补，高缓存产品须等它通过。不因缺真实基线停做本地合同。
- 独立发布：D9.1先核当前生产/可部署差异，按原发布边界交付冷却补丁；不可整分支带入未验功能。本轮不发布。
- 外部数据owner为供应商/接入方：实际组/模型/fast、独立参考、缓存域/存活和unknown请求唯一关联；开发方接入验证。取得可核对scope/证据后释放相应验收。
- D3.3首批非文本能力仍待明确供应商/模型/协议；D6.3真实迁移净收益仍待前缀与候选域。文本通过不外推工具/媒体/语音。
- D8.2旧版重订阅对照已失败，同候选恢复通过不算严格混版；统一排空升级是另一交付路径，不更名关闭原验收。D9.2仍依赖完整本地与实际启用范围证据。

## 工作区与既有环境限制

- 唯一工作树：H:/Root/jiaoben/01_AI接口与网关/sub2api；origin=a5116107/ransub2api；upstream=ranxi2001/sub2api且禁推。统一H盘，不建额外worktree。
- 正式源码153包历史已验，测试并发1–2；Redis5.0.14.1复用，Redis3缺Lua的失败保留；命令/来源见DECISIONS，不代表真实供应商。
- 12个2×3测试PNG/1164bytes已由源码生成匹配SHA256；data/删除被执行策略拦截，保留原路径不换工具。
- backend/tmp/availability-non5xx-regression历史overlay拒删，根go test ./...会误编译；正式源码153包已验，未删改目录。
- Windows完整repository缺SeCreateSymbolicLinkPrivilege，Linux同例通过；系统管理员提供符号链接权限进程后重跑，未改skip。
- 历史拒删候选/overlay/pyc/stream-health/cache-conclusions/v2211-verify及newapi空目录保留原owner/路径/解除条件，见DECISIONS；不换工具重试/移动，.1包保留。
- H:/Root/DevCache/tmp/sub2api-cooldown-recovery同被blocked by policy；已有0文件/0字节、非重解析点证据，删除0，待拥有者清理。
- 本轮timing全规则、六包增量fast lint/vet与Linux编译通过；PATH旧lint失败保留。独立PG/Redis/预览已停；scratch保留H:/Root/DevCache/tmp/sub2api-upstream-raw-cache，删除待执行策略解除。
