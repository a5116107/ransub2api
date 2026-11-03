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

## 2026-11-03：D4 固定售价准入与拒绝诊断修复

- 沿用 work/monitor-driven-scheduling 的同一工作树，以 01b4be6 为本轮起点，仅保留原有未跟踪 service/data 图片。D2 剩余范围核对时发现直接影响固定售价的验收缺口：原计费按账号 GroupRateMultiplier 调整用户倍率，而利润门与预演没有排除不同附加售价的账号；CLI 输入还会丢弃 group_rate_multiplier。优先修复此项，D2 仍未完成，不改原依赖和验收标准。
- 加入回归后，在旧源码实际复现 8 个叶子失败：service 的 0/折扣/加价/NaN/Inf 五项、handler 等待槽/已得槽两项、CLI 丢失附加倍率一项。命令 go test -tags=unit -p 2 ./internal/service ./internal/handler ./cmd/profit-preview -run 'Test(ProfitControlFixedSaleAccountMultiplier|ParsePreviewInputsPreservesAccountSaleRate|AcquireResponsesAccountSlotProfitRecheck)' -count=1，实际退出码 1，三包分别 FAIL 1.703s/1.677s/2.030s。
- 在既有共用 profitControlAccountRateAt 原位加入固定售价兼容检查：启用门时 Account.UserGroupRateMultiplier() 的有效值必须精确为 1；nil 和原有负值按计费既有默认处理。0、折扣、加价和非有限有效值不参与固定售价调度，独立原因 profit_account_sale_rate_mismatch；不改原计费链、配置、usage 或 U/D，不用增大门阈值允许换路变价。关闭门与显式抑制边界保持。
- Preview 共用同一判定，新增 rejected_account_sale_rate_mismatch 分类并排除合格容量；CLI 补齐 group_rate_multiplier 输入传递及分类总数。原账号倍率/同步快照字段和 JSON 兼容保持。老导出应包含附加售价字段，正式启用前须重新预演候选容量；已有非单位附加倍率账号在启用门时会被拒绝。
- 五平台共用门、免费上游、恢复、WS 轮次冻结时刻、已得槽/等待槽释放且不写响应均覆盖。专项连续三轮实际通过：service 1.432s、handler 0.782s、profit-preview 0.895s。相关利润/定价/RecordUsage/槽位/诊断/CLI 三包扩大回归连续三轮实际通过：service 2.737s、handler 20.167s、profit-preview 1.821s，退出码 0；保留未启用门的账号附加计费原测试。
- 同时发现共享 Gateway 诊断只识别 threshold/invalid-rate，其余利润拒绝落入 default 被计作 eligible。原位补齐 group-config/upstream-evidence/sale-rate 三类，三个真实诊断回归验证 Total=1、Eligible=0 且原因计数=1。详细日志复用既有 summarizeSelectionFailureStats，删除重复的统计格式/参数列表；没有新增平行诊断器。进程采样观测增加独立 veto_account_sale_rate_mismatch_total。
- 真实执行 go run ./cmd/profit-preview 的文本与 JSON 两种入口，标准/加价/折扣三个账号结果为 1 准入、2 个附加售价不符拒绝，模型合格容量为 1；程序校验输出 Actual CLI JSON: 2 sale-rate rejections, 1 admitted account; PASS。仅生成 bin/task-fixed-sale-input.json，演练后精确删除，Fixture remaining: False，未写日志或二进制产物。
- 最终 service/handler/profit-preview 的 go vet -tags=unit -p 2 实际退出码 0；完整未限量 golangci-lint（--build-tags=unit --timeout=12m --concurrency=2 --allow-parallel-runners --max-same-issues=0 --max-issues-per-linter=0）实际输出 0 issues.、Touched packages lint exit: 0。不重复扩大至未修改包，不运行 CI。
- D4 转为 in_progress，固定售价附加倍率子项与关联诊断缺口已验证，但完整 D4 仍依赖 D2 及配置新鲜度、模型/fast 来源和剩余路径核查；没有新拆 ID 或把部分验收冒充全局 done。按设计现有 20 个 ID 计数：12 done、2 in_progress、6 pending，本轮 done 增量 0。STATE 更新为 84 行，三份文档原位维护。
- 仅提交本轮 9 个源码/测试文件与 3 份项目文档；没有推送、PR、worktree、迁移或部署。git diff --check 通过，新增 TODO/FIXME/XXX 为 0，演练产物剩余 0。之前已被自动审批拦截的 bin 产物和图片不重试删除，Windows 符号链接权限等待保持，未宣称全工作区无遗留。

