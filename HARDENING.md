# 加固记录（Hardening Log）

> 本文档记录基于外部审查报告（D:\sc 1-9.md）与 16 轮多维度循环审查的全部生产修复、
> 验证基础设施与核心不变量。基线：fork 自 chenyme/grok2api v3.1.4（909bb810），上游零漂移。

## 一、实时路由守卫（requestRetry）

### 检测规则（R1-R6，实验证实）
- R1 终态无推理一律扣留（与长度无关：推理模型对任何回答都思考，33/33 零重叠）
- R2 证据 = 非空推理**文本增量**；SSE 注释/项头/usage 声明均不算（降智流会开项但不流文本）
- R3 hold 超时粘性：超时后到达的小输出立即扣留，不退回等待
- R4 响应头预算早断 earlyHeaderAbort（健康头 0.7-6.8s 恒定 vs 降智 12-75s；每请求单发）
- R5 空流（terminal+零内容+零推理；usage 声明不能洗白，含 usage 先到的合并形态）
- R6 >1MiB 单行 fail-open（解析不可靠；优先于空流判定）

### 拦截链
扣留 → 同号重试×1（probe 通道来源账号永不参与，wasQuotaProbeCandidate 兜底转正翻转）
→ 换号（预算 6）→ fail_closed（503）/ fail_open（保底交付）。

## 二、RSC 风险归因

- 触发：质量扣留 → 异步 grok.com RSC 检查（经关联 Web SSO；admission 去重 + 64 上限）
- **split maps**：admissionDedup（credential.ID）与 checkInflight（webID）键空间隔离；同身份并发检查 singleflight 合并（等待者共享结果）
- denied/flagged：verdict 永久缓存（saveVerdictGuarded：error 不覆盖 denied）；onDenied=flag 标记**整个身份组**（Web+Build+Console 独立渠道）为 rsc_denied——保持启用但永不调度，直到人工解除（UI 编辑开关 → PATCH riskStatus）
- clean：仅解除 missing_thinking 家族与 quality_idle 冷却；泛型 5xx 永不清除
- 启动对账 ReconcileRiskyVerdicts：游标分页（10000/页）收敛 risk_status 到 verdict 表
- patrol：15min tick 按 bucketDays 复查 clean/error（risky 永不复查）

## 三、冷却分类（三族独立）

| 族 | 标记 | 触发 | 解除 |
|---|---|---|---|
| 实时路由守卫 | missing_thinking（二击停用 missing_thinking_disabled）| 无推理扣留 | 到期或 clean RSC |
| 实时路由守卫 | quality_idle_timeout | 空流/2min 静默（定向两列写，不碰 failure_count）| 到期或 clean RSC |
| 路由 | upstream status NNN | 泛型失败指数退避 | 到期（clean 不清除）|

## 四、安全加固（P1×4 + P2×1）

1. 非凭证类上游 4xx（400/404/422）不再原文透传客户端 → 受控 UpstreamFailure 信封（chat 与 stored-response 两路径）
2. OAuth 结构化错误字段统一过 redactOAuthDiagnosticText（防 refresh_token/client_secret 回显入 LastRefreshErrorMessage/日志/admin API）
3. 审计清洗键族扩至 10 族（含 client_secret/password/sso_token/session_token/bare token=）
4. RSC 裁决 botFlagDetails/Error 在持久层集中 redact（防 compromise 上游夹带秘密）
5. 解析器非有限分数守卫（risk=NaN/Inf 会毒化 GORM/json——fuzz 种子实证）

## 五、生命周期与并发

- reconcile/patrol 由 Run() 的 background WaitGroup 托管：关停先取消任务再等退出，随后才关库；一次性对账完成后 <-taskCtx.Done() 挂起（runSupervisedTask 会重启任何返回者——曾致生产崩溃循环）
- patrol 用 NewTicker+Stop（弃 time.After 泄漏）
- 取消语义三测：等待者取消有界返回 / 并发闸超时 leader 关 done / leader 中途取消不误判 clean
- 并发流隔离实证：32 流×交错分块，零串话零丢帧
- 扫描器 scratch 复用（allocs -18%）：字符串别名安全由专项回归锁定

