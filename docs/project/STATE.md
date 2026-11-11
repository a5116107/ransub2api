# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D5 TTFT时效/恢复/黏性本地已验；父项开放。
授权：原H盘本地修复/验证/提交；不CI/推送/部署。
源码：b392080后原EWMA；无新缓存或调度器。
验证命令：service race、原规则lint/vet/Linux编译；原PG14 SQL补验。
验证结果：调度229/457、PG14 14/93通过；lint 0 issues，vet/Linux退出0。
验证来源：backend/tmp/protocol-cache/runtime-ttft-*、cache-migration-pg14.jsonl。
下一动作：补齐D5跨请求前缀/缓存域和跨协议可比性证据；D2/D4来源契约仍待。
尝试：14
阻塞：D2 unknown待供应商契约；生产排空待管理员；旧缓存及本轮临时目录拒删待拥有者。
边界：视频查询转发原节点，生成/双预算/结算不迁移；真实采购价不做。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-12。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 授权本地实施/修复/清理/验证/提交；真实采购价不做，生产另需授权。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置覆盖、倍率口径、共享Key缺口与最小补丁边界已记录 |
| D2 | in_progress | D2.1–D2.4、多出站/持久交接、三节点查询/冲突/恢复、视频永久身份/原节点结算、双预算/退款/竞争已验；精确动态售价、grant组/价格授权及336排空/跨库已验，完整lint零诊断；unknown待契约 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 最新状态/托管轮转、区域v2、HTTP/WS、Messages及Gemini/Antigravity完整回答/作用域已验，race/PG14、lint/vet/build通过；其他能力待验 |
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
| D5 | in_progress | 协议/缓存/TPS/请求内换线已验；TTFT时效/恢复/黏性229顶层race及lint/vet/Linux通过，PG14为14/93；跨协议可比性/会话估计待验 |
| D6 | pending | 分配与缓存保护；依赖前置指标与准入 |
| D7 | pending | 前端配置与决策解释；依赖策略契约 |
| D8 | pending | admin/跨层SQL、WS绑定、重试截止及批量资金/竞争/回滚16项夹具已验证；其余集成/升级/全局CLI待验收，不运行CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用Go1.27.3、原go.mod和lint规则；`backend/bin/task-d2-golangci-lint.exe` v2.15，unit/integration标签、并发1–2，lint 6GiB/18分钟。
- 真实Redis竞争测试按PATH启动；本轮前置已有`H:/Root/tools/redis/5.0.14.1`，系统Redis3缺`redis.replicate_commands`。其余夹具使用已有Docker Redis7；integration默认将自己创建的隔离PostgreSQL交给区域夹具，不打印DSN。
- 图片测试隔离DATA_DIR并关闭relay；历史12个PNG/1164bytes保留、不提交，精确拒删记录见DECISIONS。
- 协议/区域/Live验收边界和失败历史见DECISIONS；历史结果不代替新源码或生产验收。区域竞争使用就绪SQL池。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

D5跨请求估计/跨协议比较待验。原28项20 done，D2/D3/D4/D5进行中，D6–D9待验；336回退须排空并留账单。

## 外部等待与清理

- D2 unknown由供应商/接入方补原请求关联查询契约；D4补完整模型/fast适用证据后恢复对应验收。D9由管理员提供生产执行授权与排空窗口。
- 原PG14补验已释放：原repository SQL 14顶层/93含子例通过，零失败/跳过（cache-migration-pg14.jsonl）；不代表全仓或生产。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删：`frontend/tmp/availability-coverage-ui/`、`backend/tmp/cache-observability/`及`backend/tmp/output-rate/`中的旧夹具/缓存；由工作区拥有者处理，详见DECISIONS。
- 历史`backend/tmp/ws-turn-metrics/vite-cache`拒删11文件/2886386bytes待拥有者手动删除，不换工具重试。本轮浏览器及5186夹具已关闭，无新增Vite缓存；历史PNG原位保留、不提交。
- 历史Go缓存拒删96份/32.20GiB、删除0，清单cache-reclaim-blocked.json；前序压缩及SHA256证据见DECISIONS，不换工具重试。
- 历史TEMP下sub2api-chat-wire及-final-static/-sql/-static约69MiB拒删保留；本轮复用-sql隔离PG17，不重试删除。
- 本轮TEMP下sub2api-protocol-rate的18文件/700.05MiB删除被宿主blocked by policy拒绝，删除0；PG17/浏览器已停止，拥有者手动清理。
- TEMP/sub2api-runtime-ttft仅余空目录（0文件/0进程引用）；删除命令被宿主blocked by policy拒绝，未换工具重试，由拥有者手动移除。