## 2026-11-03：Key 共享上限决策与不明确占槽结果的回收

- 用户明确选择同一上游 API Key 单独配置共享总上限，各账号并发限制同时保留。设计第 4.4 节原位记录该规则：共享策略为 Key 维度单一配置，不取账号最小值，不把总上限复制到各账号；跨账号/产品/实例共享，额度身份与缓存域证据独立。具体策略存储与管理接口仍未实施，不将设计决策写成已具备能力。
- 核查发现当前申请账号/用户槽位后，只要缓存返回错误就直接返回；服务器已完成写入但回复丢失时，本次 requestID 无释放句柄而遗留到租约过期，可能造成虚假满载。新增旧源码回归实际退出 1：TestAcquireSlotLostReplyReclaimsExactMember 的账号/用户、清理成功/失败四个子例均因未调用释放而失败，service FAIL 1.450s。
- 在原有 AcquireAccountSlot/AcquireUserSlot 错误路径补精确成员释放尝试；与正常完成路径共用 releaseConcurrencySlot，删除两处重复释放块，并修正“入口自动等待”的过时注释。释放保留上下文路由值、脱离取消且受 5 秒超时约束；仍返回原申请错误，不允许清理故障改写成成功。无清空命名空间、无改动租约期限；迟到写入和缓存持续故障仍需原租约及后续所有权设计处理。
- 回归验证清理只影响本次 ownerID/requestID，其他请求保留；取消不阻断清理，清理错误不覆盖原申请错误；成功只在显式释放时回收，确认拒绝不额外释放。service 首轮专项三轮通过（1.652s）。service/handler/repository 槽位、并发、清理、Live 与利润终检相关三轮通过，分别 6.726s/1.442s/2.320s，退出码 0。
- 补充 miniredis 双独立客户端适配器回归，实际执行原 Redis Lua 占槽后注入丢失回复并取消请求，验证健康请求保留、容量立即可复用及最终计数归零；不把 miniredis 冒充生产 Redis 网络故障。增加健康成员 ZSCORE 精确断言后的最终 service/repository 新回归连续三轮实际通过（3.991s/3.803s），退出码 0。
- 尚未完成 D2 的固定线路/原同步兼容、共享 Key 配置与完整接线、缓存域证据；本轮没有新增或改名任务 ID，20 项状态仍为 12 done、2 in_progress、6 pending，父任务完成增量 0。没有保留探索中的账号 extra 共享上限、独立新槽位入口或未接通的 Redis 方法，避免把部分路径限额冒充全路径保障。
- 最终 service 与 repository 完整未限量 lint 分别输出 0 issues.、退出码 0；service/handler/repository go vet 退出码 0，最后 repository 测试变更后再次 vet 输出 Final repository vet: PASS。仅本地验证，没有运行 CI。
- 仅本地提交本轮三个源码/测试文件及现有三份项目文档。单一 work/monitor-driven-scheduling 工作树，未推送、创建 PR、生产迁移或部署。STATE 为 85 行；git diff --check 通过，新增 TODO/FIXME/XXX 为 0；没有生成新日志/二进制/图片。历史受保护图片和已拒绝清理的三个 bin 产物保持，不重试删除。

## 2026-11-03：D2 共享上游 Key 的 HTTP/SSE 实施

