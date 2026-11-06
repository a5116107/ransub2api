# 项目状态

## 恢复检查点

<!-- resume:start -->
- 任务：D2普通OAuth的Codex用量/模型清单探测与Agent Identity签名密钥接入共享Key额度；父任务仍在进行。
- 源码：H盘唯一工作树`work/monitor-driven-scheduling`，本轮基于d40ac0c；仅本地修改与隔离验证，不CI/推送/部署/生产迁移。
- 证据：旧实现漏控而发出CONNECT已复现；Bearer探测两轮真实Redis直连/Astra race通过；Agent Identity公钥验证与真实Redis/loopback两轮race通过；models manifest 直连准入回归通过；unit vet、默认编译、原规则lint 0；详见DECISIONS。
- 下一动作：继续D2其他未知签名、插件外独立出站及跨请求后台额度，再推进D3/D4和D5–D9；不猜测不同运行时是否同密钥。
- 等待：历史清理/符号链接与生产等待不变；原20项12 done、3 in_progress、5 pending。
<!-- resume:end -->

## 目标与入口

更新日期：2026-11-07。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 用户授权本地实施、修复、相关清理与验证、本地提交；真实采购价格不在本期范围。生产执行仍需对应授权。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置覆盖、倍率口径、共享Key缺口与最小补丁边界已记录 |
| D2 | in_progress | D2.1–D2.4、共享Key/Live、turn-state、Gemini、HTTP/原生WS来源、批量下载与持久额度、BPS缓存来源、共享Key头大小写、Astra/区域WS、V1探测、Bedrock签名、OAuth用量/模型清单直连准入、Agent Identity公钥准入已验证；其他来源待验收 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 最新availability及故障分类已验证；BPS账号线路纯文本专属探测已验证，其余能力、托管连接、未观测传输及WS作用域待补 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 6项未引用辅助符号与死计数器删除，unused 6→0 |
| M2 | done | 错误检查247→0；1072顶层用例通过、6既有skip保留，vet与重复回归通过 |
| M3 | done | WinSock拒绝/不可达识别；超时/重置不误判；真实POST及代理/WS组合5轮通过 |
| M4 | done | 三处原子持久化收敛；22项Windows失败修复，双平台回归/三包lint/vet通过 |
| M5 | done | 生产依赖与测试夹具边界明确，三处兼容点说明；六包lint 0及禁止依赖反向验证通过 |
| D4 | in_progress | 固定售价、严格请求新鲜策略、7平台及BPS独立发送权威终检已验证；仍依赖D2、区域传播和模型/fast来源 |
| D5 | pending | timing与缓存分层/冷启动估计；依赖D2/D3 |
| D6 | pending | 分配与缓存保护；依赖前置指标与准入 |
| D7 | pending | 前端配置与决策解释；依赖策略契约 |
| D8 | pending | admin/跨层SQL、WS绑定、重试截止及批量资金/竞争/回滚16项夹具已验证；其余集成/升级/全局CLI待验收，不运行CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用项目Go1.27.3；不降低go.mod。lint工具`backend/bin/task-d2-golangci-lint.exe` v2.15，完整包、unit标签、并发2与原规则；service预算12分钟，其余8分钟。
- 真实Redis仅进程PATH前置`H:/Root/tools/redis/5.0.14.1`；真实SQL须进程环境指向隔离PostgreSQL，不打印DSN。
- 图片测试使用隔离DATA_DIR、关闭relay；既有`backend/internal/service/data/`12个相同测试PNG共1164bytes，历史精确删除曾被策略拒绝（DECISIONS），保留且不提交。
- 既有HTTP/compat归属、区域共享Key与Live的实际版本、完整/局部验证边界及失败历史见DECISIONS；不代替本轮WS源码或生产升级验收。区域竞争使用就绪SQL池，不计冷建连时延通过。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

D2继续AgentAssertion、非标准鉴权、绕过HTTPUpstream及跨请求后台额度等边界，再补D3其他能力、D4区域传播及D5–D9。BPS未观测能力仍保守拒绝；旧批量缺来源任务保持等待。共享策略启用前排空旧版本及远端批量任务；受限Live沿4.4.2，区域沿原SQL预算。
模型/服务档隔离只证明请求续接边界，不证明供应商缓存实际存活、内部不降级、跨档同价或高缓存率。API Key长历史仍受既有12条尾部及工具边界guard约束。

## 外部等待与清理

- Windows令牌缺`SeCreateSymbolicLinkPrivilege`，原repository完整测试在os.Symlink报权限错误；未改skip。Linux同例三轮通过。拥有者：系统管理员；解除：提供可创建符号链接的进程，再运行原Windows完整命令。
- 历史删除被审批`blocked by policy`拒绝，不换工具绕过。`backend/bin`三文件：task-d2-full-tests.jsonl、task-d2-golangci-lint.exe、task-d2-preview.json，共132004965bytes；lint仍使用。解除：拥有者按准确路径手动清理可回收产物。
- `backend/tmp/admin-key-pg-d8`删除同被拒绝；既有隔离实例由原拥有者管理，不启动/停止。测试schema已清零；解除：资源拥有者停止实例并手动清理准确目录。
- `backend/tmp/availability-non5xx-regression`4文件73491bytes，旧源码overlay验证完成，删除被拒绝；解除：拥有者手动删除准确目录。
- `backend/tmp/regional-key-pg`停止及删除的整条命令被自动审批`blocked by policy`拒绝，未执行；恢复确认55449实例PID12708仍运行。保留该独立夹具，拥有者停止并手动清理；不改用其他工具绕过。
- 历史overlay已清除；准确空目录`backend/tmp/live-key-verification`的边界校验后删除命令仍被自动审批`blocked by policy`拒绝，整条未执行。拥有者手动删除该空目录；不重试其他删除工具。
- 历史`backend/tmp/ws-turn-state-verification`的边界校验与`Remove-Item -WhatIf`删除预演整条被自动审批`blocked by policy`拒绝，未执行实际删除。保留13文件77603133bytes，拥有者按准确目录手动清理；不换工具/移动绕过。

- 历史`backend/tmp/gemini-route-pg`递归删除命令被自动审批`blocked by policy`拒绝，整条未执行；随后独立停止55451成功，测试schema为0。保留约42MB目录，拥有者手工删除准确路径；不重试删除/移动绕过。
- 本次`backend/tmp/batch-key-verification`删除预演被自动审批`blocked by policy`拒绝，命令未执行；保留目录，由拥有者按准确路径手动清理。
- 本次 `frontend/tmp/channel-monitor-ui-fixture` 精确删除命令被自动审批 `blocked by policy` 拒绝，未执行；保留三文件及目录，拥有者按准确路径手动清理。
