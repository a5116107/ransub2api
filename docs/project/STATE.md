# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D2受限共享Key未知Authorization鉴权子项本地完成；不CI/推送/部署。
证据：基线defec63；原受限HTTP混合认证实际发出并得200，修复后原生WS/主从SQL均按准入拒绝。
验证：race（顶层/子场景）config 9/57、service 33/27、repository 28/31、WS兼容14/33均0失败跳过。
下一动作：多peer及其他认证/独立出站合同继续验收；三包lint 0 issues、vet/编译退出0，旧标记0引用。
外部等待：unknown核实待供应商关联查询契约；生产排空待管理员；历史目录待拥有者清理。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-10。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 用户授权本地实施、修复、相关清理与验证、本地提交；真实采购价格不在本期范围。生产执行仍需对应授权。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置覆盖、倍率口径、共享Key缺口与最小补丁边界已记录 |
| D2 | in_progress | D2.1–D2.4、未知Authorization失败关闭、多出站准入、视频持久交接/恢复、mTLS目录及原节点查询已验；多peer、unknown核实及其他来源待验 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 最新状态、流完整性、托管隔离/预算轮转及原生WS文本检测→SQL→逐轮准入/停调恢复已验证；其他未观测协议/能力待验 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 6项未引用辅助符号与死计数器删除，unused 6→0 |
| M2 | done | 历史247项及本轮integration101条诊断清零，123处断言已检查；四包完整lint零诊断、240顶层race零失败/跳过，探测指纹及权威时钟夹具已验 |
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
| D5 | pending | timing与缓存分层/冷启动估计；依赖D2/D3 |
| D6 | pending | 分配与缓存保护；依赖前置指标与准入 |
| D7 | pending | 前端配置与决策解释；依赖策略契约 |
| D8 | pending | admin/跨层SQL、WS绑定、重试截止及批量资金/竞争/回滚16项夹具已验证；其余集成/升级/全局CLI待验收，不运行CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用项目Go1.27.3；不降低go.mod。lint工具`backend/bin/task-d2-golangci-lint.exe` v2.15，完整包载入、unit/integration标签、按内存并发1–2与原规则；service预算12分钟，其余8分钟。
- 真实Redis竞争测试按PATH启动；本轮前置已有`H:/Root/tools/redis/5.0.14.1`，系统Redis3缺`redis.replicate_commands`。其余夹具使用已有Docker Redis7；integration默认将自己创建的隔离PostgreSQL交给区域夹具，不打印DSN。
- 图片测试使用隔离DATA_DIR、关闭relay；既有`backend/internal/service/data/`12个相同测试PNG共1164bytes，历史精确删除曾被策略拒绝（DECISIONS），保留且不提交。
- 既有HTTP/compat归属、区域共享Key与Live的实际版本、完整/局部验证边界及失败历史见DECISIONS；不代替本轮WS源码或生产升级验收。区域竞争使用就绪SQL池，不计冷建连时延通过。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

D2目录、终态归属、原节点查询及未知Authorization失败关闭已验（迁移332仅隔离夹具）；任务/生成额度留在原节点，不实施权威迁移。继续多peer、其他自定义头/独立出站；无ID unknown核实待供应商关联查询契约。主从仅认领自身权威，不凭目录接管或重占槽。当前固定peer目录128条/源、30秒期限，缺失/过期/冲突本地拒绝。D3其他未观测能力、D4区域传播及D5–D9保留。BPS未知能力保守拒绝，旧批量缺来源任务等待。共享策略启停/变更/版本切换前须排空旧视频及批量任务；受限Live沿4.4.2，区域沿原SQL预算。
模型/服务档隔离只证明请求续接边界，不证明供应商缓存实际存活、内部不降级、跨档同价或高缓存率。API Key长历史仍受既有12条尾部及工具边界guard约束。

## 外部等待与清理

- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 本任务的 `frontend/tmp/availability-coverage-ui/` 临时界面夹具删除被自动审批拒绝（blocked by policy）；保留原位，由工作区拥有者处理，不尝试绕过。
- 策略拒绝清理的历史lint文件、验证目录、缓存和旧进程保留且不提交；准确路径、schema、端口/PID及大小见DECISIONS。由工作区拥有者核实后手动停止/删除，不换工具绕过策略。