- 本轮从 7a142ac 的单一 work/monitor-driven-scheduling 工作树推进 D2，原 tracked 无差异，仅历史受保护图片未跟踪。用户确认的 Key 独立总上限落在 gateway.upstream_key_concurrency 配置：revision + limits，后者以最终出站凭据的 SHA256 小写摘要为键。没有复制账号额度或构建第二套调度器，原账号/用户限制保持；不新增数据库表或真实采购价格能力。
- 在现有 HTTPUpstream Do/DoWithTLS 取得最终出站鉴权后、发送前接入独立 Redis namespace。跨账号和下游分组发送同 Key 共用计数，识别标准 Bearer/Token/Key 头及 key/api_key query，重复值只占一次。生产 ProvideHTTPUpstream 与 Wire 生成接线注入既有 Redis；无共享权威时明确拒绝，NewHTTPUpstream 在未配置模式下保持原行为。
- Redis Lua 使用服务器时间原子判断上限/占槽；revision 不得倒退，同版本完整配置摘要不同拒绝。降低上限保留已有槽位，在途不重置。首次启用或移除规则须停止接流、排空旧请求，所有实例部署同一配置；此前未受限请求没有租约，不能声称滚动修改无需排空。配置启动加载，回滚也用新 revision，禁止清空 Redis 绕过版本保护。
- 长响应使用 30 秒租约、5 秒续租与 19 秒独立看门狗；续租只刷新现存成员，丢失所有权取消实际出站上下文。HTTP 共享额度随 Body EOF/错误/Close 释放，取消主动关闭响应体，重复释放幂等，多凭据部分申请失败精确回收。清理 Redis 故障依赖租约过期兜底，未声称跨 Redis 数据丢失仍可保持旧所有权。
- 本地满额与策略冲突沿原有限重选返回 503，不落入供应商/代理故障统计、账号冷却或用量活动记录。回归以 nil service/writer 证明这些外部路径未被调用，并保留原账号状态；发送后丢失租约不作为可直接重放的本地拒绝。
- 协议兼容边界明确：配置模式下 HTTP-to-WS 路径回落 HTTP，直接 Responses WS ingress 拒绝；受限 Key 的 realtime 请求拒绝；区域 SQL/本地快照模式在配置和运行时拒绝。此为防绕过边界，仍是 D2 未完成能力，未改变原父验收。自定义签名、非标准鉴权、绕过 HTTPUpstream 的任务及共享缓存域仍未验收；前端管理归 D7。
- 新 config/repository 共享额度专项连续三轮实际通过（3.583s/28.493s），含真实独立 Redis 5 进程的 32 竞争/上限缩小/旧版本及同版本冲突测试。HTTP 双独立客户端验证同 Key 跨账号满额不发第二次网络请求、其他 Key 独立、响应关闭后立即复用；同时覆盖取消、精确成员、重复 Close、申请失败和 realtime/缺 Redis 拒绝。
- HTTP/配置/WS/原并发回收扩大回归连续三轮实际退出 0：config 3.375s、repository 34.033s、service 14.256s。最终错误归类与 WS 边界专项 verbose 三轮显示各子例 PASS，service 2.763s；真实 Redis 竞争三轮逐项 PASS（2.28s/0.61s/0.35s），repository 包 17.124s，无 skip。首轮 Redis 尚未监听时的连接拒绝由 readiness 重试处理，不作为跳过或通过前提。
- D2 固定选路、WS/Live/区域完整接线、缓存域仍未完成；D3–D9 保留原依赖。原 20 个 ID 仍为 12 done、2 in_progress、6 pending，本轮父任务 done 增量 0。最终 lint/vet、服务器编译及提交结果待命令返回后追加，不以早期结果代替最终源码检查。
- 最终静态检查首轮 3 项：新 SSE 测试未检查 Flusher 类型断言、config 既有测试两处 reflect.Ptr 过时用法。前者改用受检 ResponseController.Flush，后者原位改 reflect.Pointer，不修改检查规则。配置可达性回归随后实际发现新 revision 未注册默认值，三轮均报 1 config keys have no default registered；补齐默认值后 config 全包实际通过（2.457s），原断言保持。
- 修复后一次复核命令遗漏进程 PATH，误用系统 Redis 3，真实 Redis 竞争三轮均失败（预期 4 个获槽，实际 0）；不将其标为跳过，也不改 Lua 兼容旧环境。显式加入 H:/Root/tools/redis/5.0.14.1 后，配置/Key/可达性/PR612 三轮复核实际通过（config 0.760s、repository 28.534s）。cmd/server 生产注入编译实际输出 ok 2.232s [no tests to run]，属于编译检查。
- 最后源码与夹具修正后，执行 go vet -tags=unit -p 2 ./internal/config ./internal/repository ./internal/service，实际输出 Final shared upstream Key vet: PASS。完整未限量 golangci-lint run --build-tags=unit --timeout=12m --concurrency=2 --allow-parallel-runners --max-same-issues=0 --max-issues-per-linter=0 同三包最终输出 0 issues.，退出码 0。未扩大为全后端/生产验收。
- 本地提交本轮已核对的 20 个源码/测试/配置/文档文件；git diff --check 通过，新增 TODO/FIXME/XXX 为 0，STATE 87 行，无新持久日志/二进制/图片。只使用原单一工作树，未推送、PR、CI、生产迁移或部署；历史受保护图片和被自动审批拒绝清理的 bin 产物原样保留。实际提交标识由 Git 保存。

