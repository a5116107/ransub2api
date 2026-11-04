# 项目状态

更新日期：2026-11-05。

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
| D2 | in_progress | D2.1–D2.4 完成；HTTP/SSE、HTTP→原生 WS、直接 WS ctx_pool/passthrough/桥接逐轮共享额度、静态 Key 请求绑定及本地拒绝归因/搜索503已验证；Sub2API请求固定组与倍率确认、最终HTTP/原生WS握手归属及通用原生网关13个发送点、Gemini三个生成循环及Antigravity六个发送点终检已实现；跨协议计价、Live/区域及缓存域仍待验收 |
| D2.1 | done | 自动同步快照有效性、尝试时刻高峰、原同步值域与在线/预演同源已落地；30 类边界及槽位释放覆盖，三轮专项、完整本地回归、vet 与真实 CLI 演练通过 |
| D2.2 | done | New API 声明组与单路由须一致；唯一掩码完整归属、预览/快照同源及失效时间清理已修复，最终验证见本轮记录；17 类边界、同组→异组/多组→恢复回归通过；保留余额、身份绑定与账号倍率，相关三包三轮回归、vet 与六包未限量 lint 0 通过 |
| D2.3 | done | 协议地址/协议/接入模式及模型分流规则统一进入身份边界，编辑/CRS/绑定旧证据失效；16 类四路径与 4 项绑定回归，规则实际切换协议及9平台发送前拦截已覆盖；专项三轮通过 |
| D2.4 | done | 按入站/模型分流的探测、New API 授权/列表/绑定/事务保存统一使用生效地址；旧源码 6 子例失败，新回归及相关四包三轮通过；不作为跨协议同价或共享缓存证据 |
| D3 | in_progress | 状态/独立业务5xx及OpenAI发送准入已验证；本轮补齐401/408/429及2xx响应体读取错误独立失效、数据库复测与SIWC观察，保留质量保护边界；已接10类解析路径的缺终态独立失效并区分本地502；专项/完整回归及完整service包lint通过；其他协议/能力、握手前传输、原生WS及缓冲转换仍待完成 |
| D4.1 | done | 旧实现失败已复现；双故障/账号消失拒绝，缓存回源及未装门行为保持；专项、service/handler 回归和差异检查通过 |
| D4.2 | done | 分组配置失败/空对象/错组拒绝准入，HTTP/WS/免费账号及恢复覆盖；补齐 15 项夹具/旧语义缺口后，专项 3 轮、默认标签、vet 和 Linux 全量通过；历史 lint 诊断在 M5 明确边界后归零 |
| M1 | done | 删除 6 项未引用测试辅助符号及死计数器；相关回归通过，unused 告警 6→0 |
| M2 | done | 错误检查 247→0；受影响顶层用例 1072 pass、6 个既有 skip，vet 通过；受检断言辅助包及图片策略重复回归通过，原边界保留 |
| M3 | done | WinSock 原生拒绝连接/主机与网络不可达已识别；超时/重置不误判；真实单 POST 及代理/时序/WS 组合回归连续 5 轮通过（140.262s） |
| M4 | done | 三处私有原子持久化收敛为共用接口；原 22 项 Windows 失败全部实际通过；双平台完整回归、核心三包 lint 0 与最终 vet 通过，本轮产物归零 |
| M5 | done | 明确生产依赖与根目录测试夹具的检查范围，3 处有意兼容调用逐点说明；六包完整 lint 0，生产禁止依赖反向验证 2/2 命中，回归与 vet 通过 |
| D4 | in_progress | 固定售价、线路/倍率终检、严格请求跳过旧缓存、HTTP 首次/WS 每轮权威策略读取已验证；已补齐全部7个多协议供应商的账号权威刷新与最终发送绑定，并校验实际HTTP/原生WS目标，通用原生网关13个发送点、Gemini三个生成循环、Antigravity六个发送点及Messages/Chat/Responses/原生入口门传播已补齐；仍待 D2、区域传播及模型/fast 来源 |
| D5–D7 | pending | 指标、缓存预测、分配与前端依赖前置任务 |
| D8 | pending | admin/跨层PostgreSQL及既有WS绑定已验证；本轮修复真实桥接重试的固定墙钟上限和Grok提交前截止的夹具问题，相关组合20轮/100轮及最终完整回归通过；集成/升级/全局CLI场景仍待验收；不运行CI |
| D9 | pending | 依赖验证与对应生产执行授权 |

## 环境与工作区

