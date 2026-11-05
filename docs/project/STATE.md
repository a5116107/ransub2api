# 项目状态

## 恢复检查点

- 任务：D2区域共享Key本地验收及收尾完成，待本地提交；独立身份、SQL共用预算、策略同步、HTTP/WS持有期与缩减恢复已验证。不运行CI、不推送/部署/生产迁移。
- 源码：H盘唯一工作树，分支`work/monitor-driven-scheduling`，本轮起点`161b3fb`；交付身份以`git log -1`及状态为准。
- 最近证据：冻结cf29c626源码组合125.255s、58顶层无skip/fail；race99.177s、vet、默认入口、迁移包与目标包lint通过。恢复核对config1.826s、slots0.637s及control编译通过；无活跃验证句柄。
- 下一动作：D2 Live持久交接及其他协议缓存域/跨协议来源，再接D3/D4；区域子范围完成不替代D2父验收。
- 等待：无新增外部阻塞；Windows符号链接权限及历史清理审批由原拥有者解除。原20项12 done、3 in_progress、5 pending，父done增量0。

## 目标与入口

更新日期：2026-11-05。倍率范围内多供应商自动调度，保护质量、完整响应体验与会话缓存，便于同步原仓库。
- [唯一设计与任务定义](monitor-driven-scheduling-design.md)
- [追加式决策与验证历史](DECISIONS.md)
- 用户授权本地实施、修复、相关清理与验证、本地提交；真实采购价格不在本期范围。生产执行仍需对应授权。

## 任务进度

| 任务 | 状态 | 已验证范围 / 剩余验收 |
| --- | --- | --- |
| D1 | done | 原配置覆盖、倍率口径、共享Key缺口与最小补丁边界已记录 |
| D2 | in_progress | D2.1–D2.4、最终HTTP/WS与多协议发送绑定、公共共享Key重复摘要及回收恢复已验证；桥接线路/过期及生效模型/fast托管缓存、HTTP客户端状态归属完成；区域共享Key身份/SQL预算/HTTP/WS及缩减恢复完成；跨协议来源、Live及完整缓存域待验收 |
| D2.1 | done | 自动同步新鲜度、高峰值域与在线/预演同源，30类边界、完整回归和CLI验证通过 |
| D2.2 | done | New API声明组与唯一完整路由同组；异组/多组拒绝和恢复、绑定保存、三轮回归及六包lint通过 |
| D2.3 | done | 地址/协议/接入模式/模型规则统一线路身份；四路径旧证据失效、实际协议切换及9平台发送前拦截通过 |
| D2.4 | done | 探测与New API授权/列表/绑定/事务使用生效地址；旧源码6例失败、修复四包三轮通过 |
| D3 | in_progress | 最新availability、业务5xx独立、401/408/429/读错误、14路径缺终态、缓冲/原生WS及取消计费、实际联网无响应故障已验证；其他协议/能力、未观测传输及WS探测作用域待补 |
| D4.1 | done | 双故障/账号消失拒绝、回源与未装门边界，专项和完整回归通过 |
| D4.2 | done | 配置失败/空对象/错组拒绝，HTTP/WS/免费及恢复；原断言、默认标签、vet及Linux完整通过 |
| M1 | done | 6项未引用辅助符号与死计数器删除，unused 6→0 |
| M2 | done | 错误检查247→0；1072顶层用例通过、6既有skip保留，vet与重复回归通过 |
| M3 | done | WinSock拒绝/不可达识别；超时/重置不误判；真实POST及代理/WS组合5轮通过 |
| M4 | done | 三处原子持久化收敛；22项Windows失败修复，双平台回归/三包lint/vet通过 |
| M5 | done | 生产依赖与测试夹具边界明确，三处兼容点说明；六包lint 0及禁止依赖反向验证通过 |
| D4 | in_progress | 固定售价、严格请求新鲜策略、7平台权威账号刷新及全部发送点终检/门传播已验证；依赖D2、区域传播和模型/fast来源 |
| D5 | pending | timing与缓存分层/冷启动估计；依赖D2/D3 |
| D6 | pending | 分配与缓存保护；依赖前置指标与准入 |
| D7 | pending | 前端配置与决策解释；依赖策略契约 |
| D8 | pending | admin/跨层SQL、WS绑定及重试截止夹具已验证；集成/升级/全局CLI仍待验收，不运行CI |
| D9 | pending | 依赖前述验收及生产执行授权 |

## 工作区与验证约定

- 唯一工作树：`H:/Root/jiaoben/01_AI接口与网关/sub2api`；origin为ranxi2001/sub2api。基线`6de829fa2efa801dcf3071c4efe842198c7724be`，不推官方上游。
- 使用项目Go1.27.3；不降低go.mod。lint工具`backend/bin/task-d2-golangci-lint.exe` v2.15，完整包、unit标签、并发2、8分钟与原规则。
- 真实Redis仅进程PATH前置`H:/Root/tools/redis/5.0.14.1`；真实SQL须进程环境指向隔离PostgreSQL，不打印DSN。
- 图片测试使用隔离DATA_DIR、关闭relay；既有`backend/internal/service/data/`12个PNG共1164bytes保留，不纳入任务提交。
- 42df0d6/b00c535/161b3fb：线路/模型/服务档与HTTP客户端状态归属已验证；专项、race、完整service/handler/routes、vet、lint与默认入口的准确证据见DECISIONS，不能代替本轮源码验证。
- 本轮区域共享Key：64竞争/公共租约和原WS失败组合就绪池三轮55.387s；最终58顶层125.255s、race99.177s、vet、默认入口1.454s、迁移包1.465s、完整config/regional/repository lint 0 issues，实际顺序退出0。冷建连和生产升级不计通过。
- 不新增平行台账、配置或缓存实现，不用账号运营组充当Key额度键/缓存域证据。C盘副本统计effb8271不计主线完成数，开发统一在H盘。

## 下一项工作

依序完成D2 Live及其他协议缓存域、跨协议来源，补D3作用域和D4模型/fast来源；随后D5–D7、D8、D9。Live保守拒绝保留；区域共享额度使用同一SQL预算和受信任策略，不以局部测试替代混合版本升级或生产验收。
模型/服务档隔离只证明请求续接边界，不证明供应商缓存实际存活、内部不降级、跨档同价或高缓存率。API Key长历史仍受既有12条尾部及工具边界guard约束。

## 外部等待与清理

- Windows令牌缺`SeCreateSymbolicLinkPrivilege`，原repository完整测试在os.Symlink报权限错误；未改skip。Linux同例三轮通过。拥有者：系统管理员；解除：提供可创建符号链接的进程，再运行原Windows完整命令。
- 历史删除被审批`blocked by policy`拒绝，不换工具绕过。`backend/bin`三文件：task-d2-full-tests.jsonl、task-d2-golangci-lint.exe、task-d2-preview.json，共132004965bytes；lint仍使用。解除：拥有者按准确路径手动清理可回收产物。
- `backend/tmp/admin-key-pg-d8`删除同被拒绝；既有隔离实例由原拥有者管理，不启动/停止。测试schema已清零；解除：资源拥有者停止实例并手动清理准确目录。
- `backend/tmp/availability-non5xx-regression`4文件73491bytes，旧源码overlay验证完成，删除被拒绝；解除：拥有者手动删除准确目录。
- `backend/tmp/regional-key-pg`停止及删除的整条命令被自动审批`blocked by policy`拒绝，未执行；恢复确认55449实例PID12708仍运行。保留该独立夹具，拥有者停止并手动清理；不改用其他工具绕过。