## 2026-11-03：共享 Key 的直接 Responses WS 桥接与逐轮错误类型

- 原位删除配置共享 Key 后直接拒绝全部 Responses WS ingress 的分支，复用既有 HTTP/SSE bridge。每轮在 HTTPUpstream 以最终请求凭据取共享槽位，账号原 BeforeTurn/AfterTurn 保持；连接空闲不持共享槽位，不新增并行 WS 租约实现。HTTP-to-WS 的 HTTP 回落保持。
- 共享限额优先于 smart protocol 的 ws/auto/shadow 及原生 passthrough；账号明确 off 仍拒绝，Grok 等原有强制桥接保持。上游须支持 Responses HTTP/SSE；这一选择不证明 HTTP 与 WS 同价、同缓存域，原生上游 WS/Live/区域与 D2 其余验收仍开放。
- 回归先复现第二轮共享额度满额/策略错误被包装为普通 502：两个 turn_2 子例均失败，第一轮通过。修复桥接器原错误分类入口后，各轮统一保留本地 503 UpstreamFailoverError，不写供应商健康，发送后的 lease lost 不变成可重放的准入失败。
- 新 repository 组合测试使用真实 loopback WS/HTTP 与 miniredis 执行生产服务及实际 HTTPUpstream：首两轮各持一个共享槽，其他账号/独立客户端同凭据被拒且无额外请求；响应完成后空闲槽为零；第三轮取消传到上游 HTTP 并回收精确成员。连续三轮通过（2.159s）。独立真实 Redis 5 竞争与该组合测试 verbose 复核均 PASS、无 skip（2.243s）。
- service 网络测试覆盖 smart protocol 关闭/ws/auto/shadow、两轮 usage、账号显式关闭和后续账号并发钩子拒绝。首轮关闭测试误用被账号旧 enabled 字段覆盖的默认 off，30 秒等待失败；按原优先级改为显式账号 off，保持实现和原语义。最终新增专项三轮 1.883s；扩大 WS/bridge/续接/智能协议/Key 回归三轮 service/repository 105.830s/18.402s，最终两包 go vet 退出 0。
- 完整两包 lint 首轮发现新组合测试未检查取消返回错误 1 项；补齐 require.ErrorIs(err, context.Canceled)，并非忽略错误。最终组合测试三轮 1.796s、两包 go vet 与完整未限量 golangci-lint 均退出 0，lint 输出 0 issues.。git diff --check 通过，STATE 88 行；原整体拒绝分支及误导的 xai-http-bridge 日志名称已移除，无新增 TODO/FIXME/XXX。仅本地提交这 9 个任务文件；父任务仍为 12 done、2 in_progress、6 pending，done 增量 0，推进的是 D2 的直接 WS 生命周期验收。历史受保护文件不重试删除，不 CI/推送/部署。

## 2026-11-03：静态 Key 绑定与发送前倍率证据复核

- 原线路指纹把所有敏感键排除，API Key 替换/清空/null 的三个准入子例实际返回 nil，旧 WS 也会继续匹配。静态 api_key 现纳入 API Key 账号的内存指纹；OAuth token 的正常刷新排除规则保持，未扩大敏感数据输出或存储。
- 原每轮准入虽然读最新行，却没有复核采购倍率、销售附加倍率与同步证据；在有效账号夹具下，倍率上涨、销售倍率改变、证据移除三个子例均实际错误放行。现于原准入方法共用 checkOpenAITurnBindingAndRate，候选/终检规则、固定销售阈值及原同步写回不改。
- HTTP 协议适配器可能在早期准入后构建请求再发送；最终 doOpenAIUpstream 的四个负向回归也先实际失败。启用利润门时最终出站前按原调度组重读账号并复核同一判定，失败不调用 HTTPUpstream/插件或进入健康归因。未启用门的发送行为保持。
- 严格 OpenAI 请求跳过进程准入缓存，不将先前缓存中的便宜倍率或旧 Key 当成新授权；原关闭门缓存、区域快照权威和 TTL/持久栅栏保持。缓存回归先暖缓存再替换主库行，断言发生第二次读取并拒绝；不会把数据库与网络之间的变化窗口表述成原子事务。
- 新真实 loopback WS/HTTP 两轮回归确认：首轮正常，第二轮 BeforeTurn 替换主库静态 Key 后返回 account_binding_changed，实际 HTTP 请求总数仍为 1。原 OAuth token 更新、未装门准入、正常静态 Key 和不变价格回归保持。
- 新每轮专项三轮 13.584s；扩大相关回归三轮 108.158s，补齐最终 HTTP guard 后相关专项三轮 13.358s，最终新增专项三轮 15.442s。最终完整 service 回归实际输出 ok 376.385s，Full service exit: 0；service/handler vet 输出 PASS、退出 0。
- 前两次完整 service 返回失败（319.984s/329.647s），工具截断约 18 MB 输出中段，未保留具体失败名称，不能声称根因已定位；首次运行与后续版本/全量运行有重叠。最后使用内存提取失败片段、无持久日志的原完整命令实际退出 0。并发干扰仅是未证实假设，不登记为已修复或跳过任何测试。
- 最终 service 完整未限量 lint 输出 0 issues.、退出 0；没有降低检查规则。git diff --check 通过，STATE 89 行，无新增 TODO/FIXME/XXX；7 个任务文件本地提交，不推送、不 CI、不创建工作树或生产变更。既有受保护图片和 bin 产物保持；D2/D4 仍 in_progress，原 20 项为 12 done、2 in_progress、6 pending，父任务完成数不变。