- 基线：`6de829fa2efa801dcf3071c4efe842198c7724be`；单一工作树，本地交付分支 `work/monitor-driven-scheduling`，origin 为 ranxi2001/sub2api；只本地提交，不推送。
- 项目要求的 Go 1.27.3 已通过自动工具链获得；未降低 go.mod 要求。完整包 lint 使用 backend/bin/task-d2-golangci-lint.exe（v2.15.0），不修改检查规则。
- 真实 Redis 测试只在命令进程 PATH 加入 H:/Root/tools/redis/5.0.14.1，不修改系统 Redis 3 或全局环境。
- D2.1–D2.4、D4.1/D4.2、M1–M5 及本轮之前的逐项回归、失败根因、平台结果和清理证据统一保存在 [DECISIONS.md](DECISIONS.md)；历史通过不代替当前修改的验证。
- 图片测试已隔离 DATA_DIR 并关闭 relay；历史 12 个 PNG 共 1164 bytes 保留，后续验证不得向源码目录新增产物。
- D3独立业务5xx链路：旧实现三类配置真实SQL失败已复现；修复后真实PostgreSQL、默认标签、人工状态/拥有者、service专项及完整service/handler/routes回归通过；最终四范围vet退出0、lint为0 issues。续接复核三轮repository10.686s、service4.986s通过；原20项仍12 done、3 in_progress、5 pending，父任务done增量0。
- D3发送准入：旧HTTP证据误放行WS的两例失败已复现并修复；专项service32.237s、真实SQL repository151.451s通过；完整service365.157s/handler161.247s/routes15.081s通过，四范围vet退出0、lint 0 issues，默认标签wiring编译通过；文档链接和差异检查通过。
- D3故障补齐与D8夹具修复：SQL组合三轮21.699s、最终service专项三轮13.899s、队列组合20轮101.030s及凭据组合100轮23.462s通过；最终完整service344.703s/handler157.127s/routes12.414s通过，四范围vet/lint退出0且0 issues；默认wiring编译通过。失败及超时尝试保留在决策记录。
- D3协议缺终态：10路径矩阵三轮17.144s、相关流式/桥接组合三轮155.134s通过；最终完整service369.127s/handler168.781s/routes13.528s通过，三范围vet及默认wiring6.220s编译通过；三范围lint两次超时不计通过；实际修改的完整service包lint为0 issues且退出0。原20项统计不变。
- 只将任务源码、测试及本目录三份文档纳入本地交付；保留其他任务资源和历史清理等待，不隐藏未跟踪产物以制造干净状态。C盘运行副本统计修复已保存为effb8271，不计主线完成数，后续开发统一在H盘。

## 当前验证入口

在 backend 目录运行（本地验证，不运行 CI）：

```powershell
$env:PATH='H:/Root/tools/redis/5.0.14.1;'+$env:PATH
# 真实SQL回归须先在本命令进程设置 SUB2API_QUALITY_TEST_DSN，指向隔离PostgreSQL。
go test -tags=unit -p 2 ./internal/service -run 'TestAvailability|TestQuality|TestPelican|TestAccountConnectionTiming' -count=3 -timeout=6m
go test -tags=unit -p 2 ./internal/repository -run 'TestAvailability|TestQuality|TestPelican' -count=3 -timeout=4m
go vet -tags=unit -p 2 ./internal/repository
```

## 下一项工作

D2：D2.1 自动同步准入、D2.2 固定分组证据、D2.3 协议线路身份、D2.4 生效目标统一已完成；Sub2API请求级固定组与倍率确认、OpenAI通用最终HTTP与原生WS握手归属已实现；剩余跨协议计价来源、Live/区域共享额度与缓存域；不能把 AccountOpsGroup 当作 Key 限额键或缓存共享证据。
D3已完成OpenAI发送准入、401/408/429及响应体读取错误失效子范围，已接缺终态EOF失效，专项/完整回归及完整service包lint通过；其他协议/能力、握手前传输、原生WS及缓冲转换待补；D5–D9 主线仍待按设计依赖实施与验收；原20项保持12 done、3 in_progress、5 pending，父任务done增量0。
D2 接着完成跨协议来源、Live/区域及缓存域证据；Gemini生成循环与Antigravity六个发送点、智能重试和静态Key最终准入已实现。HTTP→原生 WS 已按实际握手 Key 共用活跃轮次额度，取消/租约丢失关闭连接且后者不转 HTTP 重放；直接下游 WS 保留 ctx_pool/passthrough 原生协议并按实际握手 Key 逐轮共享，等待后权威终检、终态释放与取消/失效关闭回收已验证；Live/区域仍拒绝。D4 继续核对其他路径、区域传播及模型/fast 来源，原父验收不变。

## 外部验收等待

Windows 当前令牌没有 SeCreateSymbolicLinkPrivilege；系统在 os.Symlink 构造夹具时返回 A required privilege is not held by the client。原测试未改成 skip、未放松断言；Linux 同用例已三轮实际通过。Windows 全量验收的解除条件：由系统管理员提供具有符号链接创建权限的测试进程，再运行原 repository 完整命令；本任务未调整机器安全策略。

## 当前清理阻塞

历史自动审批审核以 blocked by policy 拒绝删除 3 个已验证普通文件，明确路径删除也被拒绝，未改用其他工具规避。
保留在 backend/bin：task-d2-full-tests.jsonl、task-d2-golangci-lint.exe、task-d2-preview.json，共 132004965 bytes（125.890 MiB）；均为已忽略的历史产物，其中 lint 工具仍用于本地验证，可重新构建。现有 12 个受保护图片夹具保持原样。

历史清理等待：backend/tmp/admin-key-pg-d8；自动审批拒绝删除（blocked by policy），未重试或换工具。本轮发现隔离实例已运行并复用，未启动/停止该既有进程；测试schema由夹具清理，最终余数0。解除条件为资源拥有者停止实例后手动清理该准确目录，不影响代码验收。

本轮诊断清理等待：backend/tmp/availability-non5xx-regression共4文件73491bytes，旧源码overlay回归已完成，删除被自动审批以blocked by policy拒绝；保留且未换工具重试，解除条件为手动删除此准确目录。
