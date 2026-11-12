# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D7候选首字/支付按需边界本地验收完成；父项与清理待外部。
授权：原H盘本地修复/验证/清理/提交；不CI/推送/部署。
源码：401b6f9后候选不再补0ms；Airwallex从公共包移入独立动态包。
验证命令：Vitest112项及支付21项、ESLint、vue-tsc/Vite构建、Chromium。
验证结果：测试/类型/lint/构建退出0；中英390px刷新及无支付外站请求通过。
验证来源：backend/tmp/protocol-cache/priority-candidate-latency-*。
下一动作：等待下列外部事实/生产授权；本轮临时目录由拥有者手工删除。
尝试：51
阻塞：父项事实/生产授权待补；清理CreateProcess前被策略拒绝，删除0。
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
| D2 | in_progress | D2.1–D2.4、多出站/持久交接、三节点及视频身份/双预算/原节点结算/竞争、336排空/跨库及原节点期限恢复已验；unknown待原请求关联契约 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 状态/托管/区域文本准入、带内故障/空流/缺终态/间隔超时已验；race447/3151、26场景SQL、四包vet/lint0、Linux通过；其他能力待验 |
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
| D5 | in_progress | 协议/缓存/TPS/TTFT及入站/线路/effort隔离已验；选定延迟未知/null已验，service662/3193、四包lint0/vet/Linux通过；跨协议/会话估计待证据 |
| D6 | in_progress | 普通保留/硬准入及门票16场景已验，扩大race668/3225、四包vet/lint0及Linux通过；软迁移仍须前缀/候选缓存域与净收益证据 |
| D7 | in_progress | 并发保存/409、重算及候选缺失/无效延迟已验；UI112+支付21、类型/lint/构建/中英390px刷新通过；完整解释待契约 |
| D8 | in_progress | CLI/三PID/30请求/重启/额度/升级及备份字段已验；配置并发race299/707、SQL三轮12/90、vet/Linux/lint0；其他能力/完整混版待验 |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- Go1.27.3、原go.mod/lint规则；`backend/bin/task-d2-golangci-lint.exe` v2.15，unit/integration标签、并发1–2，lint 8GiB/18分钟。
- Redis竞争测试用PATH既有`H:/Root/tools/redis/5.0.14.1`；Redis3缺`redis.replicate_commands`。其他夹具用Docker Redis7；integration将自建PostgreSQL交区域夹具，不输出DSN。
- 图片测试隔离DATA_DIR/关闭relay；历史12个PNG/1164bytes保留、不提交。
- 协议/区域/Live验收边界和失败历史见DECISIONS；历史结果不代替新源码或生产验收。区域竞争使用就绪SQL池。
- 不增平行实现/台账；运营组不作Key额度键/缓存域证据。C盘effb8271不计主线，开发统一H盘。

## 下一项工作

D7候选展示与支付按需边界子项完成；原28项20 done、7部分、D9待生产。其余等待下列证据/授权。

## 外部等待与清理

- D2 unknown由供应商/接入方补原请求关联查询契约；D4补完整模型/fast适用证据后恢复对应验收。D9由管理员提供生产执行授权与排空窗口。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6待供应商/接入方补可信前缀、候选缓存共享/存活证据；验证迁移净收益后恢复D7/D8依赖验收。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删：`frontend/tmp/availability-coverage-ui/`、`backend/tmp/cache-observability/`、`backend/tmp/output-rate/`及`backend/tmp/ws-turn-metrics/vite-cache`（11文件/2886386bytes）；拥有者手动处理，不重试，详见DECISIONS。历史PNG保留、不提交。
- 历史Go缓存拒删96份/32.20GiB、删除0，清单cache-reclaim-blocked.json；前序压缩及SHA256证据见DECISIONS，不换工具重试。
- 历史TEMP拒删：chat-wire/protocol-rate、latency-evidence/cli-processes/priority-backup/recompute-latency及其余测试产物；精确路径/大小和拦截证据见DECISIONS，拥有者手工处理，不重试/移动、不计释放。
- 本轮`H:/Root/DevCache/tmp/sub2api-candidate-latency`为239文件/9578704bytes，无重解析点/活跃引用；递归清理在启动前被策略拒绝，删除0，拥有者手工处理，不重试/移动。浏览器复现脚本仍在其中，最小验证证据留原protocol-cache。
