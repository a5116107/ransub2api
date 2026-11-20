# 生产监测、验真与调度流程图册

> 核查：2026-11-20，配置快照11:28–11:32（UTC+8）。运行版 `v2.21.0-ransub.8`，发行源码 `1171780c7fc5e38def77dba160ac570a827a45bf`，镜像摘要 `291c5bf6dccd`。这是时间点快照，不是实时面板。
>
> 本文负责**生产事实与流程解释**；唯一方案和任务仍在[主设计§12.1、§13.3](monitor-driven-scheduling-design.md#121-低心智配置与上游监控面板提案)。开发功能不视作生产能力。本轮只读，不触发付费检测、不改配置、不部署、不运行CI。

[打开离线图文版（9张已渲染流程图）](production-observation-flow.html)。HTML由本文和主设计§12.1生成，不是另一份方案。

## 1. 核心判断

目标是“管理员定义产品边界，程序持续证明并维护合格供应”，不是增加分数后继续手工搬账号。生产尚未完整闭环：

1. V2面板的健康色、缓存和首Token等指标，**不直接决定选路资格**。质量规则、原生故障保护、人工开关分别生效。
2. 15个账号只有9、14号开启调度，每个配置并发10。配置上限不等于实时可用容量，也不代表每个模型都有两个合格备选。
3. 主力账号的**报表估算倍率0.10、利润准入倍率1.00、下游售价倍率0.20**。利润门当前关闭，直接开启会排除两个主力账号。
4. **单题通过、整轮通过、处置完成、当前可调度是不同结论**。不能用一个红绿灯代替。
5. 随机推理题、糖果题、返回模型名及Codex/TLS客户端指纹，不单独认证真实模型。当前没有模型身份认证闭环的生产证据。

## 2. 线上上下游与配置

### 2.1 具体拓扑

`A`是账号ID，`G`是分组ID，`K`是本系统下游Key ID。`R`是配置base URL的脱敏地址桶，不是已核实的独立供应商、实际出口或缓存域；凭据和原始站点地址不进入文档。

```mermaid
flowchart TD
  CLIENT["下游 Codex / API 客户端"] --> KEY["K1 · 用户1 · 有效<br/>绑定G2"]
  KEY --> GROUP["G2 · OpenAI<br/>售价D=0.20 · 无用户覆盖<br/>利润门关 · 无fallback组"]
  GROUP --> ROUTE["现有基础选路<br/>资格、连续性、负载与槽位"]
  ROUTE --> A9["A9 · R09 · 调度开<br/>配置并发10<br/>准入1.00 / 估算成本0.10"]
  ROUTE --> A14["A14 · R10 · 调度开<br/>配置并发10<br/>准入1.00 / 估算成本0.10"]
  A9 --> U9["上游Key线路R09<br/>内部账号 / 缓存域未知"]
  A14 --> U14["上游Key线路R10<br/>内部账号 / 缓存域未知"]
  OFF["另13个账号调度关闭<br/>A2/A11/A15另有error状态"] -. "非正常业务候选" .-> ROUTE
  G1["G1 · Anthropic · D=1.00<br/>无账号 / 下游Key绑定"]
  ROUTE -. "被动观测" .-> OBS["用量、错误、性能、V2面板"]
```

所有账号均为OpenAI API Key、优先级1、账号附加销售倍率1、配置并发10。12个地址桶、15个不同Key值，未发现同值Key重复建号；不同地址仍可能共用供应商故障域。

| 账号 | 地址桶 | 状态 / 调度 | 当前关联 | 准入账号倍率U | 报表估算倍率C | 倍率探测 / 账号倍率同步 |
| --- | --- | --- | --- | ---: | ---: | --- |
| A1 | R01 | active / 关 | G2 | 0.20 | 0.20 | 开 / 开；快照有效 |
| A2 | R03 | error / 关 | G2 | 0.08 | 0.08 | 开 / 开；保存的成功快照过期 |
| A3 | R02 | active / 关 | G2 | 1.00 | 0.10 | 开 / 未开启；404不支持；pool_mode=true |
| A4 | R05 | active / 关 | G2 | 1.00 | 0.10 | 开 / 关；404不支持 |
| A5 | R10 | active / 关 | G2 | 1.00 | 0.10 | 开 / 关；404不支持 |
| A6 | R10 | active / 关 | G2 | 1.00 | 0.10 | 开 / 未开启；404不支持 |
| A7 | R12 | active / 关 | G2 | 1.00 | 0.029 | 开 / 关；声明0.029 |
| A8 | R11 | active / 关 | G2 | 1.00 | 0.04 | 开 / 关；声明0.04 |
| A9 | R09 | active / **开** | G2 | **1.00** | **0.10** | 开 / 关；404不支持 |
| A10 | R04 | active / 关 | 无 | 1.00 | 0.03 | 开 / 未开启；声明0.03 |
| A11 | R07 | error / 关 | G2 | 1.00 | 0.10 | 开 / 关；404不支持 |
| A12 | R08 | active / 关 | G2 | 1.00 | 0 | 开 / 未开启；声明0，非已验证免费 |
| A13 | R08 | active / 关 | G2 | 1.00 | 0 | 关 / 关；无快照 |
| A14 | R10 | active / **开** | G2 | **1.00** | **0.10** | 关 / 关；无快照 |
| A15 | R06 | error / 关 | G2 | 1.00 | 0.10 | 开 / 关；无快照 |

“关”只说明保存值，不反推操作者或原因。A5/A6/A14共地址，A12/A13共地址，但Key不同，缓存关系仍未知。A9/A14的sol、terra、6.2-sol均同名映射，映射不是身份认证。

### 2.2 什么真正生效

| 机制 | 快照配置 | 当前影响 |
| --- | --- | --- |
| V2监控 | 开，G2，每60秒刷新 | 展示聚合健康，不自动移组/隔离 |
| V2阈值 | 错误权重0.6、TTFT/缓存各0.2；最小样本10 | TTFT警告8s/严重20s；缓存警告85%/严重60%；不是选路门 |
| V2糖果题 | G2 / gpt-6.1-sol / medium，每分钟 | 分组抽样，不是每个上游Key各测一次 |
| 分组鹈鹕 | G2 / gpt-6.1-sol，每15分钟 | 分组回答/展示，非全组账号验真 |
| 账号质量 | 13条启用，每30分钟，随机推理/high | 动作移出G2、自动恢复开、5xx即时触发关 |
| 普通连通性 | A3/A4/A6/A7共4条，每30分钟 | 原生auto_recover，与质量计划独立 |
| 最新可用性准入 | 未配置；状态表为空 | 尚未启用“最近检测可用且未过期才放行” |
| 性能阈值暂停 | 无账号配置 | 性能可观测不等于自动停调；统计窗源码默认60分钟 |
| 优先评分 | 设置缺省，源码默认关 | advanced scheduler亦显式关；并非统一评分分流 |
| 跨请求健康保护 | **开：5分钟累计3次、暂停1分钟** | 符合分类的失败按账号累计，成功不清零，不是失败率阈值 |
| 倍率元数据探测 | 全局缺省开/30分钟，13个账号开 | 单组有效声明可更新估算C；账号倍率同步另有开关 |
| 利润门 | G1/G2均关 | 报表盈利不等于会拦超倍率线路 |
| V1 / V3空闲探测 | V1无配置，当前V2；V3 idle_probe关 | 不是另外正在运行的主动检测池 |
| 自动采购桶、受管产品、完整多维资格与预算 | 开发或提案 | 不属于本次生产稳定版已验能力 |

## 3. 监测与检测的控制边界

```mermaid
flowchart TD
  BIZ["真实业务尝试"] --> LOG["用量 / 错误日志<br/>进程内HTTP统计"]
  LOG --> V2["V2分组×模型×时间窗<br/>健康色 / 首Token / Token缓存"]
  LOG --> BREAK["符合分类的故障<br/>5分钟3次 → 账号冷却1分钟"]
  LOG -. "性能阈值未配置" .-> PERF["独立性能暂停层"]
  CLOCK["原定时运行器"] --> PRICE["倍率/余额元数据<br/>默认30分钟 + 账号开关"]
  CLOCK --> Q["13条账号质量规则<br/>30分钟 · 随机推理/high"]
  CLOCK --> C["4条普通连通性<br/>30分钟 · auto_recover"]
  CLOCK --> GROUP["G2糖果每分钟<br/>分组鹈鹕每15分钟"]
  Q --> RESULT["答题 + 判题 + 同题对照<br/>通过 / 明确失败 / 不确定"]
  RESULT --> ACTION["所有权校验<br/>移组 / 恢复 / 冲突"]
  PRICE --> SNAP["声明快照 / 估算倍率<br/>可选账号倍率同步"]
  GROUP --> VIEW["分组观测与展示<br/>不授予每个Key资格"]
  MANUAL["手工分组缓存测试"] --> GW["本人下游Key经正常网关<br/>多轮复用实验，可计费"]
  GW --> VIEW
  V2 -. "不是资格写入通道" .-> VIEW
```

过去窗口的高可用率不能覆盖新故障，上次通过也不能覆盖换Key、过期或模型不一致。V2忽略错误含authentication、client_cancelled、content_policy、context_limit、group_access、model_unsupported、not_found、quota_or_balance；面板成功率不是所有请求未经筛选的成功率。

## 4. 质量检测、处置与恢复

```mermaid
flowchart TD
  PLAN["13条质量规则按account_id运行<br/>移出G2后仍可复测"] --> TEST["共享批次生成题目/参考答案<br/>配置模型×样本"]
  TEST --> R{"得到完整可判答案？"}
  R -- "传输/鉴权失败或跳过" --> UNKNOWN["质量不确定<br/>另记运行错误；不新增降智处置"]
  R -- "是" --> JUDGE["LLM判题：G2 / gpt-6.1-terra<br/>排除被测账号，占并发槽"]
  JUDGE --> J{"有效判题结论？"}
  J -- "无可用判题账号 / 失败 / unknown" --> UNKNOWN
  J -- "正确" --> ALL{"整轮所有目标均明确通过？"}
  J -- "错误" --> PEER{"同条件其他账号有通过？"}
  PEER -- "否" --> UNKNOWN
  PEER -- "是" --> REMOVE["版本/租约/所有权校验<br/>移出G2并记录规则拥有的变更"]
  REMOVE --> PLAN
  ALL -- "缺样本或其他模型不确定" --> UNKNOWN
  ALL -- "是" --> OWN{"自动恢复且持有变更？"}
  OWN -- "无需恢复" --> PASS["本轮通过<br/>原停调/人工限制保持"]
  OWN -- "有" --> CHECK{"当前关系仍符合拥有的基线？"}
  CHECK -- "是" --> RESTORE["只恢复该规则移除的关系<br/>刷新调度快照"]
  CHECK -- "否" --> CONFLICT["答案通过，但恢复冲突<br/>不覆盖外部关联或人工限制"]
  POOL["G2也是业务池和失败移组目标"] -. "判题循环依赖风险" .-> JUDGE
```

图中为逻辑归约；处置在原整轮/共享批次收集完成或到达其收集期限、且版本与所有权校验通过后执行，不是单个答案返回就立即改组。

### 4.1 生产实例

| 时间 / 证据 | 实际情况 | 结论 |
| --- | --- | --- |
| 11:00 P12/A9，9184/9185 | terra答错，peer_passed=0/no_passing_peer；sol通过 | 答案一错一对；处置整轮inconclusive，非两个模型都通过 |
| 11:00 P16/A14，9182/9183 | 同样是terra无通过对照、sol通过 | 不新增确认降智，也不恢复整个规则 |
| 10:30 P16/A14，9159/9160 | 两模型正确，动作restore_conflict | 答题通过，恢复未完成，不代表全部恢复 |
| 11:00 P14/A12、P15/A13 | 单模型本轮passed | 调度开关仍false；检测不替管理员打开停调 |
| P13/A10 | 保存移出G2的所有权，当前无关联 | 按ID的复测仍继续 |

当前有质量所有权的规则为P12/P13/P16；A9/A14又在G2中，历史所有权与外部关联需对账，不能由当前关系倒推全部历史原因。

**风险不等于宣称当前没有判题账号。** A9/A14开调度且声明支持terra，通常互为候选，但仍受容量和隔离等约束；若两者都被移组/停调，判题失去来源。应使用不在本产品移组目标中的独立参考池，本轮没有创建它。随机题本地生成参考答案后，仍调用配置LLM比较答案，不是纯本地判分。

## 5. 用户请求如何放行、等待与切换

```mermaid
flowchart TD
  REQ["K1：G2/模型/effort/协议/会话"] --> AUTH["鉴权、余额配额、组权限与能力检查"]
  AUTH --> PRICE["固定本次售价时刻<br/>D=0.20；利润门关"]
  PRICE --> ELIG["当前G2资格<br/>状态/开关/限流/冷却/模型隔离<br/>主要开启者A9、A14"]
  ELIG --> OWNER{"会话或数据归属？"}
  OWNER -- "原线路合格兼容" --> STICKY["优先原账号，保护缓存连续性"]
  OWNER -- "新会话或可合法换线" --> SELECT["基础选路与负载选择<br/>评分/advanced当前关闭"]
  OWNER -- "已知归属不兼容" --> STOP["明确返回原因<br/>不强行跨线续接"]
  STICKY --> SLOT["获取槽位并核对当前配置<br/>共享Key限额仅在已配置时生效"]
  SELECT --> SLOT
  SLOT --> CAP{"取得槽位？"}
  CAP -- "否" --> WAIT["有界等待<br/>黏性配置120s / fallback30s<br/>Key队列30s，非一律相加"]
  WAIT -- "取得槽位并终检通过" --> SEND
  WAIT -- "超时 / 取消 / 资格变化" --> RETRY
  CAP -- "是" --> SEND["终检后发往上游<br/>OpenAI响应头超时配置20s"]
  SEND --> FIN{"完整完成？"}
  FIN -- "是" --> BILL["按实际结果与usage入账<br/>保存观测，延续有效粘性"]
  FIN -- "否" --> RETRY{"可重试、未提交下游输出<br/>未取消、有预算、归属允许？"}
  RETRY -- "是" --> EX["排除失败候选 / 受限重试"]
  EX --> ELIG
  RETRY -- "否" --> STOP
  FIN -. "符合分类故障" .-> HEALTH["健康保护/原生冷却<br/>影响后续请求，不拼接已输出内容"]
```

这是OpenAI文本主链逻辑视图，不承诺HTTP/WS/媒体内部步骤相同。归属未知按已选兼容策略透传，只拦截有证据的跨线数据；未知透传不保证换线成功。HTTP200不等于SSE完整完成，已输出后的断流不透明拼接重试。

20秒是响应头限制，不是首文本SLA或请求总上限。黏性满槽可先等120秒，重试继续耗时。诊断应合并展示“已等多久、在等什么、剩余预算、为何不切换”，而非让管理员分别猜超时。

近24小时留存日志中，无账号归属的一类503在一次查询中有503条，后一次为504条；另有A9/A14归属的502及HTTP200内错误。查询时点不同，且日志不是完整冻结请求全集，不直接相加计算失败率，也不凭状态码认定都是队列问题；需请求ID与尝试链关联后定责。

## 6. 倍率、利润门与报表

```mermaid
flowchart TD
  UP["上游分组/覆盖/高峰声明"] --> PROBE["倍率适配器<br/>Sub2API billing / 已配置New API适配"]
  PROBE --> SNAP["声明快照与期限<br/>成功/失败/不支持"]
  SNAP --> C["可更新估算成本C<br/>extra.cost_multiplier"]
  SNAP --> SYNC{"另开账号倍率同步？"}
  SYNC -- "开且证据有效" --> U["准入/账号统计倍率U<br/>accounts.rate_multiplier"]
  MANUAL["手工账号倍率"] --> U
  D["G2售价 + 用户覆盖 + 高峰<br/>本次D=0.20"] --> GATE["利润门<br/>U ≤ D × (1 - margin - buffer)"]
  U --> GATE
  GATE -. "当前关闭，不做该价格过滤" .-> BIZ["业务选路"]
  BIZ --> USAGE["usage_logs<br/>基数total_cost / 扣费actual_cost"]
  USAGE --> REPORT["只读利润报表<br/>估算成本=total_cost×当前C<br/>估算利润=actual_cost减估算成本"]
  C --> REPORT
  CAUTION["A9/A14：U=1.00，C=0.10<br/>直接启门会因1.00大于0.20全排除"] -. "先预演，不直接开关" .-> GATE
```

- **采购声明倍率**：特定Key/固定组的声明，带来源、期限、模型/服务档；不是已核对的实际结算价。
- **准入U**：利润门使用的账号倍率；开启同步时需新鲜有效单组快照并保守取值。手工值不是供应商价格证据。
- **估算C**：报表使用，缺失默认0.10；默认不能包装成已确认价格。当前报表按当前C计算，不是历史逐请求采购价快照。
- **销售D**：本次固定的销售规则，不随换账号改变；actual_cost为保存的扣费，订阅扣费也不自动等于现金收入。

实时文本利润门公式为 `U ≤ D × (1 - profit_min_margin - profit_safety_buffer)`。上游声明0须核对语义，404只说明元数据接口不支持；不等于模型不能用。真实采购价重构继续延期，高缓存不用于绕过既定倍率上限。

独立图片/视频、Grok媒体、count_tokens、live不直接套用此文本门；混合Responses的token计费部分仍受保护。报表查询的5分钟预算不是上游响应超时。

## 7. 缓存究竟面向谁

### 7.1 实际用量示例

11:30左右读取近24小时usage_logs；跨effort/协议混合，仅解释现状，不作为严格线路比较。

| 本系统账号 / 模型 | 账单记录数 | 归一化Token读取率 | 保存的平均首Token |
| --- | ---: | ---: | ---: |
| A14 / gpt-6.1-sol | 3835 | 96.34% | 15.52s |
| A9 / gpt-6.1-sol | 2664 | 96.51% | 17.48s |
| A9 / gpt-6.2-sol | 79 | 92.05% | 15.77s |
| A14 / gpt-6.2-sol | 4 | 49.67% | 18.09s，低样本 |

共6582条，Token加权读取率约96.37%。这不是供应商全站统计，也不是每个新会话的热缓存保证；百分比不算术平均。高缓存与慢首字可同时存在。

| 面向谁 | 正确范围 | 当前边界 |
| --- | --- | --- |
| 管理员比较采购 | 上游Key/绑定组×模型×协议/服务档×时间窗 | 可看账号用量；内部账号/缓存域未知，G2平均不能贴给每个上游 |
| 下游用户核对服务 | 本人下游Key×销售组×模型×时间窗 | usage有api_key_id；**V2用户聚合按user_id，不是逐Key**。现在只有K1，增加Key后不再等价 |
| 分组状态页 | 获授权的分组/模型统计 | 是分组输出，不证明全部上游；并列忽略错误与覆盖窗口 |
| 主动复用实验 | 同上游Key、scope、前缀的cold→warm | 现手工分组缓存页走本人下游Key和正常调度，属于端到端实验，不是逐上游直测 |

V2归一化公式：`Σcache_read / Σ(input + cache_creation + cache_read)`。Responses/Chat原始input常已含缓存，必须先归一化，不能重复加入。请求命中率另定义为“read>0请求 / 缓存字段可观测请求”，并列覆盖率，缺字段不当0。账单改写后的桶、原始usage与复用实验证据分开。

普通API Key没有统一的“查询全Key缓存命中率”协议。现有Sub2API billing/New API适配主要取倍率、组和余额；请求回传usage时按范围计算。个别站点有授权用量接口，可显式适配并标覆盖，不能假设全支持，也不为统计自动索取站点管理员凭据。模型名与缓存数都可能是自报，属于证据而非独立认证。

## 8. 现有后台为什么复杂

```mermaid
flowchart TD
  ADMIN["新增一个上游"] --> ACC["账号页：Key/模型/状态/并发/调度"]
  ACC --> GROUP["分组页：销售倍率/关联/权限/利润门"]
  ACC --> RATE["倍率探测：探测/同步/估算成本"]
  GROUP --> QUALITY["质量页：题型/模型/周期/动作/恢复"]
  QUALITY --> JUDGE["另配判题分组/参考模型<br/>可能与业务池相同"]
  ACC --> PERF["账号性能阈值 + 全局统计窗"]
  GROUP --> MON["监控页：模式/展示/健康阈值/糖果"]
  GROUP --> PRIORITY["优先调度：开关/范围/策略"]
  GROUP --> ENTITY["业务渠道实体：显式绑定"]
  ENTITY --> PROFIT["利润报表：当前C与保存扣费"]
  RATE --> PROFIT
  QUALITY --> OPS["渠道运维：结论/动作/恢复冲突"]
  PERF --> OPS
  MON --> OPS
  PRIORITY --> OPS
```

| 入口 | 职责 / 易混点 |
| --- | --- |
| `/admin/accounts`、`/admin/groups` | 接入、关联、倍率；库存、销售和判题来源混用分组 |
| `/admin/account-quality` | 单题、整轮、动作和当前资格容易混淆 |
| `/admin/channels/monitor`、`/monitor` | 展示健康阈值不等于隔离阈值 |
| `/admin/priority-scheduling` | 页面存在不等于启用；评分不授予资格 |
| `/admin/channel-operations` | 原运维总览；地址桶不是业务渠道或故障域 |
| `/admin/upstream-channels`、`/admin/profit-operations` | 当前1个实体、仅A1绑定；其余按地址只读归类；并非模型价卡渠道 |
| `/admin/group-cache-tests`、`/admin/pelican-tests` | 端到端抽样/展示，可计费，不是逐Key全覆盖认证 |

根因是**业务对象、证据范围与写入所有权交叉**；把原表单折叠到同一页仍需记住同样多的依赖。

## 9. 优化方向与交付边界

统一方案见[主设计§12.1](monitor-driven-scheduling-design.md#121-低心智配置与上游监控面板提案)：一条供应记录、一个产品策略、一份可解释准入结果；各检测器只产证据，一个拥有者维护受管关系；三步配置，日常只处理异常。

本轮是事实核查与设计，不是已上线的新后台。原28项仍20done/8部分，D3/D4/D5/D6/D7/D8的生产及外部验收不因画图关闭。

## 10. 证据与源码

本地最小证据：`H:/Root/DevCache/evidence/sub2api-production-flow-review/snapshot.json`，只有白名单状态、ID、模型及聚合数，无Key/地址/登录响应/原始正文。只读事务限时8秒，保留SSH主机校验；一次Paramiko EOF切到原生SSH，一次本地解码错误未算成功采集。

源码固定到上述发行提交，不能用开发HEAD替代：

| 对象 | 源码 |
| --- | --- |
| 整轮归约 | [account_quality.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/account_quality.go#L234) |
| 判题来源/超时 | [quality_judge.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/quality_judge.go#L56) |
| 同题对照 | [quality_shared_batch.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/quality_shared_batch.go#L150) |
| 答题到判题 | [pelican_scheduled.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/pelican_scheduled.go#L490) |
| 可用期限 | [availability_policy.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/availability_policy.go#L31) |
| 基础选路 | [openai_gateway_scheduling.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/openai_gateway_scheduling.go#L1206) |
| 累计保护分类 | [openai_apikey_health_breaker.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/openai_apikey_health_breaker.go#L22) |
| 利润准入 | [openai_profit_control.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/openai_profit_control.go#L8) |
| 报表成本/默认 | [account_cost.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/account_cost.go#L10)、[profit_operations.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/profit_operations.go) |
| 倍率探测 | [upstream_billing_probe.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/upstream_billing_probe.go#L35) |
| 缓存归一化公式 | [channel_monitor_v2_repo.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/repository/channel_monitor_v2_repo.go#L1440) |
| 性能暂停 | [account_performance.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/account_performance.go#L38) |
| 默认评分关闭 | [priority_scheduling.go](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/backend/internal/service/priority_scheduling.go#L57) |
| 后台路由 | [router/index.ts](https://github.com/a5116107/ransub2api/blob/1171780c7fc5e38def77dba160ac570a827a45bf/frontend/src/router/index.ts) |

源码证明机制存在，不单独证明已启用、模型真实或供应商兑现价格/缓存承诺。留存数据和本次只读查询也不替代故障会话的完整生产验收。
