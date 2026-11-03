# 当前有效决策

更新日期：2026-11-02。

本次按用户“统一文档、移除过时内容”的要求归并决策记录，仅保留当前有效结论。
后续新决策追加记录；技术建议与未确认参数在 [设计文档](monitor-driven-scheduling-design.md) 中讨论。

| ID | 已确认决策 | 依据与影响 |
| --- | --- | --- |
| ADR-001 | 使用最新有效可用状态准入，历史可用率不代替当前状态 | 用户明确纠正统计口径；隔离后继续独立复测 |
| ADR-002 | 固定下游售价，在允许的上游分组倍率内自动选择；无合格路线明确返回 | 用户确认固定售价与严格范围；示例倍率不写死 |
| ADR-003 | 真实采购价格、实付对账和货币级优化本期延期 | 用户最新范围调整；不作为本期实现依赖 |
| ADR-004 | 指标不限于截图，涵盖首字/TTFT 等完整体验与质量 | 用户补充；质量独立判断，动态指标不相互抵消硬条件 |
| ADR-005 | 显式考虑同组账号切换的缓存损失 | 用户补充；保护健康会话，软迁移需评估重建代价 |
| ADR-006 | 设计需便于跟进原 sub2api 仓库代码更新 | 用户澄清“同步上游”含义；优先既有能力和局部扩展 |
| ADR-007 | 当前仅设计与文档整理 | 用户要求先规划讨论；实施、迁移、提交与部署未包含在本次授权 |

这些决策不代表已确认具体检测间隔、权重、状态有效期、恢复并发或迁移阈值。

## 2026-11-02：本地实施授权与 D1 结论

- 用户授权推进本地任务、修复阻塞、相关清理并执行本地验证，明确不进行 CI；此前仅设计的边界被本次授权更新。
- 提交、生产迁移与部署仍需对应授权；真实采购价格保持延期。
- D1 核对确认现有分组利润配置可以表达首期倍率上限，不新增重复上限字段。
- 优先复用原质量隔离、硬黏性、调度评分和并发；共享额度当前按 Account ID，不误称重复 Key 自动共享。
- 新增 D4.1 独立修复范围：已有倍率门的槽后刷新双故障应拒绝准入；不全局改变未启用组的行为。
- 解除主题文档被全量忽略的维护阻塞：仅将本目录三个指定 Markdown 文件加入例外，不执行暂存或提交。
- D4.1 使用共享终检函数原位修复：最终账号刷新报错或为空返回 `profit_account_refresh_failed`；沿用原槽位释放、候选排除、重选预算及刷新失败观测，不新增并行路径。
- 缓存故障但数据库回源成功仍复核最新对象；无倍率门请求维持原行为。装门前分组读取失败的放行风险留在完整 D4 验收范围，不将局部修复等同于全链路严格保证。
- M1 清理经全仓引用检索及 lint 双重确认的 6 项未使用测试符号，并删除无人读取的计数器；保留路由 golden 对比与实际测试断言，相关回归通过。
- 本机旧 lint 的构建版本和导出格式均不满足 Go 1.27；使用本机缓存的 v2.15.0 源码重新构建完成检查，不改项目 Go 版本、依赖或 lint 规则。
- 完整 lint 清理后仍报告 76 条既有测试问题，本次新增/修改行告警为 0；将全量问题纳入 D8 的未完成范围，不以差异检查替代全量通过。

## 2026-11-02：M2 本地验收修复与 M3 Windows 代理兼容

- M3 从真实 loopback 拒绝连接回归失败进入修复：WinSock 返回的 10061/10065/10051 不等于 Go 的 POSIX errno 别名，原分类漏判持久故障；仅在 Windows 增加类型错误链识别，不依赖英文错误文案。
- 不改代理链的安全前提：只有 HTTP 尚未获得连接、请求未发送且可重放时才换路；超时/连接重置维持瞬时分类，票据绑定、请求已发送和备用链预算保护保留。
- M3、代理完整矩阵、时序与 WS 生命周期组合本地回归连续 5 轮通过：`ok github.com/Wei-Shaw/sub2api/internal/service 140.262s`。未部署线上，也未执行 CI。
- 测试中的连续 `time.Now()` 可能共享平台刻度：Ollama 先复现“夹具没有不同代际”，再由捕获 reset 构造明确的新代际；质量事件明确晚于已完成计划，快照测试显式建立新观察顺序。不改生产 CAS 或放宽原断言。
- 取消用例通过信号固定“处理返回后再尝试未来输出”，避免 read-ahead 把夹具写完而使新错误断言失真；保留 usage、取消、join 与无重放验证。
- 类型断言统一为独立、无 service/repository 依赖的测试辅助子包；错误类型会终止测试，并验证 map 身份、接口、typed nil 和错误诊断，不添加并行生产路径。
- 全量 Windows 回归还暴露目录 `Sync: Access is denied`、权限 `0600` 与 `0666` 不等价及区域快照持久化失败；不以跳过测试、禁用规则、吞掉持久化错误或放松安全权限来取得通过。原始命令与剩余状态由 STATE.md 保留。
- 默认 lint 报告限额曾隐藏同类诊断；后续状态使用未限量结果。跨层测试依赖与有意的 nil-context/历史密文兼容用例不通过代理辅助包或 blanket exclude 隐藏。