## 2026-11-03：New API 完整 Key 归属与授权预览固定分组同源

- 在 caaf3a0 单一工作树原位修复，不增加任务 ID、不修改父验收。旧实现实际复现：唯一掩码碰撞、揭示接口仍为掩码/空值/失败/缺失均错误 Matched=true；绑定后的碰撞仍返回 OK；预览把异组/空路由错误认作有效倍率。首轮红色回归退出 1（2.453s）。
- 预览、保存与绑定探测复用 verifyNewAPITokenKey。完整库存精确相等可直接证明；掩码即使唯一也须完整揭示比对，多个候选仍由人工明确选择；拒绝掩码凭据，不输出 Key 或管理密钥。揭示不可用时该绑定不再作为有效证据，可能使旧的未完整验证绑定转为 failed，恢复接口/正确 Key 后按原探测恢复。
- 预览和快照复用 newAPIFixedGroupRate，删除旧的 len(route_groups)<=1 宽松分支；完整归属与固定组适用性分别判断，不合格组仍可保存已验证绑定并独立展示钱包，不写账号倍率或改上游 Key。
- 首次修复后回归又实际暴露：失败工厂保留上次 FreshUntil（首轮退出 1，2.297s）。New API 失败路径现统一由原 persistNewAPIFailure 清除 Data/ReceivedAt/FreshUntil 和钱包有效状态，保留失败退避，不修改通用原生同步快照规则。恢复回归验证成功→碰撞→成功→揭示404→成功，并检查实际持久快照。
- 新增四项行为测试覆盖六类揭示结果、完整库存/前缀空白/大小写/掩码凭据、恢复及七类固定分组；默认正常夹具补完整 Key 揭示响应，未放宽失败断言。相关三包三轮回归实际通过：service 3.015s、repository 2.088s、admin handler 2.506s；最终三包 vet 输出 New API final vet: PASS、退出 0。
- 本轮首次完整 service 实际失败（316.041s）：TestIssue578ReviewActualAutoResetAndPostResetWorkflow 的 post_reset/usage_rejected 与 post_reset/details_delayed 期待100、实际89。独立十轮再次失败，自动重置子例也复现；此前截断失败的历史根因仍不追认。检查确认测试阻塞请求后立即发响应头，没有建立持久墙上时钟的严格先后关系，同刻观测不满足其“旧查询”前提。按既有测试模式等墙上时钟前进，不改业务比较规则/期待值/超时，不跳过测试。
- 首次扩大十轮后 TestIssue578OlderQueryCannotUndoLaterHeaders 又出现同一前提缺失，补齐该用例同样的墙上时钟先后关系并关闭测试网关资源。修复后的 New API/Issue578 全组十轮实际通过（12.199s），New API and snapshot regression exit: 0；另两处相关测试原位修正，无并行实现或放宽断言。
- 扩大 service/admin 的完整未限量 lint 退出1：8项均位于本轮未修改的 admin 测试（depguard 1、errcheck 5、staticcheck 2）；git diff --exit-code -- backend/internal/handler/admin 退出0，未隐藏这些诊断。实际修改的完整 service lint 随后输出0 issues、退出0；admin 扩大检查仍待处理，不扩大 M5 原六包验收范围。最终源码检查与完整回归结果另列。
- 配额测试时序修复后的最终完整 service 未限量 lint 实际输出 0 issues.；service/repository/admin 三包 vet 实际输出 Final service lint and three-package vet: PASS，同一命令退出0。未修改 lint 规则或 admin 的8项既有诊断。
- 最终完整 service 本地回归实际输出 ok 319.099s、Full service exit: 0。此前316.041s失败及独立十轮失败保留，不以最终通过抹去；时序前提修复已随完整回归验证。本轮只提交8个文件，git diff --check通过，无新增TODO/FIXME/XXX或持久测试产物，STATE保持100行以内；单一工作树、不CI/推送/部署，历史受保护产物保持。原20项仍为12 done、2 in_progress、6 pending，本轮父任务done增量0，D2.2核验缺口修复已闭环。

