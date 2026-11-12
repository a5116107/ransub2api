# 项目状态

## 恢复检查点

<!-- resume:start -->
任务：D5/D7选定延迟未知的API/热图子验收已完成。
授权：原H盘本地修复/验证/清理/提交；不CI/推送/部署。
源码：cab57c6后续原位修复；14项验证输入一致，调度/账单不变。
验证命令：专项十轮60/140、service662/3193、admin7/37；四包vet/lint及Linux均0。
验证结果：UI48/类型/ESLint/构建/中英390px及503恢复通过；lint为0 issues。
验证来源：backend/tmp/protocol-cache/latency-evidence-*。
下一动作：等待D2–D8缺失契约/能力/缓存证据；D9待生产授权与窗口。
尝试：42
阻塞：本轮TEMP删除在启动前被策略拦截，236文件/9455936bytes保留供拥有者处理。
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
| D2 | in_progress | D2.1–D2.4、多出站/持久交接、三节点及视频身份/双预算/原节点结算/竞争、336排空/跨库已验；unknown待原请求关联契约 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 最新状态/托管/区域文本准入、带内故障/空流及缺终态/间隔超时已验；race447/3151、26场景SQL、四包vet/lint零诊断、Linux通过；其他能力待验 |
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
| D5 | in_progress | 协议/缓存/TPS/TTFT及入站/线路/effort隔离已验；终检和选定延迟未知/null已验，本轮service662/3193、四包lint0/vet/Linux通过；跨协议/会话估计待证据 |
| D6 | in_progress | 普通保留/硬准入及门票16场景已验，扩大race668/3225、四包vet/lint0及Linux通过；软迁移仍须前缀/候选缓存域与净收益证据 |
| D7 | in_progress | 原表单/API会话开关及旧客户端兼容已验；选定延迟未知/精确分位、UI48/类型/lint/构建、中英键盘/390px/503恢复通过；完整解释待依赖契约 |
| D8 | in_progress | 原admin/SQL、WS/资金/竞争/回滚已验；双网关/共享Key及升级race186/626、专项三轮9/21、四包vet/lint0；全局CLI/混版待依赖证据，不CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用Go1.27.3、原go.mod和lint规则；`backend/bin/task-d2-golangci-lint.exe` v2.15，unit/integration标签、并发1–2，lint 8GiB/18分钟。
- 真实Redis竞争测试按PATH启动；本轮前置已有`H:/Root/tools/redis/5.0.14.1`，系统Redis3缺`redis.replicate_commands`。其余夹具使用已有Docker Redis7；integration默认将自己创建的隔离PostgreSQL交给区域夹具，不打印DSN。
- 图片测试隔离DATA_DIR并关闭relay；历史12个PNG/1164bytes保留、不提交，精确拒删记录见DECISIONS。
- 协议/区域/Live验收边界和失败历史见DECISIONS；历史结果不代替新源码或生产验收。区域竞争使用就绪SQL池。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

本轮D5/D7未知延迟子验收完成；原28项20 done、7部分验收、D9待生产。当前无已就绪的剩余父验收，缺失证据释放后按下列依赖继续。

## 外部等待与清理

- D2 unknown由供应商/接入方补原请求关联查询契约；D4补完整模型/fast适用证据后恢复对应验收。D9由管理员提供生产执行授权与排空窗口。
- D3由用户确认首批非文本的上游/模型/能力组合，再定义独立观测和成功条件；已异步询问，现有文本通过不授权工具/媒体/语音。
- D5/D6由供应商/接入方补可信可复用前缀、候选缓存共享/存活证据；验证软迁移净收益后恢复D7/D8依赖验收。
- Windows完整 repository 测试缺 `SeCreateSymbolicLinkPrivilege`，Linux 同例已通过；由系统管理员提供可创建符号链接的进程后重跑，未改 skip。
- 历史拒删：`frontend/tmp/availability-coverage-ui/`、`backend/tmp/cache-observability/`、`backend/tmp/output-rate/`及`backend/tmp/ws-turn-metrics/vite-cache`（11文件/2886386bytes）；拥有者手动处理，不重试，详见DECISIONS。历史PNG保留、不提交。
- 历史Go缓存拒删96份/32.20GiB、删除0，清单cache-reclaim-blocked.json；前序压缩及SHA256证据见DECISIONS，不换工具重试。
- 历史TEMP拒删（chat-wire、protocol-rate及其余测试产物/空目录）精确路径见DECISIONS；拥有者手工处理，不重试、不计释放。
- 本轮TEMP：`C:/Users/Administrator/AppData/Local/Temp/sub2api-latency-evidence`，236文件/9455936bytes；无活跃引用或重解析点，删除在启动前被策略拦截，拥有者手工处理，不重试、不计释放。
