# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D5缓存分层/冷热历史观测子项已验证交付；父项仍进行中。
授权：原H盘工作树，本地修复/验证/提交，不CI/推送/部署。
源码：基线f10bb5c；28项最终清单e8b03b81，见证据目录。
验证命令：requesttiming/service/admin/repository race，四包lint/vet及Linux build。
验证结果：65顶层/547含子例race通过；lint 0 issues；vet/build、前端17例/类型/ESLint及浏览器通过。
验证来源：backend/tmp/cache-observability。
下一动作：D5其余协议/WS timing、TPS与会话迁移冷输入风险；D4模型/fast来源仍开放。
尝试：1
阻塞：D2 unknown待供应商关联契约；生产排空待管理员；历史拒删资源待拥有者。
边界：视频查询转发原节点，生成/双预算/结算不迁移；真实采购价不做。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-11。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 用户授权本地实施、修复、相关清理与验证、本地提交；真实采购价格不在本期范围。生产执行仍需对应授权。

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
| D5 | in_progress | 文本间隔及缓存分层/冷热历史观测子项已交付；本轮65/547 race、PG14、lint/vet/build与前端通过；其余timing/迁移风险待验 |
| D6 | pending | 分配与缓存保护；依赖前置指标与准入 |
| D7 | pending | 前端配置与决策解释；依赖策略契约 |
| D8 | pending | admin/跨层SQL、WS绑定、重试截止及批量资金/竞争/回滚16项夹具已验证；其余集成/升级/全局CLI待验收，不运行CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用Go1.27.3、原go.mod和lint规则；`backend/bin/task-d2-golangci-lint.exe` v2.15，unit/integration标签、并发1–2，lint 8GiB/18分钟。
- 真实Redis竞争测试按PATH启动；本轮前置已有`H:/Root/tools/redis/5.0.14.1`，系统Redis3缺`redis.replicate_commands`。其余夹具使用已有Docker Redis7；integration默认将自己创建的隔离PostgreSQL交给区域夹具，不打印DSN。
- 图片测试使用隔离DATA_DIR、关闭relay；既有`backend/internal/service/data/`12个相同测试PNG共1164bytes，历史精确删除曾被策略拒绝（DECISIONS），保留且不提交。
- 既有HTTP/compat归属、区域共享Key与Live的实际版本、完整/局部验证边界及失败历史见DECISIONS；不代替本轮WS源码或生产升级验收。区域竞争使用就绪SQL池，不计冷建连时延通过。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

D5缓存分层/冷热历史观测子项完成；后续为其余协议/WS timing、输出TPS及会话迁移冷输入风险，未知不补零。D4模型/fast适用范围仍缺来源。原28项20 done、D2/D3/D4/D5进行中、D6–D9待验；D2 unknown待供应商关联契约。视频查询转发原节点，生成/双预算/结算不迁移；336升级/回退先排空并保留账单。模型/档位隔离与历史冷热差值不证明供应商缓存存活或迁移收益；API Key长历史保留原12条尾部及工具guard。本轮未改计费或线上策略。

## 外部等待与清理

- D2 unknown由供应商/接入方补原请求关联查询契约；D4补完整模型/fast适用证据后恢复对应验收。D9由管理员提供生产执行授权与排空窗口。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 本任务的 `frontend/tmp/availability-coverage-ui/` 临时界面夹具删除被自动审批拒绝（blocked by policy）；保留原位，由工作区拥有者处理，不尝试绕过。
- 自动审批拒删的历史资源及本轮`backend/tmp/cache-observability/`浏览器夹具/Vite缓存保留、不提交；路径/大小见DECISIONS，由工作区拥有者手动处理。本轮浏览器和UI服务器已关闭，不换工具绕过删除限制。