## 2026-11-03：D8 admin 静态检查阻塞与跨层测试整理

- 接续86d2685，在同一work/monitor-driven-scheduling工作树处理前轮实际报告的8项诊断；不新建任务ID或工作树，不CI/推送/部署，不改变生产代码和lint规则。原depguard 1、errcheck 5、staticcheck 2对应的检查范围全部保留。
- 将整个 custom_legacy_admin_key_test.go 从handler单元目录迁入 internal/integration/adminkey/legacy_admin_key_test.go，标签由unit改为integration，改经公开NewSettingHandler构造器调用；旧文件删除，真实repository/HTTP/数据库断言保持。完整Key原子轮换能力在夹具初始化处受检断言后注入屏障，不再运行未检查的类型转换。
- batch响应数据类型断言增加失败检查；三处sqlmock资源在Cleanup中期待Close、检查关闭错误并统一核验剩余SQL期待，删除原尾部重复期待检查；两个事务结果分支改为switch，保留提交失败、回滚、身份变化与缓存失效时机断言。
- 真实PostgreSQL测试的数据库连接、schema删除和client关闭均检查错误，并按注册逆序清理。未取得现有55433实例凭据，不修改它；复用本地pgsql17工具在本项目backend/tmp/admin-key-pg-d8建立独立127.0.0.1:55439临时实例，仅该命令进程注入无密钥测试DSN。
- 首轮相关handler测试连续3轮通过（1.461s），完整admin单元包通过（2.596s）。真实数据库迁移测试首轮三轮通过（1.997s），补齐清理返回值后的最终三轮逐项PASS（2.078s），没有skip；覆盖删除先提交不重建、可轮换并删除、缺失/空值不新建。
- 完整admin unit lint实际输出0 issues、退出0；新integration包以integration标签完整未限量lint同样0 issues，admin unit vet实际输出Admin unit vet and integration lint: PASS、退出0。最终源码在删去重复SQL期待后再次运行完整单元测试及vet，结果另列。
- schema清理后SELECT count(*)返回0；pg_ctl fast stop实际输出server stopped、Task PostgreSQL stop exit: 0。包含删除的新目录清理命令被自动审批blocked by policy拒绝，整条未执行；随后仅单独核对schema并关闭进程，不重试文件删除或换工具绕过。保留新目录973文件、41445106 bytes（约39.525 MiB），供手动删除；此前历史受保护文件保持。
- 删除重复断言后的最终完整admin单元包实际输出ok 5.365s、Final admin unit tests and vet: PASS、退出0；integration三轮后的独立vet退出0。最终admin完整未限量lint再次输出0 issues、退出0，integration完整lint亦0。git识别跨层测试为81%相似度重命名，旧路径不存在，新增TODO/FIXME/XXX为0，diff检查通过；原20项保持12 done、2 in_progress、6 pending，本轮完成admin检查阻塞闭环，未将D8整体提前标done。仅本地提交任务文件；临时数据库目录因审批拒绝保留为明确外部清理等待。

## 2026-11-03：D4 Grok 文本的权威准入与准备后发送复核

