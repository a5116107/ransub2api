# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：M11 补齐管理端用户用量真实统计；父任务进行中，禁止CI/推送/部署/生产迁移。
证据：GetUserUsageStats 复用 UsageLogRepository 聚合查询，按 day/week/month/year 窗口返回真实请求、token、成本和平均时延。
验证：service usage unit race 两轮退出0、1.231s；admin handler race 两轮退出0、1.216s；server默认编译1.407s；unit vet退出0；gofmt无输出。
下一动作：继续 D2 区域归属/unknown 核实及 D3 其他未观测出站；M11 待提交。
外部等待：生产升级/回滚前排空旧任务与索引，需生产管理员执行；历史目录由拥有者清理。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-09。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 用户授权本地实施、修复、相关清理与验证、本地提交；真实采购价格不在本期范围。生产执行仍需对应授权。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置覆盖、倍率口径、共享Key缺口与最小补丁边界已记录 |
| D2 | in_progress | 已覆盖D2.1–D2.4及多种出站准入；异步视频Redis和SQL主节点持久交接、终态释放/恢复已验；区域从节点、跨区域归属、unknown自动核实及其他来源待验 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 最新availability、故障分类、BPS纯文本探测、Grok语音HTTP/WS握手前终检及Codex harvest Edge HTTP/WS共享准入已验证；其余能力、托管连接、未观测传输及WS作用域待补 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 6项未引用辅助符号与死计数器删除，unused 6→0 |
| M2 | done | 错误检查247→0；1072顶层用例通过、6既有skip保留，vet与重复回归通过 |
| M3 | done | WinSock拒绝/不可达识别；超时/重置不误判；真实POST及代理/WS组合5轮通过 |
| M4 | done | 三处原子持久化收敛；22项Windows失败修复，双平台回归/三包lint/vet通过 |
| M5 | done | 生产依赖与测试夹具边界明确，三处兼容点说明；六包lint 0及禁止依赖反向验证通过 |
| M6 | done | 删除无调用且假成功的 ProxyService.TestConnection；引用搜索0、gofmt、service编译及vet通过 |
| M7 | done | 删除无调用且假成功的 AccountService.TestCredentials；引用搜索0、gofmt、service编译及vet通过 |
| M8 | done | 分组统计移除 mock 零值；API Key 总数/活跃数分页准确，历史请求和标准成本复用 dashboard 数据，handler race/vet通过 |
| M9 | done | 兑换码统计移除 mock/TODO；真实分页统计状态、类型和已使用面值，admin race、service编译与vet通过 |
| M10 | done | 删除无调用且假成功的 AdminService.RefreshAccountCredentials；接口/实现/stub引用为0，admin race、service编译与vet通过 |
| M11 | done | 用户用量移除 mock 零值；接入既有 UsageLogRepository 聚合、周期校验和缺依赖失败关闭，service/admin race、server编译与vet通过 |
| D4 | in_progress | 固定售价、严格请求新鲜策略、7平台及BPS独立发送权威终检已验证；仍依赖D2、区域传播和模型/fast来源 |
| D5 | pending | timing与缓存分层/冷启动估计；依赖D2/D3 |
| D6 | pending | 分配与缓存保护；依赖前置指标与准入 |
| D7 | pending | 前端配置与决策解释；依赖策略契约 |
| D8 | pending | admin/跨层SQL、WS绑定、重试截止及批量资金/竞争/回滚16项夹具已验证；其余集成/升级/全局CLI待验收，不运行CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用项目Go1.27.3；不降低go.mod。lint工具`backend/bin/task-d2-golangci-lint.exe` v2.15，完整包、unit标签、并发2与原规则；service预算12分钟，其余8分钟。
- 真实Redis集成优先使用进程PATH；本机Windows Redis 3.0不支持共享额度Lua脚本所需的`redis.replicate_commands`，集成夹具自动切换已有Docker Redis 7；真实SQL须进程环境指向隔离PostgreSQL，不打印DSN。
- 图片测试使用隔离DATA_DIR、关闭relay；既有`backend/internal/service/data/`12个相同测试PNG共1164bytes，历史精确删除曾被策略拒绝（DECISIONS），保留且不提交。
- 既有HTTP/compat归属、区域共享Key与Live的实际版本、完整/局部验证边界及失败历史见DECISIONS；不代替本轮WS源码或生产升级验收。区域竞争使用就绪SQL池，不计冷建连时延通过。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

D2继续异步视频区域从节点/跨区域归属、无任务ID的unknown自动核实，以及其他非标准鉴权/独立出站；再补D3其他能力、D4区域传播及D5–D9。BPS未观测能力仍保守拒绝；旧批量缺来源任务保持等待。共享策略启停、变更或版本切换前须排空旧视频和批量任务；受限Live沿4.4.2，区域沿原SQL预算。
模型/服务档隔离只证明请求续接边界，不证明供应商缓存实际存活、内部不降级、跨档同价或高缓存率。API Key长历史仍受既有12条尾部及工具边界guard约束。

## 外部等待与清理

- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 策略拒绝清理的历史产物仍保留：`backend/bin` lint 三文件、`backend/tmp/admin-key-pg-d8`、`availability-non5xx-regression`、`regional-key-pg`（55449/PID12708）、`live-key-verification`、`ws-turn-state-verification`、`gemini-route-pg`、`batch-key-verification`、`frontend/tmp/channel-monitor-ui-fixture` 及 `C:\Users\Administrator\AppData\Local\go-build-sub2api`。相关 schema/实例状态和大小详见 DECISIONS；由拥有者按准确路径手动停止/删除，不换工具绕过策略。