## 六、验证矩阵（scripts/verify.sh）

- fast（make verify）：build / vet / staticcheck / race 全量
- full（make verify-full）：+ fuzz 种子 / govulncheck / flaky 探针 count=3
- fuzz（make fuzz）：SSE 扫描器与 RSC 解析器各 30s 变异（曾达 845 万执行零崩溃）
- 补充：coverage 基线（risk 75.7% / rsc 86.2%）、errorCode 覆盖索引（COVERING INDEX 实测）

## 七、运维入口

- 审计页 errorCode 过滤（quality_degraded 预设）与「拦截」指标卡（周期降智扣留次数；原 dashboard 质量健康卡已移除）
- 账号行冷却原因悬停（missing_thinking/空流/泛型区分）+ rsc_denied 徽标与手动解除
- settings 页「实时路由守卫与 RSC 风险归因」卡片说明 config-only 项与重启要求

## 八、生产实测（截至 2026-08-20）

- 24h：244 请求 / 42 次降智扣留 / **88% 经重试链最终成功交付** / 5 空流拦截 / 5 真耗尽 503
- 优雅停机：SIGTERM 快速退出零错误；重启 reconcile 幂等（identities:7）
- 日志量 24h 224 行，无刷屏
## 九、补充轮次（17-22）

- **上游零漂移**：fork 基线即 upstream/main HEAD，无 rebase 冲突风险（round 17 实测）
- **生产冒烟**：真实流式请求 200/6.5s/reasoning_content/[DONE]，全链路无回归（round 18）
- **i18n 一致性自动化**：i18n.test.ts 双不变量（键集合 + 插值占位符）；首跑抓 10 处真实漂移（9 个 zh 缺键含复数键族 + 1 个占位符漂移）并修复（round 19）
- **probe-lane E2E**：TestProbeLaneNeverSameAccountRetries 闭合审计 P0#2 的 E2E 缺口——probe 账号扣留后恰好尝试 1 次、直接换号、惩罚不豁免（round 20；至此第 1 轮审计 8 项全清）
- **对抗复审第二波（审 8-20 轮）双 P1**：
  1. scratch 复用字段复活——json.Unmarshal 复用切片背衬不清零缺席字段，上一帧 content 被重复计数（复现测试实证 19→38 runes）；修复：复用前逐元素清零（round 21，自查先于复审命中）
  2. CJK 诊断误伤——redactOAuthDiagnosticText 的 ≥80 字节长字段清洗会整体抹除无空格中文描述（strings.Fields 不切 CJK）；修复：长字段清洗限定纯 ASCII（round 21）
- **并发负载实测**：16 路并发流式 → 13 成功 / 3 上游 429 快速传播（2.3s fail-fast 非挂起；干净池 4 账号下 upstream resource exhausted 正确分类 account_scoped）；p50 9.6s；13/13 交付流全部 verdict=deliver；负载后零 panic 零重启（round 22）
- **运维注意（P3）**：request_audits 大表（PostgreSQL）首建 idx_audits_error_code_created_id 在启动事务内同步执行，超大表可能延迟就绪——与既有迁移风格一致，按需再引入并发迁移路径

## 十、sidecar 移除与品牌剥离（rounds 23-24）

