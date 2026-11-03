# 项目状态

更新日期：2026-11-03。

## 目标与入口

倍率范围内的多供应商自动调度，保护质量、完整响应体验和已有会话缓存，并便于跟进原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [决策记录](DECISIONS.md)

## 当前授权

用户已授权推进本地实施、修复、相关清理和本地验证；不运行 CI。
最新 AGENTS.md 已授权验收后本地提交；未授权远程推送、生产数据库迁移或部署。真实采购价仍在本期范围外。

## 任务进度

| 任务 | 状态 | 证据/剩余范围 |
| --- | --- | --- |
| D1 | done | 主设计第 3.1 节已记录原配置覆盖、倍率口径、共享 Key 缺口及最小补丁边界 |
| D2 | in_progress | D2.1–D2.4 已完成；请求级固定线路选择、重复 Key 的共享额度与缓存域证据仍待落地 |
| D2.1 | done | 自动同步快照有效性、尝试时刻高峰、原同步值域与在线/预演同源已落地；30 类边界及槽位释放覆盖，三轮专项、完整本地回归、vet 与真实 CLI 演练通过 |
| D2.2 | done | New API 声明组与单路由须一致；17 类边界、同组→异组/多组→恢复回归通过；保留余额、身份绑定与账号倍率，相关三包三轮回归、vet 与六包未限量 lint 0 通过 |
| D2.3 | done | 协议地址/协议/接入模式统一进入身份边界，编辑/CRS/绑定旧证据失效；12 类四路径与 3 项绑定回归，四包三轮回归、vet 和 service 未限量 lint 0 通过 |
| D2.4 | done | 按入站/模型分流的探测、New API 授权/列表/绑定/事务保存统一使用生效地址；旧源码 6 子例失败，新回归及相关四包三轮通过；不作为跨协议同价或共享缓存证据 |
| D3 | pending | API Key 最新可用状态、触发、过期与恢复待补齐 |
| D4.1 | done | 旧实现失败已复现；双故障/账号消失拒绝，缓存回源及未装门行为保持；专项、service/handler 回归和差异检查通过 |
| D4.2 | done | 分组配置失败/空对象/错组拒绝准入，HTTP/WS/免费账号及恢复覆盖；补齐 15 项夹具/旧语义缺口后，专项 3 轮、默认标签、vet 和 Linux 全量通过；历史 lint 诊断在 M5 明确边界后归零 |
| M1 | done | 删除 6 项未引用测试辅助符号及死计数器；相关回归通过，unused 告警 6→0 |
| M2 | done | 错误检查 247→0；受影响顶层用例 1072 pass、6 个既有 skip，vet 通过；受检断言辅助包及图片策略重复回归通过，原边界保留 |
| M3 | done | WinSock 原生拒绝连接/主机与网络不可达已识别；超时/重置不误判；真实单 POST 及代理/时序/WS 组合回归连续 5 轮通过（140.262s） |
| M4 | done | 三处私有原子持久化收敛为共用接口；原 22 项 Windows 失败全部实际通过；双平台完整回归、核心三包 lint 0 与最终 vet 通过，本轮产物归零 |
| M5 | done | 明确生产依赖与根目录测试夹具的检查范围，3 处有意兼容调用逐点说明；六包完整 lint 0，生产禁止依赖反向验证 2/2 命中，回归与 vet 通过 |
| D4 | pending | 待 D2；继续核查所有路径、配置新鲜度及固定售价；局部修复不替代完整严格范围验收 |
| D5–D7 | pending | 指标、缓存预测、分配与前端依赖前置任务 |
| D8 | pending | 双平台既有回归、D2.1 Windows 完整回归/本地 CLI 专项、M5 六包完整 lint 通过；集成/升级/全局 CLI 场景仍待完成；不运行 CI |
| D9 | pending | 依赖验证与对应生产执行授权 |

## 环境与工作区