- 接续81f003b，按原D4验收修复Grok文本在非区域模式下跳过权威读取的问题：latestOpenAITurnAccountForGroup原本对非OpenAI直接返回选号对象，checkOpenAITurnBindingAndRate又仅比较OpenAI线路指纹。原位复用既有只读可重复读仓库、指纹和倍率判定，没有新增调度器、配置或实现分支副本。
- 先扩展既有三项OpenAI回归到OpenAI/Grok。旧实现实际失败：Grok倍率上涨/销售倍率改变/同步证据丢失、静态Key替换/清空、地址变化、移出组、禁用账号在每轮或最终发送前仍返回nil。首次运行还发现扩展缓存测试误将Grok未装门的原零读取行为断言为1，夹具按原边界修正，并明确预热本地缓存；修正后的红色回归仍实际退出1（2.338s），包括严格模式缓存未重新读取。未弱化失败边界。
- 利润门有效时，Grok文本复用原权威刷新和区域快照优先级；跳过本地准入缓存，最终HTTP出站复核当前分组、账号资格、线路指纹、采购/销售倍率及同步证据，保持请求冻结售价。未装门Grok保留原直接调用边界，OAuth正常刷新不当作静态Key替换。保留生产构造器requireLatestTurnAdmission与小型内部夹具原兼容边界；不宣称跨SQL与网络原子，不将额外读取延迟当作零成本。
- 扩大后的新增专项连续三轮实际通过（3.187s）：含数据库失败/账号消失/缺少Reader/缺少Repository时无网络发送、未装门不额外读库、OAuth刷新保留。真实loopback HTTP跑Grok Responses可恢复加密内容错误：第一读正常，第二读倍率上涨时仅1次网络请求、本地准入错误原样返回且不写下游502、不冷却账号；不变时2次发送并恢复成功。未重复实现HTTP夹具。
- 此轮局部验收属于原D4，不新增ID或提前关闭父任务；D2、其他路径、分组配置新鲜度和模型/fast来源仍保留。最终全包、扩大回归和静态检查结果待实际返回后列出；无CI、推送、部署或新工作树。
- 最终扩大回归连续三轮实际通过：service18.644s、repository20.098s、handler2.892s；三包vet实际输出Grok admission regression and three-package vet: PASS、退出0。完整service未限量lint输出0 issues、退出0，最终完整service本地回归ok323.453s、Full service exit: 0。未扩大为全后端/生产验收。
- 交付复核：旧的非OpenAI直接跳过注释和设计中Grok待实现表述已原位更新；实际代码只扩展两处既有判定，测试扩展既有平台矩阵并复用HTTP夹具，未新增并行实现或TODO/FIXME/XXX。git diff --check通过，STATE保持100行以内；7个任务文件本地提交，未推送、CI或创建工作树。原20项仍12 done、2 in_progress、6 pending，本轮关闭Grok文本缺口，不将D4父任务提前标done；历史受保护图片/bin/已停止临时PG目录保持。

## 2026-11-03：D4 WS 每轮分组策略刷新与认证售价隔离

- 接续前序已复现的六个失败子例：连接携带同 ID 的旧认证分组时，收紧策略仍放行；旧认证快照显示关闭时，配置源失效也未装入拒绝门。根因是每轮重新解析仍优先使用认证上下文，而非每轮读取配置源。
- 原位扩展既有 resolveOpenAIProfitControlGate 的调用参数，区分 HTTP 请求内复用与 WS 新轮次刷新；未增加第二个解析器或并行调度实现。WS 使用既有 GetGroupByIDLite：主节点读主库，区域读本地快照；读取报错、空对象及错组继续沿用已有配置不可用门与拒绝原因，免费账号也不绕过。
- 策略刷新不覆盖认证分组或该轮的销售倍率快照；测试特意将新策略组销售倍率设为 99，仍以原认证售价计算阈值。HTTP 同请求 failover 保持已冻结门与 pricingAt，WS 每轮重冻 pricingAt；内部未接入配置源夹具、抑制标记及确认关闭行为保持。
- 新回归覆盖 OpenAI/Grok × 主库/区域的收紧、放宽、关闭、重开，以及主库/区域的缺失、报错、错组、恢复，逐次保留旧认证对象以验证不误用连接快照。扩大专项连续三轮实际通过：service 13.635s、handler 8.440s；同两包 go vet -tags=unit -p 2 实际退出 0。完整包回归与 lint 另按实际结果追加。
- 本轮关闭 WS 每轮策略刷新缺口，D4 父任务仍待 D2、其他路径、HTTP 首次装门/区域传播新鲜度及模型/fast 来源；原 20 项保持 12 done、2 in_progress、6 pending。未运行 CI、推送、部署或生产迁移，单一工作树保持；历史审核拒绝清理的文件不重试删除，不以隐藏未跟踪图片伪造工作区干净。
- 最终 service/handler 完整未限量 golangci-lint（unit 标签、concurrency=2、max-same-issues=0、max-issues-per-linter=0）实际输出 0 issues.、退出 0；本轮未修改 lint 规则。五个变更 Go 文件 gofmt 检查通过，原解析函数所有调用点已更新，无新增 TODO/FIXME/XXX 或重复解析入口。
- 最终完整 service 本地回归实际输出 ok 328.225s、Full service verification exit: 0；测试进程仅注入 Redis 5 的 PATH。前序已启动的完整回归在恢复上下文时仍运行，其退出输出未取到，本记录只采用本轮独立取得的最终结果。
- 状态文档从 97 行收拢为 73 行：删除已在本文件保存的重复历史验证叙述，保留原任务、当前验收、Windows 符号链接权限及历史清理等待，验证入口更新为当前专项；不增加第二份台账。最终源码目录仍只有原 12 个图片共 1164 bytes，本轮新增持久测试产物 0；按授权本地提交这 8 个任务文件，不推送。实际提交标识由 Git 保存。