## 2026-11-02：M2 验收与图片夹具泄漏修复

- 受影响顶层用例验收：handler 91 pass；service 两批分别 493 pass、488 pass/6 个既有 skip，共 1072 pass/6 skip，退出码均为 0；对应包 `go vet -tags=unit -p 2` 退出码 0。未运行 CI。
- 未限量 lint 命令：`./bin/task-golangci-lint.exe run --build-tags=unit --timeout=12m --concurrency=2 --allow-parallel-runners --max-same-issues=0 --max-issues-per-linter=0 ./internal/service ./internal/handler ./internal/testutil/assertions`。实际退出码 1，报告 `14 issues: depguard 11, staticcheck 3`；errcheck、govet、unused、gofmt 均为 0，不宣称全量通过。
- 残余 depguard 是测试直接依赖 repository/redis；SA1012 两处是 nil-context 兼容边界，SA1019 一处是历史 payment 密文夹具。保留断言和规则，不用包装依赖或泛化豁免掩盖问题。
- `go test ./internal/testutil/assertions -count=1` 实际结果 `ok 0.381s`；辅助包不依赖 service/repository。
- `TestExcelBPSImagePolicyForwardCompactAndContinue` 过去未设置 DATA_DIR，relay 回落到源码 `./data`，也未注册关闭。现原位补充 `t.Setenv("DATA_DIR", t.TempDir())` 与 `t.Cleanup` 关闭，未更改生产默认目录或添加忽略规则。
- `go test -tags=unit -p 2 ./internal/service -run '^TestExcelBPS.*Image' -count=3` 实际结果 `ok 2.487s`；源码图片文件计数 `12 -> 12; new artifacts: 0`。旧 12 个 PNG 均为 97 bytes、相同测试时间和 PNG 签名，共 1164 bytes；精确范围删除被执行器策略拦截，文件保留，未通过其他工具重试。
- Windows service 原始完整回归：10365 pass、27 个顶层 fail、11 skip（331.925s）；其中 5 项由本轮根因修复并通过连续 5 轮组合回归。仍有 22 项涉及区域同步、traffic delivery 与安全文件权限，不把受影响范围通过等同于全量通过。保留原始错误：`sync ...: Access is denied.`、`cannot persist regional sync state`、权限 `expected 0x180, actual 0x1b6`。可用 `go test -tags=unit -p 2 ./internal/service -run '^(TestRegional|TestTrafficDelivery|TestSafetyPromptStoreRetentionAndPermissions)' -count=1` 复现对应范围。

## 2026-11-02：同一工作区 Linux 完整本地回归