- 基线：`6de829fa2efa801dcf3071c4efe842198c7724be`，单一工作树；本地交付分支 `work/monitor-driven-scheduling`，远程 origin 为 ranxi2001/sub2api，本次不推送。
- 本机 Go 1.25.6 已通过自动工具链获得项目要求的 Go 1.27.3；未降低 go.mod 要求。
- D4.1 专项与 service/handler 倍率、槽位、重选、WS 定价回归通过；go vet 通过。
- lint 工具阻塞已解除：使用本机缓存的 v2.15.0 源码并以 Go 1.27.3 构建。
- M5 最终 service/handler/assertions/regional/securefile/profit-preview 六包未限量 lint 输出 0 issues、退出码 0。生产边界反向验证准确拒绝两处禁止的 Redis 导入，验证文件均已移除；完整订单兼容与 WS 回归连续 3 轮通过（2.257s），另 5 轮专项通过（2.833s），go vet 通过。例外只适用指定测试范围/调用行，不声称后端全部包均已受检。
- 图片策略测试已隔离 DATA_DIR 并关闭 relay；完整图片测试连续 3 轮通过（2.487s），源码目录新增产物 0。
- M4 后 Windows 完整本地回归：service 10396 pass/11 个既有 skip、handler 1100 pass/2 skip、regional 13 pass、securefile 6 pass、assertions 3 pass；5 包均 pass，原 22 个平台失败逐项确认实际通过（无 skip）。
- M4 最终 Linux service 10397 pass/10 个既有 skip/0 fail，10407 个顶层启动均有结果，退出码 0（377.269s）；securefile/assertions/regional 各连续 3 轮通过，未吞掉 fsync 错误或放松权限断言。
- M4 验收后精确清理 14 个测试程序/日志，共 396790278 bytes（378.409 MiB），当时任务产物归零；历史证据归入 DECISIONS.md。
- D2.1 Windows 完整本地回归：service 10399 pass/11 个既有 skip、handler 1100 pass/2 skip、CLI 3 pass；三包 0 fail，启动/结束逐项匹配。相关专项连续 3 轮、go vet 和真实 CLI 文本/JSON 演练通过；旧 lint 诊断已由 M5 核查处理，历史证据见 DECISIONS.md。
- `backend/internal/service/data/bps-images/` 下 12 个本任务 PNG 共 1164 bytes，删除被执行器策略拦截而保留；未改用其他工具绕过。
- 清理后调度、路由矩阵、额度与倍率回归：service `ok 13.496s`；此前 service/handler `ok 2.131s/7.787s`；go vet 通过。
- 仅放行本目录三份主题文档进入 Git 检查，其余 docs 忽略规则保持。
- 已验收的任务源码、测试及本目录三份文档按最新授权本地提交；提交标识以 Git 记录为准。不纳入生成图片或历史 bin 产物，不清除其他任务文件，不生成重复设计或临时台账。
- D2.4 四包扩大回归三轮通过：service/repository/handler/CLI 分别 108.055s/5.710s/20.313s/1.907s。目录夹具改用 UUID，固定时钟下十轮通过；repository 受影响组合三轮通过（3.669s）。
- Windows 完整 service/handler/CLI 通过（326.174s/145.517s/1.793s）；repository 首轮 13 个含父级失败逐项定位后，完整剩余用例通过（22.031s），但创建符号链接夹具缺少系统权限，原测试保留，不能声称 Windows 全部通过。
- 凭据密钥初始化复用 securefile 原子创建及打开句柄权限检查，删除重复持久化逻辑；并发不覆写、私有 ACL、POSIX 权限/fsync、原备份锁与验证码断言保持。Linux 凭据/New API/备份/验证码及 securefile 连续三轮均 PASS，含符号链接拒绝；最终六包 vet 通过。
- 测试 Redis 位于 H:/Root/tools/redis/5.0.14.1；仅命令进程 PATH 使用，不改系统 Redis 3 或全局环境。真实 Redis 发布/失效/恢复及 Ticket 三轮通过。新交叉编译程序和下载压缩包已清理 180515955 bytes，便携运行工具保留 48299724 bytes。
- 最后源码的 service/repository/handler/assertions/regional/securefile/profit-preview 七包完整未限量 lint 输出 0 issues、退出码 0；最终六包 vet 退出码 0。Windows 权限等待与全局未完成验收仍保留，不由静态检查代替。

## 可复现的 D4.1 专项验证

在 backend 目录运行，已通过（`ok 1.681s`）：

```powershell
go test -tags=unit -p 2 ./internal/service -run 'TestGatewayProfitControlTerminalRefresh(FailureRejectsAdmission|WithoutGatePreservesAdmission)$' -count=1
```

## 下一项工作

D2：D2.1 自动同步准入、D2.2 固定分组证据、D2.3 协议线路身份、D2.4 生效目标统一已完成；剩余请求级固定线路选择/原同步兼容、共享 Key 限额与缓存域，不能把 AccountOpsGroup 当作 Key 级限额键或缓存共享证据。
D3–D9 主线仍待按设计依赖实施与验收；D4.1/D4.2 和 M1–M5 的完成不代表全局方案已全部落地。

## 外部验收等待

Windows 当前令牌没有 SeCreateSymbolicLinkPrivilege；系统在 os.Symlink 构造夹具时返回 A required privilege is not held by the client。原测试未改成 skip、未放松断言；Linux 同用例已三轮实际通过。Windows 全量验收的解除条件：由系统管理员提供具有符号链接创建权限的测试进程，再运行原 repository 完整命令；本任务未调整机器安全策略。

## 当前清理阻塞

自动审批审核以 blocked by policy 拒绝删除本轮 3 个已验证普通文件，明确路径删除也被拒绝，未改用其他工具规避。
保留在 backend/bin：task-d2-full-tests.jsonl、task-d2-golangci-lint.exe、task-d2-preview.json，共 132004965 bytes（125.890 MiB）；均为已忽略的本轮产物，可由相同验证命令重新生成。现有 12 个受保护图片夹具保持原样。