- **出口质量守护 sidecar 全量移除**（tools/egress-quality-guard、主动探测、被动 TPS 分类、节点隔离、probe profile、轮换 webhook、compose profile、内部探测身份、降智账号面板）：主动探测覆盖不了两次探测之间的降智，被动 TPS 是事后归因（上下文已污染），生产从未启用。
- **实时路由守卫零改动保留**：配置键 `qualityGuard.requestRetry` → 顶层 `requestRetry`，`QualityGuardConfig` 壳结构体删除；旧键由 KnownFields 显式拒绝（TestLegacyQualityGuardKeyRejected 锁定），requestRetry 支持配置热加载。
- **二轮复查补漏（round 25）**：① docker/entrypoint.sh 与 Dockerfile 的 sidecar 状态目录 `/var/lib/grok2api-quality-guard` 创建逻辑（每次启动空跑，已删）；② ForcedEgressNodeID 死链——管理员质量测试端点删除后全链无生产 setter 的尸体（provider 字段、adapter 消费分支、gateway Input 字段与传递、selector.AcquireForKeyOnEgressNode 方法与 forcedEgressNodeID 参数过滤、守卫永假排除条件、3 个死测试），出生提交 137589c3（sidecar 链首个提交）；账号绑定的 WithEgressNode/EgressNodeFromContext 基础设施无关，保留。
- dashboard 质量健康卡移除；「拦截」指标卡迁至请求审计页。
- 品牌清理：settings 卡片、README 双语、示例配置不再出现「质量守卫 / qualityGuard」；出口节点冷却措辞回归传输失败语义。

## 十一、五提交全量对抗审查（round 27，909bb810 之后全部提交）

三审查员并行（流量分类路由 / sidecar 移除正确性 / 加固与前端），协调者逐条验证后修复：
- **P0（已修）**：4xx 消毒被换号绕过——service.go 消毒分支要求 lastFailure == nil，首跳 429（设置 lastFailure）换号后次跳 400 跳过消毒原文透传（TestNonRetryable4xxAfterAccountSwitchStaysSanitized 锁定）。
- **P1（已修）**：订阅卫生漏 sticky 模板——repo 层卫生无 cipher 解不了密，订阅刷新可把规则固定目标改成 {account} 模板绕过节点编辑守卫；syncSource 尾部补应用层 enforceRouteRuleHygieneAfterSync（TestSyncHygiene* 锁定）。
- **P1（已修）**：审计页「拦截」卡把 i18n 语言码当 IANA 时区传 dashboard API → 后端 400 → 恒显 0；改用 Intl resolvedOptions 时区 + error 态显示 -。
- **P2（已修）**：README 双语 ForcedEgress 尸句删除；requestRetry 示例补 earlyHeaderAbort/sameAccountRetry/accountCooldown 并标注默认关闭；clientkey 空 const () 清理。
- 已查干净：流量分类绑定优先级、节点管理 CRUD 与基线一致、守卫 ForcedEgress 永假条件删除无副作用、requestRetry 热加载、i18n 双不变量、NaN/Inf 守卫、scratch 清零、CJK 脱敏。
- 遗留清单（未修，低风险）：stored-response 404/410 原文 body 直出（P1，建议下轮）；OAuth 脱敏嵌套 JSON/URL 编码绕过面（P1）；RSC 日志未 redact（P1）；审计 sanitizer camelCase/URL 编码缺口（P2）；决策表测试缺口（A-P1-3）；verify.sh STATES 未定义+缺工具仍记 ok（P2）；400 信封 5xx 措辞（P2）。
- **三轮复查补漏（round 26，六新维度：git 提交级权威清单 / API 路由面 / swagger / i18n 孤儿键 / DB schema / 构建配置）**：① .dockerignore 4 条 sidecar 死白名单；② dashboard i18n 孤儿键 qualityTitle/requestQualitySummary（已删卡片的键尸体，双语清除，i18n 奇偶测试锁定）；③ README 双语与 assignment.go 注释沿用 sidecar「quarantine/隔离」黑话（autoAssign 界限功能合法保留，措辞改为节点下线/evacuation）。git 权威清单（sidecar 链 15 提交触碰 83 文件）现存文件零残留；swagger/路由面/建表语句零残留。