- 没有安装另一套 Go、创建副本或工作树；使用现有 Go 工具链设置 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0`，运行 `go test -c -tags=unit -o bin/task-service-linux.test ./internal/service`，随后恢复命令环境变量。
- WSL 从同一挂载工作区的 `backend/internal/service` 执行 `env GOMAXPROCS=2 ../../bin/task-service-linux.test '-test.v=true' '-test.parallel=2' '-test.timeout=15m' '-test.count=1'`。实际结果：`WSL service exit=0 elapsed_seconds=368.815`，最终 `PASS`。
- 主测试程序有 10403 个顶层启动和 10403 个对应结果：10393 pass、10 个既有 skip、0 fail；不存在启动后无结果的顶层用例。
- 内存保护测试有意运行两种会失败的负向子进程，父用例 `TestInflightEstimate_MemoryMeasurementControls` 要求这些进程报错并检查原始 8 MiB 断言；日志中的两段子进程 `--- FAIL` 不代表主套件失败，未删除或放宽控制测试。
- Windows 上尚未通过的 22 个区域持久化、traffic delivery、文件权限用例，在 Linux 上全部通过。Windows 适配仍未完成，不以平台切换宣称 Windows 已修复；未吞掉目录 Sync 错误、放松安全权限或跳过这些用例。
- 最后一次未限量 lint 仍为 14 项既有诊断（depguard 11、staticcheck 3），退出码 1；图片测试修复没有引入额外诊断。`go vet` 先行完成且退出码 0。
- 验证证据归入本文件后，精确删除已结束的 11 个 `backend/bin/task-*` 普通文件，共 376466989 bytes（359.027 MiB）；其中原有任务残留 136696729 bytes，新建交叉编译程序与 Linux 日志 239770260 bytes。没有删除整个 bin、全局缓存、用户数据或其他任务资源；清理后 `bin/task-*` 数量为 0。此前被执行器拦截的 12 个小图片夹具保持原样，后续未换工具重试。

## 2026-11-02：D4.2 分组配置不可用准入修复

- 先将既有共享 Gateway 的卸门放行测试改为严格准入，并增加 OpenAI HTTP/WS 轮次回归。旧实现实际退出码 1：三种入口均在 `require.NotNil(gate)` 失败，复现“配置读取失败后没有门”。
- 原位扩展既有请求门表达配置不可用，不新增并行调度器、配置表、全局开关或采购价格功能。实际读取报错、返回空对象或 ID 错配时拒绝，返回 `profit_group_config_unavailable`；不能用 threshold=0 代替，否则免费账号会绕过。
- 共享 Gateway/OpenAI HTTP/WS 复用同一否决函数；选路结果携带门，槽后终检优先保留配置故障原因，不以账号刷新错误覆盖；增加原采样观测中的 `veto_group_config_unavailable_total`。
- 已确认关闭或不适用的组、显式媒体/元数据范围保持；WS 后续轮次重新读取实际调度组，恢复或明确关闭后解除对应请求门，不改计费父组、冻结时间、旧轮次或缓存绑定代码。OpenAI 无调度快照的内部调用兼容边界仍保留，不将其宣称为生产严格范围保证。
- 新回归放入原有 unit 标签测试文件；原有无标签测试保持原位，未通过新增标签减少覆盖。配置失败、免费账号、空/错组、跨组门重放、恢复、终检与计数专项连续 3 轮通过：`ok github.com/Wei-Shaw/sub2api/internal/service 14.139s`。
- `go vet -tags=unit -p 2 ./internal/service ./internal/handler` 通过；handler 全量 1100 pass、2 个既有 skip、0 fail，退出码 0。首次未限量 lint 仍为既有 14 项（depguard 11、staticcheck 3），退出码 1。
- 首轮 Linux 全量真实结果：10382 pass、15 fail、10 skip，10407 个顶层启动均有结果，退出码 1（379.909s）。其中 14 个原有调度/票据/水合测试缺少分组读取依赖，现补入确切 ID、显式关闭利润门的既有分组仓库 mock，保留所有原账号、模型、槽位、票据和缓存断言，不放松生产失败策略。
- 另 1 个隐私仓库故障用例原本仍期待选择 fallback；依据严格配置范围，改为断言包含配置不可用原因且没有选中账号，保留原隐私约束、账号不被标错、运行态不被污染及 response binding 不丢失断言。该调整强化拒绝而不是降低安全约束。
- D2 及 D3–D9 的主线依赖继续保留，不以 D4.2 局部修复代表共享 Key、最新可用调度或全局设计已完成。
- 补齐配置夹具后的组合专项 `go test -tags=unit -p 2 ./internal/service -run '(ProfitControl|ProfitGate|ProfitAdmission|GatewayTokenRequestPricing|CodexTicketScheduler|AccountModelRouteDBRecheckFailsOverToOwner|LegacyAccountModelRouteDBRecheckFailsOverToOwner|SessionStickyRateLimitedAccountFallsBackToFreshCandidate|SessionStickyDBRuntimeRecheckSkipsStaleCachedAccount|DBFreshGroupRecheckWaitsOnValidAccount|UsesWSPassthroughSnapshotFlags|PreviousResponseHonorsGroupAndRequiredPrivacy|DBRuntimeRecheckRateLimitedMiss|HydratesSelectedAccountFromSchedulerSnapshot|PreservesAccountRequestID)' -count=3` 退出码 0，实际 `ok 1.714s`。默认标签专项 `go test -p 2 ./internal/service -run '(ProfitControl|ProfitGate|ProfitAdmission)' -count=1` 退出码 0，实际 `ok 1.643s`；原无标签测试继续执行。
- 六个配置夹具文件调整后，重新执行 `go vet -tags=unit -p 2 ./internal/service ./internal/handler`，退出码 0；最终未限量 lint 仍退出 1，精确报告 `14 issues: depguard 11, staticcheck 3`，与调整前一致。未更改 lint 规则、添加跳过或弱化断言；完整 Linux 回归另以最终进程结果验收。
- D4.2 最终完整验收：恢复环境变量前以 `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOMAXPROCS=2` 执行 `go test -c -tags=unit -p 2 -o bin/task-d42-service-linux.test ./internal/service`，随后由 WSL 从同一工作区的 `backend/internal/service` 运行测试程序，参数 `-test.v=true -test.parallel=2 -test.timeout=15m -test.count=1`。实际 `Linux service exit=0 elapsed_seconds=438.5826342`，最终 `PASS`。
- 按行统计顶层结果：10407 个启动、10397 pass、10 个既有 skip、0 fail；启动/结束多重集合完全一致，无未完成用例。首轮 15 项失败已解除；未扩大超时、添加 skip、吞掉持久化错误或降低文件权限断言。D4.2 标记 done，完整 D4 及 D2–D9 仍按原依赖保留。
- 最终证据记录后，按预览清单精确删除 6 个已结束的 D4.2 测试程序与日志，共 313104916 bytes（298.600 MiB）；删除命令退出码 0，复核当前 `backend/bin/task-*` 普通文件数量为 0。未删除整个 bin、全局缓存、源码或此前被执行器拦截的 12 个图片夹具；无提交、部署、生产迁移或 CI。

## 2026-11-03：M4 跨平台私有文件持久化

- Windows 原故障命令 go test -tags=unit -p 2 ./internal/service -run '^(TestRegional|TestTrafficDelivery|TestSafetyPromptStoreRetentionAndPermissions)' -count=1 实际退出 1（16.336s）：区域快照持久化失败、流量暂存目录 Sync: Access is denied，以及 POSIX 0600 与 Windows 0666 模式投影不等价。没有改为 skip、吞掉提交错误或允许公开权限。
- 原位移除区域快照、流量暂存、安全事件三处重复写入，统一为 internal/pkg/securefile.WriteAtomic。POSIX 保留临时文件 0600、文件 Sync、rename 及目录 Sync；Windows 使用带受保护 owner-only DACL 的 CREATE_NEW，在写入任何内容前即限制访问，再文件 Sync 和 write-through 替换。提交失败仍返回错误，区域副本继续 fail-closed，流量保留原故障计数/重放，安全事件显示持久化警告。
- 权限断言核对实际平台安全属性：POSIX 仍精确要求 0600；Windows 要求进程用户拥有文件、DACL 不继承、唯一 ACE 只授予 owner full-control。显式负向覆盖 world-access、null DACL、继承和不足权限；不把 Windows mode 0666 当作合格，原权限断言得到真实 ACL 验证。
- 新持久化测试覆盖完整替换/空内容、旧文件权限不被继承、缺少父目录不自举、提交失败旧内容保留和临时文件清理；Windows 独占目标文件时提交必须报错且旧版本不变。go test -p 2 ./internal/pkg/securefile ./internal/testutil/assertions ./internal/regional -count=1 实际通过（0.759s / 0.533s / 1.081s）。
- 区域、流量及安全事件 service 组合 go test -tags=unit -p 2 ./internal/service -run '^(TestRegional|TestTraffic|TestSafetyPrompt|Test.*SafetyPrompt|TestApplySafetyPrompt|TestInjectSafetyPrompt|TestRewriteSafetyPrompt)' -count=3 实际退出 0，ok 23.421s。完整双平台回归、未限量 lint 和任务产物清理另以最终结果验收；没有修改生产配置、部署、提交或运行 CI。

- Windows 完整测试的落盘 JSON 结果已核对：service 10396 pass / 11 个既有 skip（315.086s），handler 1100 pass / 2 skip（144.238s），regional 13 pass（0.683s），securefile 6 pass（0.364s），assertions 3 pass（0.764s）；5 个包最终 Action 均为 pass，所有顶层启动均有结束结果。原 22 个故障用例逐项在日志中确认全部为 pass，不是 skip；先前启动命令的终端句柄已在跨消息时失效，按真实 Go JSON 结果记录，不伪造原终端退出码。

- M4 Linux 最终完整回归：同一工作区交叉编译后由 WSL 执行 service 测试程序，实际 Linux service exit=0 elapsed_seconds=377.2685462；10407 个顶层启动全部有结束结果，10397 pass、10 个既有 skip、0 fail，最终 PASS。securefile / assertions / regional 各连续 3 轮通过，分别为 15 / 9 / 39 个顶层 pass，退出码均为 0。原始 WSL 日志混有 UTF-16 启动诊断和尾部 NUL；保留原始日志，按实际测试行及退出码核对，没有修改测试结果。
- Windows 独占句柄负向测试补齐断言失败时的 defer 关闭后，核心三包再连续 3 轮通过：securefile ok 0.835s、assertions ok 1.198s、regional ok 1.827s，Final core test exit=0。最终 go vet -tags=unit -p 2 ./internal/pkg/securefile ./internal/testutil/assertions ./internal/regional ./internal/service ./internal/handler 实际 Final vet exit=0。
- 核心三包最终未限量 lint 实际 0 issues、Core lint exit=0；含 service/handler 的完整五包未限量 lint 实际 14 issues（depguard 11、staticcheck 3）、exit=1，均为原有测试诊断，M4 所触及文件诊断为 0。规则未改，完整 lint 不称通过。
- 重复写入由 3 处收敛为 1 个共用接口；旧 regional.writeAtomic 及三处 CreateTemp/Sync/rename 写入序列删除。流量损坏文件的隔离 rename 仍由原逻辑负责，未删除有用代码；没有新增 TODO、配置旁路或生产迁移。最后清理仅限预览清单中的 14 个本轮测试程序/日志，共 396790278 bytes，删除后另记录实际结果。

- 验证证据记录后，精确删除预览清单中的 14 个 M4 普通文件，共 396790278 bytes（378.409 MiB），删除命令退出码 0，当前 backend/bin/task-* 为 0。此前 12 个受执行器保护的图片夹具原样保留，没有更换工具删除或移动。M4 已完成；D2、D3、完整 D4、D5–D9 仍按原依赖保留，14 项历史 lint 继续属于 D8。

## 2026-11-03：D2.1 自动同步倍率准入与预演一致性

- 缺口：自动同步只把基础 resolved 倍率写回 Account.RateMultiplier，在线门与预演原来只比较该保存值，忽略当前高峰及同步快照失效。先增加公开准入与预演的行为回归，旧实现实际复现 18 个表内失败场景及 1 个高峰失败场景，go test 退出码 1。
- 在线与预演替换为共用 profitControlAccountRateAt；账号保存倍率的有限值/非负校验由 2 处收敛为 1 处。手工、OAuth 和未同时开启原两个开关的账号保持旧规则；自动同步 API Key 要求 OK、未过期、时间可信、token 单组且原同步值域有效的快照，失败返回 profit_upstream_rate_unavailable。
- U(attemptAt)=max(保存倍率, resolved 倍率×当前上游高峰)，D 仍固定于 pricingAt；不把探测时 effective 倍率固化，也不放低保存的保守倍率。复用原探测和身份 CAS/outbox，不增 schema，不写回账号或下游价格，不在请求内发起网络探测；历史快照无需新增 synced_rate_multiplier 才能使用。New API 的 group 范围声明不冒充 token 证据。
- 原 preview account_rate 继续表示保存倍率，新增 effective_upstream_rate；证据无效时省略该值，给出 rejected_upstream_rate_unavailable 并计入 CLI 汇总。沿用原采样日志增加独立累计计数。删除了旧的“探测状态仅告警”说明和重复比较校验，没有平行实现、旁路配置或新增 TODO。
- 专项覆盖 30 种准入边界、高峰按尝试时刻重算、失效到恢复及计数；handler 覆盖即时取得和等待取得槽位后的拒绝/释放与有效证据准入。执行 go test -tags=unit -p 2 ./internal/service ./internal/handler ./cmd/profit-preview -run 'Test(.*Profit|.*Pricing|.*UpstreamBilling|.*AccountSlot|.*Quota.*|ModelsWithZero|ParsePreview)' -count=3，实际退出码 0，三包分别 ok 9.112s / 21.350s / 9.519s。随后 go vet -tags=unit -p 2 同三包退出码 0。
- 完整本地回归命令：go test -json -tags=unit -p 2 ./internal/service ./internal/handler ./cmd/profit-preview -count=1，实际退出码 0。Windows service 10410 个顶层启动、10399 pass、11 个既有 skip、0 fail（358.465s）；handler 1102 启动、1100 pass、2 个既有 skip、0 fail（165.410s）；CLI 3 启动、3 pass、0 fail（6.049s）。逐项核对启动/结束集合，三包未匹配项均为 0，实际 JSON fail 事件为 0。日志中两条内存子进程 FAIL 属于既有 MemoryMeasurementControls 的预期负向控制，父用例通过；未弱化 8 MiB 断言。
- 真实 CLI 文本与 JSON 均已执行：4 个无密钥的本地样本中，保存 0.4/当前高峰 U=0.8 的账号被阈值 0.7 拒绝，缺失快照账号独立拒绝，手工和未实际同步账号准入。分类汇总为准入 2、利润不足 1、倍率非法 0、同步倍率不可用 1；JSON 对保存/有效倍率、失效时字段省略和四个分类逐项断言 PASS。
- 使用 Go 1.27.3 从本机缓存构建 golangci-lint v2.15.0；对 service/handler/profit-preview 执行未限量 lint，实际退出码 1，精确报告 14 issues（depguard 11、staticcheck 3），与既有结果一致，本次六个修改文件诊断为 0；未关闭规则或把完整 lint 称为通过。
- 仅在现有单一工作区实施，保留已有 113 个修改文件与四类未跟踪路径；本次新增涉及 5 个原未修改文件，另更新原已修改的 openai_profit_control.go 和唯一项目文档。D2.1 完成；D2 的固定线路协议、共享 Key 限额和缓存域，以及 D3、完整 D4、D5–D9 继续按设计依赖保留。未提交、运行 CI、生产迁移或部署。
- 本轮产物清理实际未执行成功：自动审批审核先拒绝带路径校验的普通文件清理，再拒绝明确列出三个绝对路径的 PowerShell Remove-Item，原因均只有 blocked by policy。未改用其他工具删除或移动；在 backend/bin 保留 task-d2-full-tests.jsonl（73297464 bytes）、task-d2-golangci-lint.exe（58705408 bytes）、task-d2-preview.json（2093 bytes），共 132004965 bytes（125.890 MiB），状态中明确记录清理阻塞。三项普通文件均已单独核对，既有 12 个图片夹具共 1164 bytes 原样保留；不把删除意图写成完成结果。

## 2026-11-03：M5 生产依赖与兼容测试 lint 边界

- 逐项核对前轮 14 个既有诊断：11 个 depguard 来自测试夹具使用真实 repository/Redis adapter；2 个 SA1012 刻意覆盖 AcquireOpenAIWSIngressLease 的 nil-context 兼容路径；1 个 SA1019 使用已弃用 Encrypt 生成旧密文，验证现存解密兼容分支。生产错误与有意测试边界分别处理，不删除测试或更换断言。
- 调整范围是 backend/.golangci.yml 的两条生产层依赖规则：仅排除 service/handler 根目录的 *_test.go，原有生产 deny 及原例外保持；其他测试检查、嵌套目录检查及所有 linter 均保持启用。此项明确改变测试导入的规则适用范围，不宣称删除了 11 个错误依赖。两处 nil-context 和一处旧密文调用采用精确行级 staticcheck 例外并解释原因，保留真实覆盖，不修改兼容实现或复制加密算法。
- 第一轮实际报告 11 issues（depguard 11），确认三个 staticcheck 诊断消失；同时发现 depguard 的 glob 语法不能用 service/**/*_test.go 匹配根目录文件，随后改为 service/*_test.go（handler 同理）。第二轮完整 service/handler/assertions 未限量 lint 退出码 0，实际输出 0 issues。
- 反向验证：短暂在生产 service/handler 各加入一个禁止的 Redis 导入，使用 --enable-only=depguard 执行原规则；实际底层退出码 1，精确报告 2 issues（depguard 2），两条分别命中 service-no-repository 和 handler-no-repository。验证程序检查退出码、两个文件路径及计数均正确后输出 Production boundary negative check: PASS。两个 depguard_boundary_check.go 已通过 apply_patch 删除，不保留测试污染。
- 本地相关回归：WS/支付 webhook 用例连续 5 轮退出码 0（ok service 2.833s）；进一步覆盖完整 GetOrderProviderInstance、GetWebhookProvider 和 AcquireOpenAIWSIngressLease 用例连续 3 轮退出码 0（ok service 2.257s）。service/handler/assertions 的 go vet -tags=unit -p 2 退出码 0，实际输出 go vet: PASS。未运行 CI、提交、迁移或部署；使用原单一工作区与现有 lint 程序，没有新增二进制/日志产物。
- 移除反向验证文件后，最终执行 ./bin/task-d2-golangci-lint.exe run --build-tags=unit --timeout=12m --concurrency=2 --allow-parallel-runners --max-same-issues=0 --max-issues-per-linter=0 ./internal/service ./internal/handler ./internal/testutil/assertions ./internal/regional ./internal/pkg/securefile ./cmd/profit-preview，实际输出 0 issues、退出码 0。此结论仅覆盖这六个包，不宣称整个 backend ./... 均已通过。M5 完成；D2 及 D3–D9 的业务功能仍按原依赖保留。
- 最终工作区为原 detached HEAD 的单一 worktree，已有 118 个修改文件全部保留，本轮增加 3 个修改文件（lint 配置及两个测试），合计 121 个修改文件，四类未跟踪路径保持。git diff --check 通过，生产验证文件剩余 0、新增 TODO/FIXME/XXX/shim 0；没有新增任务日志或程序，之前被执行器拒绝删除的 3 项产物仍为 132004965 bytes，未更换工具绕过。

## 2026-11-03：D2.2 New API 固定分组证据与失效恢复

- 根因：原快照仅检查 route_groups 长度不超过 1；声明 group=vip、实际单路由为 default 也使用 vip 倍率并标记 OK。先加入行为回归，在旧源码执行两个新增顶层用例实际退出 1（FAIL service 1.578s），异组真实探测及空组/非法倍率等共 10 个子例失败。初版夹具误把既有探测抖动当作精确 30 分钟，已按原 nextProbeDelay 的 25–35 分钟范围校正；未修改生产延迟。
- 原位修复 newAPISnapshot：单路由必须与声明组精确匹配，跨组重试、多路由、空/auto 分组及非法倍率不产生有效倍率证据。复用 validNewAPIGroupRate 合并 pricing 入口与快照构造的值域检查，移除快照函数未使用的 Account 参数并更新全部调用；替换旧单一多路由测试，没有并行实现。
- 兼容未提供 route_groups 的旧库存，保留 0 倍率的分组展示。group 证据仍不可成为 token 自动同步依据，不改 Account.RateMultiplier 或上游 Key 配置；无效证据替换旧倍率/有效期，但钱包余额继续有效。同组恢复后重新产生有效快照，原身份绑定与 fingerprint 不变。
- 17 类快照边界及真实授权/探测/写回四阶段回归覆盖同组→异组→多组→同组恢复。go test -tags=unit -p 2 ./internal/service -run '^(TestNewAPI|TestUpstreamBilling|TestProfitControlSynced|TestPreviewProfit)' -count=3 实际退出码 0，输出 ok github.com/Wei-Shaw/sub2api/internal/service 2.065s。
- 相关三包回归 go test -tags=unit -p 2 ./internal/service ./internal/handler ./cmd/profit-preview -run 'Test(.*Profit|.*Pricing|.*UpstreamBilling|.*AccountSlot|ModelsWithZero|ParsePreview|NewAPI)' -count=3 实际退出码 0，service/handler/profit-preview 分别 ok 2.755s / 20.334s / 3.176s。go vet -tags=unit -p 2 ./internal/service ./internal/handler 实际退出码 0、无诊断。
- D2.2 完成，D2 整体仍 in_progress；请求级强制选组、重复 Key 共享额度、缓存域和 D3–D9 不以此局部修复冒充完成。设计和状态仅原位更新，不创建第二份任务台账；无 CI、提交、迁移或部署。
- 最终六包未限量 lint 命令为 ./bin/task-d2-golangci-lint.exe run --build-tags=unit --timeout=12m --concurrency=2 --allow-parallel-runners --max-same-issues=0 --max-issues-per-linter=0 ./internal/service ./internal/handler ./internal/testutil/assertions ./internal/regional ./internal/pkg/securefile ./cmd/profit-preview，实际输出 0 issues.、退出码 0。旧快照签名与旧测试名 live 引用均为 0，本次新增 TODO/FIXME/XXX 为 0；git diff --check 退出码 0。
- 最终沿用 detached HEAD 的单一 worktree，原有 121 个 tracked 修改保留，本轮涉及另 4 个源码/测试文件，累计 125 个；四类 untracked 路径 securefile、service/data、assertions、docs/project 保持，状态文档 72 行。未新增任务日志或二进制；此前自动审批审核以 blocked by policy 拒绝删除的三项普通产物仍共 132004965 bytes，另 12 个 PNG 共 1164 bytes 保留，未换工具或移动规避。

## 2026-11-03：D2.3 协议线路身份与旧证据失效

- 根因：探测已使用 api_base_urls、api_protocol、account_mode 解析端点，但共用身份投影及批量清除判断仍仅列 Key/base_url/header。改协议地址或接入模式后，服务层单账号编辑、CRS 合并与 New API 绑定可继续认可旧证据，批量编辑也没有发出清除意图。
- 先加入 12 类线路变更/清空/原值/无关编辑回归，各验证绑定指纹、单账号编辑、CRS 合并、批量编辑。旧源码实际复现 35 个叶子子例失败，命令 go test -tags=unit -p 2 ./internal/service -run '^TestUpstreamBillingProbeRouteIdentityChanges$' -count=1，实际退出码 1、FAIL service 1.552s。单账号夹具使用该 API 真实的完整非敏感凭据替换语义，批量仍使用增量，不把两者混为同一种合并行为。
- 原位把两份重复身份字段列表收敛为 upstreamBillingProbeIdentityCredentialKeys，纳入三个可改变线路的协议字段；所有既有调用自然共用：单账号对比、批量清除、CRS、New API 指纹。数据库全凭据 CAS/事务/outbox 保持，未缩小持久化身份边界，没有新增状态表、迁移或另一套身份实现。
- 单账号相同身份值在服务层保留快照；批量显式提交线路字段（含 null）保守清除；身份变更保留探测/同步开关，没有新快照时原自动同步倍率门拒绝。CRS 不信任远端受管快照。补 3 个真实 SaveNewAPIConfig→GetNewAPIConfig→stale probe 测试，证明三种字段变化后不再显示有效绑定、探测返回 ErrUpstreamBillingProbeIdentityChanged。
- 升级兼容边界：没有新纳入字段的账号指纹不变；已存在这些字段的旧 New API 绑定指纹会失效，必须通过原配置入口重新核验。此项不证明不同协议端点计价/分组或缓存共享，不替代 D2 的请求级固定选择、重复 Key 额度和缓存域。
- 首轮相关 service 回归连续 3 轮实际通过（ok 2.096s）。补齐绑定消费者回归后，执行 go test -tags=unit -p 2 ./internal/service ./internal/repository ./internal/handler ./cmd/profit-preview -run 'Test(UpstreamBillingProbeRouteIdentityChanges|ReconcileCRSUpstreamBillingProbeExtra|.*Account.*(Probe|RateSync)|NewAPI|.*UpstreamBilling|.*Profit|.*Pricing|.*AccountSlot|ParsePreview)' -count=3，实际退出码 0；service/repository/handler/profit-preview 分别 ok 4.122s / 2.055s / 20.085s / 1.829s。go vet -tags=unit -p 2 同四包退出码 0、无诊断。以上为本地验证，不是 CI 或生产数据库演练。
- 最终执行 ./bin/task-d2-golangci-lint.exe run --build-tags=unit --timeout=12m --concurrency=2 --allow-parallel-runners --max-same-issues=0 --max-issues-per-linter=0 ./internal/service，实际输出 0 issues.、退出码 0；结论覆盖完整 service 包，不把它扩大为全后端检查。D2.3 已完成，D2 整体仍 in_progress，D3、完整 D4 和 D5–D9 保留原依赖。
- STATE 原位移除两条已被 M5 解决的历史 lint 状态描述，并把 D2.1 的旧诊断说明指回决策历史，避免已解决问题继续呈现为当前缺口。没有删除历史证据或另建台账。
- 本轮仅修改 admin_account.go、admin_account_upstream_billing_probe_test.go、原已修改的 new_api_authorization_test.go 及三份唯一项目文档；原 125 个 tracked 修改全部保留，累计 127 个，四类 untracked 路径保持。单一 detached worktree；新增 TODO/FIXME/XXX 为 0，未新增日志/二进制/图片产物。此前被自动审批审核拒绝清理的三项任务产物仍为 132004965 bytes，12 个 PNG 仍共 1164 bytes，未重试或换工具规避。无 CI、提交、生产迁移或部署。

## 2026-11-03：D2.4 生效探测/授权地址及扩大验收阻塞修复

- 原转发链使用 api_base_urls 的生效地址，New API 授权、同站点账号、绑定探测与事务保存仍读旧 base_url；原生倍率探测只处理按入站分流，漏掉按模型分流。新增真实请求及事务回归后，旧源码在 6 个叶子子例失败，service/repository 分别 FAIL 1.775s/1.874s，退出码 1。
- 原位共用 upstreamBillingProbeBaseURL 与 NewAPIAccountSite，覆盖 Kimi 入站分流、OpenCode 模型分流和只有 api_base_urls 的账号；验证请求主机、租户路径、鉴权、排除旧站点 peer 及错误授权事务回滚。专项三轮实际通过，service/repository 分别 5.532s/2.155s。此为账号级 Chat Completions 观察目标，不声称其他协议同价、固定选组或缓存共享。
- 扩大回归复现目录夹具的时钟碰撞：相同 UnixNano 命名复用了缓存键，错误结果保留上次成功目录的生产行为使 fallback 阶段读到 Responses。将原测试放入 synctest 固定时钟后稳定复现，再仅把夹具命名改为 UUID；原透传、转换和回落断言保留，生产 stale 策略未改。固定时钟用例连续十轮通过（2.026s）。
- 完整 service/repository lint 首轮报告 30 项（errcheck 29、staticcheck 1）；逐项补检查/明确清理错误处理，保留原断言、并发深拷贝覆盖与参数的真实 [4]any 类型。真实 Redis 夹具原位改为统一 loopback TCP；本机 Redis 3.0.504 不支持 Lua 多字段 HSET，使用 H:/Root/tools/redis/5.0.14.1 的便携 Redis 5，仅在测试进程 PATH 优先选用。下载归档 SHA256 为 018EA18A35876383CBB5F4CD0258ADFC87747CF9D619BCE1CF73A2E36F720CCF（本地计算，不当作发布者签名验证）。未改全局 PATH、系统 Redis 或生产 Lua。
- Ticket 端到端夹具补充明确的关闭倍率门分组及 GetByID/GetByIDLite，实现原 fixture 的已知配置，保留冷/热缓存和 12 并发选号断言。真实 Redis/readiness/Ticket/参数组合三轮通过（2.649s）；repository 受影响组合三轮通过（3.669s）。四包扩大回归三轮全部通过：service 108.055s、repository 5.710s、handler 20.313s、profit-preview 1.907s，退出码 0。
- Windows 四包完整本地测试进一步暴露 repository 旧环境依赖：验证码 SDK 的 NO_PROXY 按 host:port 精确匹配，导致请求走系统代理返回 502；备份锁夹具依赖缺失的 sh；凭据密钥发布对 Windows 目录 fsync 报 Access is denied。验证码仅在测试中绑定本地完整端点；备份夹具改用原测试程序的真实子进程，继续检查 pipe、失败退出和锁释放，不改变生产备份逻辑。
- 凭据生产修复删除重复的私有临时写入、Link 与目录 fsync，复用 securefile 新增的不覆盖原子创建；与原 WriteAtomic 共用写入逻辑。POSIX 保持 Link 和目录 fsync，Windows 创建时即 owner-only DACL、无覆盖 write-through 发布。读取验证已打开文件的权限和文件身份，不能用 Windows mode 位代替 ACL；拒绝暴露/继承/空 DACL，原密文兼容和已有密钥不覆写不变。16 并发创建只允许一个成功，其他返回 os.ErrExist，无残留临时文件；ACL 负向检查、原存储及加解密回归保留。
- 首轮 Windows 完整测试：service pass 326.174s（含子例 23179 pass/11 skip）、handler pass 145.517s（含子例 2749 pass/2 skip）、CLI pass 1.793s（3 pass）；repository fail 17.359s（含父级 13 fail）。修复后运行 repository/securefile/assertions 完整集合，仅命令明确排除缺权限的符号链接夹具，实际分别 ok 22.031s/1.509s/0.729s、退出码 0。不能把此子集说成 Windows 全量通过；原符号链接测试未修改为 skip 或放松断言。
- Windows 当前令牌未分配 SeCreateSymbolicLinkPrivilege，os.Symlink 在准备 fixture 时返回 A required privilege is not held by the client；不修改机器安全策略。用同一工作区交叉编译，经 WSL 实际运行 Test(CredentialEncryption|NewAPI|AliyunCaptchaVerifier|PgDump) 连续三轮及 securefile 全包三轮，均 PASS、退出码 0，包含原符号链接拒绝与 POSIX fsync/权限。Windows 全量验收仍等待系统管理员提供具备符号链接权限的进程，责任与解除条件保留在 STATE.md。
- 最终六包 go vet -tags=unit -p 2 实际退出码 0，无诊断。七包完整 lint 的最终结果待当前命令返回后原位补记，不用此前通过结果替代最后源码的检查。
- 新生成的两份 Linux 测试程序及下载 zip 已按明确普通文件路径清理，实际移除 3 项共 180515955 bytes；H 盘便携运行工具 16 个文件共 48299724 bytes 保留。此前已被自动审批拒绝的三项历史产物未重试；没有新增日志或图片，task-d24-* 剩余 0。保留原未提交改动，当前 tracked 修改 151、四类 untracked 路径不变，单一 detached worktree，无新 TODO/FIXME/XXX，git diff --check 通过；不运行 CI、不提交、不迁移、不部署。
- D2.4 完成，D2 整体仍 in_progress；共享 Key、固定选组与缓存域以及 D3–D9 的原验收没有拆分、改名或豁免。状态文档原位更新到 80 行，设计任务定义仍为唯一入口。

## 2026-11-03：最终静态验收与本地交付授权更新

- 前述待返回的最终七包 lint 已返回：service/repository/handler/assertions/regional/securefile/profit-preview，完整未限量检查实际输出 0 issues.、Final seven-package lint exit: 0；在最后源码修改之后执行。最终六包 go vet 也实际退出码 0。没有把未执行范围或 Windows 权限阻塞表述为通过。
- 用户最新 AGENTS.md 明确 Standing local-commit authorization applies，替换之前的无提交授权边界。沿用同一工作树，从原 detached 基线建立本地分支 work/monitor-driven-scheduling，提交已核对归属的累计调度实施、平台修复、测试清理及三份项目文档；不创建 worktree、不推送、不创建 PR、不运行 CI、不迁移或部署。origin 实际指向 ranxi2001/sub2api，未把该名称当作官方上游或推送授权。
- 提交前暂存区为空；此前 151 个 tracked 修改均为本任务累计差异，新增 securefile/assertions 源码及本目录三份文档属于其必要依赖。受保护 service/data 图片与 bin 产物不进入提交，保持可解释的工作区遗留。STATE 已同步本地交付边界，更新为 81 行；实际提交标识由 Git 保存，避免文档引用自身提交哈希。