## 2026-11-03：D4 HTTP 首次策略读取与请求内关闭决定

- D2 原生 WS、Live 与区域额度仍依赖独立的连接租约、SDP 会话交接和区域权威接线，不能由 HTTP 响应租约自动覆盖；保留原拒绝边界。本轮先关闭已有 D4 的 HTTP 首次读取缺口，未把 D2 的局部能力记成父验收完成。
- OpenAI/Grok 专用 HTTP 入口沿用同 ID 的旧认证组，收紧仍取 0.9 而非 0.4、开启/关闭变化不生效、旧认证关闭时配置异常仍无拒绝门；新增 24 个主库/区域子例在旧实现上全部实际失败（service 3.388s、退出 1）。通用 token 网关也存在相同优先级，五平台复用同一测试矩阵的独立红色回归实际退出 1，输出保留阈值、开关和配置缺失失败，不以实现推断代替复现。
- 两个原解析入口统一先读已有 schedulerSnapshot.GetGroupByIDLite，主节点主库、区域本地快照；未接入策略源的内部调用兼容保持。旧认证 Group 仅继续提供计费快照，新策略组将售价设为 99 的回归仍按认证售价计算，用户覆盖与高峰计费入口不改。首次读取增加一次 Lite 查询，不宣称其生产开销为零或多实例传播零延迟。
- 同请求也缓存已确认关闭的决定，单独记录调度组 ID，不安装活跃门；重试不重新读源或因后台开关变化中途装门。WS 新轮次绕过旧决定重新解析，关闭后的 composite 成员仍保留调度归属；重开不会误读计费父组。共用一个关闭决定处理函数，删除旧 clearForeignProfitControlGate 和不再需要的 refreshGroup/useAuthGroup 分支，更新全部调用点及认证快照过时注释；原投影字段保留。
- 24 个新增 HTTP 子例先通过；相邻 WS 回归因首次请求夹具未提供策略源出现 4 个失败，补齐原组源并将区域读取次数由 4 改为 5，保留原阈值与恢复断言。统一后的 84 个首次读取子例、请求内开关冻结、成员组重开及相关利润/WS 三轮扩大回归实际通过：service 1.503s、handler 5.398s，退出 0。
- 最终生产源码的完整 service 回归实际通过（314.062s）；同次完整 handler 失败，日志含 profit_group_config_unavailable，流式计费和平台隔离等夹具未提供分组源而未到达上游。6 个 handler 构造点改为给快照和网关注入同一份既有分组仓库，主/备用映射保持；未弱化响应、用量、取消或平台断言。更改夹具后的完整 handler 与最终 lint 结果待实际返回后追加；两包最终 vet 已实际退出 0。
- 夹具补齐后的完整 handler 实际输出 ok 142.754s、Complete handler fixtures verification exit: 0；最终 service/handler 完整未限量 lint 实际输出 0 issues.、退出 0；两包最终 vet 退出 0。完整 service 的 314.062s 结果仍对应最终生产源码，之后只更改 handler 测试夹具与文档，未重复运行已验证的 service 全包。
- 交付仍使用原 work/monitor-driven-scheduling 单一工作树；无 CI、推送、迁移或部署。旧清门函数和模式参数检索均为 0，新增 TODO/FIXME/XXX 为 0，源码图片仍为历史 12 个共 1164 bytes。本轮按授权本地提交任务源码、测试及原三份文档，历史审批拒绝删除和 Windows 符号链接权限等待保留；原 20 项仍 12 done、2 in_progress、6 pending，未提前关闭 D2/D4。
