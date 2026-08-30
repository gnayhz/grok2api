// Package gateway 是请求路由与实时质量守卫的应用层：账号选择、协议转换后的
// 零延迟判决（quality_retry*.go）、预算化扣留重试循环（service.go）与守卫
// 观测（guard_stats.go）。防降智手段的判决核心全部收敛在本包内、包私有。
package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const (
	ErrorQualityDegraded   = "quality_degraded"
	qualityRetryFailOpen   = "fail_open"
	qualityRetryFailClosed = "fail_closed"
	// defaultQualityMaxAttempts = 2（1 次初始尝试 + 最多 1 次换号重试）：
	// 全局请求预算的一环。历史上限 6 在零延迟拦截落地后失去意义——降智
	// 判定已是毫秒级，串行换号重试的边际收益趋零，反而把单请求时延推高
	// 到下游超时（499 级联）。预算耗尽即 Fail-Closed 503。
	defaultQualityMaxAttempts = 2
	// defaultQualityEvidenceTimeout = 3.5s：降智流已被 0ms 早断截胡，该截止
	// 仅作为网络假死/静默丢包的防死锁兜底（旧值 15s 是"死等密证证据"时代
	// 的产物，在零延迟拦截下纯属浪费客户端时间）；也是健康流思考增量的
	// 到达窗口（实测 clean 首增量 2.1s 内到达）。
	defaultQualityEvidenceTimeout = 3500 * time.Millisecond
	// 首事件截止：降智排队期间上游只发 keepalive 注释或零字节（直连复测
	// response.created 要等 68-125s；clean 恒定 0.8-2.2s），比证据截止更早
	// 一档掐断换号。
	defaultQualityCreatedTimeout     = 5 * time.Second
	defaultMissingThinkingCooldown   = 12 * time.Hour
	lastErrorMissingThinking         = accountdomain.LastErrorMissingThinking
	lastErrorMissingThinkingDisabled = accountdomain.LastErrorMissingThinkingDisabled
	// An empty stream that idles while held is treated as an account-quality
	// failure: the request can still rotate before any bytes reach the client.
	// 空流冷却默认 15m：空流多与出口 IP/瞬态相关（RSC clean 会自动解除），
	// 重冷却收益低；真降智走 missing-thinking 路径而非此处。
	qualityIdleAccountCooldown = 15 * time.Minute
)

var (
	errQualityDegraded    = errors.New("上游响应缺少推理")
	errQualityEmptyStream = errors.New("上游流式响应为空")
	// errQualityEvidenceTimeout 标记流式零证据截止：静默期超过预算仍无
	// 思考证据且无可见输出。按空闲路径处理（短冷却+RSC 归因+重试），
	// 不计入 missing-thinking 惩罚，也不作为指纹熔断。
	errQualityEvidenceTimeout = errors.New("上游流式响应零证据超时")
	// errQualityCreatedTimeout 标记首事件截止：静默期内连一个 SSE data
	// 事件都未到达（直连复测：降智排队期间上游只发 keepalive 注释或不发
	// 任何字节，response.created 要等 68-125s；clean 恒定 0.8-2.2s）。按
	// 空闲路径处理（短冷却+RSC 归因+重试）。
	errQualityCreatedTimeout = errors.New("上游流式响应首事件超时")
)

// QualityRetryRuntime is the isolated request-path withhold/retry policy.
// Zero Enabled leaves production behavior unchanged.
type QualityRetryRuntime struct {
	Enabled     bool
	MaxAttempts int
	OnExhausted string
	// AccountCooldown 是 missing-thinking 定罪的账号冷却。
	AccountCooldown time.Duration
	// SameAccountRetry retries the withholding account once before switching.
	// Tunnel-pool egress rotates the exit IP per request, so one same-account
	// retry distinguishes transient exit-IP pollution (retry delivers) from a
	// degraded account (retry still withholds, then the account is penalized
	// and, when the budget still has room, the next attempt switches; at the
	// default budget of 2 the retry is itself the last attempt). The retry
	// consumes the attempt budget.
	SameAccountRetry bool
	// IdleAccountCooldown 是空流/静默超时的账号冷却，独立于 missing-thinking
	// 的 AccountCooldown（二者诱因与置信度不同：空流常与出口 IP 相关）。
	// 0 = 默认 15m。
	IdleAccountCooldown time.Duration
	// EvidenceTimeout 是流式请求的零证据截止（0=默认 3.5s）：静默期超过
	// 该时长仍无思考证据且无任何可见输出时中止该次尝试。降智流已被
	// item.done 零延迟拦截截胡，该截止仅是网络假死/静默丢包的防死锁兜底。
	EvidenceTimeout time.Duration
	// CreatedTimeout 是流式请求的首事件截止（0=默认 5s）：任何 SSE data
	// 事件到达前中止该次尝试。直连复测（Python+curl+h2+socks 绕过网关）
	// 证实该延迟在上游时钟内：clean 0.8-2.2s（与复杂度无关），降智
	// 68-125s（排队期间仅有 keepalive 注释或零字节）。
	CreatedTimeout time.Duration
	// GuardedModels 是守卫介入的模型白名单（requestRetry.guardedModels）：
	// 非空时仅名单内模型进入判决，其余模型整体豁免（台账 model_out_of_scope）。
	// 空 = 全部推理模型介入（向后兼容默认）。守卫的价值集中在主力推理模型
	//（grok-4.5/4.6）；对边缘/退役模型介入只会产出噪声拦截与误罚
	//（实证：grok-4.3 四连 quality_degraded 503，而它不在运营
	// 关切内）。条目匹配 public/upstream 模型名，"grok-4.6" 前缀覆盖
	// "grok-4.6-xhigh" 等档位后缀别名。
	GuardedModels []string
}

// qualityStreamSignals is the hold classifier input. Tests drive this
// directly and via observeQualityChunk on SSE fixtures.
type qualityStreamSignals struct {
	// HasThinking 是否已收到有效思考增量（可见的 reasoning/thinking 文本
	// delta——密文、注释、usage 声明都不算）。
	HasThinking bool
	// ReasoningEndedWithoutThinking 推理阶段已闭合（output_item.done 携
	// reasoning item / thinking content_block_stop）且未产出任何思考增量：
	// 这是降智包的确切签名，零延迟拦截的触发信号。
	ReasoningEndedWithoutThinking bool
	VisibleTokens                 int64
	ReasoningTokens               int64
	OutputTokens                  int64
	Terminal                      bool
}

// QualityVerdict is the hold decision for one upstream stream.
type QualityVerdict string

const (
	QualityWait     QualityVerdict = "wait"
	QualityDeliver  QualityVerdict = "deliver"
	QualityWithhold QualityVerdict = "withhold"
)

// QualityRetryAction is what the attempt loop does with a withhold verdict.
type QualityRetryAction string

const (
	QualityActionDeliver     QualityRetryAction = "deliver"
	QualityActionDeliverLast QualityRetryAction = "deliver_last"
	QualityActionRetry       QualityRetryAction = "retry"
	QualityActionReject      QualityRetryAction = "reject"
)

func normalizeQualityRetry(cfg QualityRetryRuntime) QualityRetryRuntime {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = defaultQualityMaxAttempts
	}
	if cfg.AccountCooldown <= 0 {
		cfg.AccountCooldown = defaultMissingThinkingCooldown
	}
	if cfg.IdleAccountCooldown <= 0 {
		cfg.IdleAccountCooldown = qualityIdleAccountCooldown
	}
	if cfg.EvidenceTimeout <= 0 {
		cfg.EvidenceTimeout = defaultQualityEvidenceTimeout
	}
	if cfg.CreatedTimeout <= 0 {
		cfg.CreatedTimeout = defaultQualityCreatedTimeout
	}
	cfg.OnExhausted = normalizeQualityExhaustionPolicy(cfg.OnExhausted)
	return cfg
}

// Attributor is the risk-attribution surface the gateway consumes; the
// concrete risk.Service satisfies it. egressNodeID carries the egress node
// that served the degraded attempt (0 = direct/untraced) so a clean RSC
// verdict can be routed into exit-IP quarantine instead of account penalty.
type Attributor interface {
	OnDegraded(ctx context.Context, credential accountdomain.Credential, egressNodeID uint64)
}

// EgressDegradationObserver receives request-level exit-IP degradation
// evidence. The egress application service implements it to run cross-account
// confirmation and (with RSC attribution disabled or pending) node quarantine.
type EgressDegradationObserver interface {
	OnEgressDegraded(ctx context.Context, nodeID, accountID uint64)
	// MarkDegradeEvidence applies the pending node soft-cooldown after one
	// degrade verdict so other accounts stop hitting the suspect exit until
	// attribution confirms or the escalated deadline expires.
	MarkDegradeEvidence(nodeID uint64)
}

// UpdateAccountRisk installs the attribution hook; nil keeps it unset.
func (s *Service) UpdateAccountRisk(attributor Attributor) {
	if attributor == nil {
		return
	}
	s.accountRisk.Store(attributor)
}

func (s *Service) accountRiskAttributor() Attributor {
	if value, ok := s.accountRisk.Load().(Attributor); ok {
		return value
	}
	return nil
}

// UpdateEgressGuard installs the exit-IP degradation observer; nil keeps it
// unset.
func (s *Service) UpdateEgressGuard(observer EgressDegradationObserver) {
	if observer == nil {
		return
	}
	s.egressGuard.Store(observer)
}

func (s *Service) egressDegradationObserver() EgressDegradationObserver {
	if value, ok := s.egressGuard.Load().(EgressDegradationObserver); ok {
		return value
	}
	return nil
}

func (s *Service) UpdateQualityRetry(cfg QualityRetryRuntime) {
	normalized := normalizeQualityRetry(cfg)
	s.qualityRetry.Store(&normalized)
}

func (s *Service) qualityRetryConfig() QualityRetryRuntime {
	if s == nil {
		return normalizeQualityRetry(QualityRetryRuntime{})
	}
	if value := s.qualityRetry.Load(); value != nil {
		return *value
	}
	return normalizeQualityRetry(QualityRetryRuntime{})
}

// classifyQualityHold decides whether a held stream may be forwarded.
// Streamed visible thinking (reasoning/summary text deltas) always delivers.
// An empty reasoning item header, encrypted_content ciphertext, SSE comment
// lines (keepalive included), and usage claims are NOT evidence — degraded
// upstreams fill all four while never streaming visible thinking.
// 实测（测试环境 A/B 抓流）：RSC risk 降智账号的流携带 encrypted_content
// 密文但零可见思考增量；clean 账号的流在密文之外还有成串的
// reasoning_summary_text.delta。密文两边都有，毫无判别力——只有可见思考
// 增量能把降智流与健康流分开。
//
// 判决顺序（零延迟状态机，见 ANTI_DEGRADATION_ARCHITECTURE §四）：
//  1. 收到思考增量：瞬间放行直通（客户端不再经过守卫）。
//  2. 推理阶段闭合却无增量（密文降智包）：0 毫秒瞬间拦截——这是降智的
//     确切签名，无需等待任何超时或缓冲阈值。
//  3. 正文抢跑（无思考却出正文）：瞬间拦截。推理模型对每个回答都会思考，
//     健康流的思考增量必然先于任何正文；不论长度，无思考的正文即降智。
//  4. 终态兜底拦截（零输出零思考的流由空流短路先行接管，此处防御性）。
//  5. 其余：继续等待思考增量；静默挂起（排队/假死）由证据截止
//     （EvidenceTimeout=3.5s，空闲路径：短冷却+RSC 归因+重试）收口，而不是
//     判成 missing-thinking（12h 长冷却）——空流多与出口 IP/瞬态相关
//     （蓝图 §2#7 保留空流短冷却语义），误罚排队中的干净账号代价过高。
//
// 两个收口点约定（分类器在各收集点之前/之后运行的精确分层）：
//   - 空流短路在本分类器之前运行，但 reasoningEndedWithoutThinking 是
//     负证据、压制空流分类（EOF 补齐的降智末行按扣留而非空流收口）；
//     redacted_thinking 既非证据也非负证据，仅它时仍走空流（可能是健康
//     账号的隐私脱敏，round 46/47）。
//   - 非流式请求的 body 同样进入判决：Responses 原生形状与转换后的
//     chat/messages 客户端形状共用同一证据语义（round 41 起）。
func classifyQualityHold(sig qualityStreamSignals) QualityVerdict {
	// ReasoningTokens is audit metadata that may come from the upstream
	// usage claim; degraded streams report large counts while never emitting
	// reasoning events, so only observed events (HasThinking) deliver.
	if sig.HasThinking {
		return QualityDeliver
	}
	// 规则 2：推理阶段已闭合且未产出任何思考增量（item.done/content_block_stop
	// 携 encrypted_content 结束）——零延迟直接扣留。
	if sig.ReasoningEndedWithoutThinking {
		return QualityWithhold
	}
	// 规则 3：无思考却出正文（正文抢跑）——瞬间拦截，与长度无关。
	// 推理模型对每个回答都会思考：语料复核证实续写轮同样
	// 思考（未指定强度 136/153、显式低强度 12/18 均产生思考，零思考仅
	// 出现在降智时刻）。规模轮 16 引入的续写豁免（「推理已在历史完成」）
	// 据此推翻——它放行了 17 条零思考交付（REASONING0_LEDGER §C2/C3）。
	if sig.VisibleTokens > 0 || sig.OutputTokens > 0 {
		return QualityWithhold
	}
	// 规则 4：终态兜底拦截（含续写轮——同上；零输出零思考的流由空流
	// 短路先行接管，此处防御性）。
	if sig.Terminal {
		return QualityWithhold
	}
	// 规则 5：初始等待思考增量到达。
	return QualityWait
}

// qualityPeekAbortError prefers the idle-timeout cause over a plain
// context.Canceled so the attempt loop can retry instead of treating the
// abort as a client 499.
func qualityPeekAbortError(ctx context.Context, err error) error {
	if ctx != nil {
		if cause := context.Cause(ctx); neterrorpkg.IsUpstreamStreamIdleTimeout(cause) {
			return cause
		}
	}
	if neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
		return err
	}
	if err != nil {
		return err
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// isClientRequestCancel reports a real client disconnect. Upstream idle
// timeouts cancel the same context and must not be classified as 499.
func isClientRequestCancel(ctx context.Context, err error) bool {
	if neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
		return false
	}
	if ctx != nil && neterrorpkg.IsUpstreamStreamIdleTimeout(context.Cause(ctx)) {
		return false
	}
	if ctx != nil && ctx.Err() != nil {
		return true
	}
	return errors.Is(err, context.Canceled)
}

// decideQualityRetry caps withhold recovery at maxAttempts (default 2:
// original + one rotated account). The last withhold
// (attemptIndex == maxAttempts-1) is fail-open unless OnExhausted is fail_closed
// (the default), which rejects instead.
func decideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction {
	if verdict != QualityWithhold {
		return QualityActionDeliver
	}
	if maxAttempts <= 0 {
		maxAttempts = defaultQualityMaxAttempts
	}
	if attemptIndex < 0 {
		attemptIndex = 0
	}
	if attemptIndex < maxAttempts-1 {
		return QualityActionRetry
	}
	// attemptIndex == maxAttempts-1 (or past it): do not retry again.
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

// boundQualityRetry turns a Retry into DeliverLast/Reject when the routing
// loop has no remaining account slot, so the already-held body is not dropped
// on continue-into-exhausted-loop.
func boundQualityRetry(action QualityRetryAction, hasNextRoutingAttempt bool, onExhausted string) QualityRetryAction {
	if action != QualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	if normalizeQualityExhaustionPolicy(onExhausted) == qualityRetryFailClosed {
		return QualityActionReject
	}
	return QualityActionDeliverLast
}

func normalizeQualityExhaustionPolicy(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), qualityRetryFailOpen) {
		return qualityRetryFailOpen
	}
	return qualityRetryFailClosed
}

// QualityCommit is the single attempt-loop decision for a held stream.
type QualityCommit struct {
	Action   QualityRetryAction
	Audit    bool
	KeepBody bool
}

// commitQualityHold is the shipped withhold/retry/commit unit. The attempt
// loop must not re-derive this from Decide+Bound+switch.
func commitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool, onExhausted string) QualityCommit {
	action := boundQualityRetry(
		decideQualityRetry(verdict, qualityAttempt, maxAttempts, onExhausted),
		hasNextRouting,
		onExhausted,
	)
	switch action {
	case QualityActionRetry, QualityActionReject:
		return QualityCommit{Action: action, Audit: true, KeepBody: false}
	case QualityActionDeliverLast:
		return QualityCommit{Action: action, Audit: false, KeepBody: true}
	default:
		return QualityCommit{Action: QualityActionDeliver, Audit: false, KeepBody: true}
	}
}

// 质量守卫豁免原因令牌：guard-stats 按 token 计数（exempts 列表）。此前
// 豁免完全无痕——线上 previous_response_id 续聊链连续降智全部
// 裸奔（旧构建 ownership 豁免），事后只能靠 token 链反推是哪条路径放行。
const (
	QualityExemptDisabled         = "disabled"              // requestRetry.enabled=false
	QualityExemptSkipInput        = "skip_input"            // 受信网关侧分类器显式跳过
	QualityExemptOperation        = "operation"             // 非推理操作（image/media/embedding...）
	QualityExemptCompaction       = "compaction"            // TUI compaction 请求
	QualityExemptProvider         = "provider"              // 非 Build/Console 供应商
	QualityExemptMessagesNoThink  = "messages_thinking_off" // Messages 协议未请求 thinking
	QualityExemptModelScope       = "model_out_of_scope"    // 模型不在守卫白名单（guardedModels）
	QualityExemptModelNoReasoning = "model_no_reasoning"    // 目标模型不支持推理
)

// qualityHoldExemptReason 返回守卫不介入该请求的原因；空串表示应介入。
// 判定顺序与 shouldHoldQualityStream 完全一致（后者是它的布尔投影），
// 新增豁免路径必须两处同步演进。
func qualityHoldExemptReason(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime) string {
	// 非流式与流式同样纳入 hold：peekQualityBody 对完整 body 判决，证据规则
	// 一致（此前 !input.Streaming 豁免导致非流式降智响应直接交付，
	// 实测复现；修复后 clean/risk 的 summary 区分 11/11）。
	// previous_response_id 钉账号（attempt 预算=1）不等于豁免质量守卫：
	// 续聊降智一旦放行会写进 stored response，后续轮次全部被污染。
	if !cfg.Enabled {
		return QualityExemptDisabled
	}
	if input.skipQualityHold {
		return QualityExemptSkipInput
	}
	switch operation {
	case audit.OperationChat, audit.OperationResponses, audit.OperationMessages, "":
	default:
		return QualityExemptOperation
	}
	// TUI compaction is a normal /v1/responses body (no compaction_trigger).
	// Keep this defensive body check in addition to skipQualityHold so a caller
	// that bypasses CreateResponse cannot withhold a 100s+ summary as missing-thinking.
	if isResponsesCompactionRequest(input.Body) {
		return QualityExemptCompaction
	}
	if route.Provider != accountdomain.ProviderBuild && route.Provider != accountdomain.ProviderConsole {
		return QualityExemptProvider
	}
	// 模型范围白名单（requestRetry.guardedModels）：非空时守卫只介入名单内
	// 的模型，其余直接豁免——运营上守卫只对主力推理模型（grok-4.5/4.6）
	// 有价值，边缘模型介入只产出噪声拦截与误罚（grok-4.3 四连
	// quality_degraded 503 实证）。空名单 = 全部推理模型照旧介入。
	if !qualityModelGuarded(cfg, input.PublicModel, route.UpstreamModel) {
		return QualityExemptModelScope
	}
	// 判定只看流特征,不看请求体里的工具标记(线上实证:同一 agent
	// 会话 52 轮只有第 1 轮有守卫,后 51 轮全部裸奔,唯一一条降智原样交付)。
	// 上游曾按"带 function_call_output/hosted 工具"豁免整轮——扣留的响应从不
	// 发给客户端,客户端不可能重放其中的工具调用;而纯语义输出(工具调用形态)
	// 的流本来就会按特征 Deliver。请求体携带什么与这条响应是否降智无关。
	// （曾有 reasoning_disabled 豁免：请求体显式关闭推理即放行。
	// 删除——守卫白名单内的模型（grok-4.5/4.6）均不支持 none，显式关闭是
	// 非法组合，上游直接 400，豁免与否结果相同；白名单外的模型在上一行已
	// 整体豁免。若未来把支持 none 的模型纳入白名单，应将其排除在名单外而
	// 非恢复此豁免。）
	// Anthropic Messages 协议未请求 thinking 的【流式】请求：转换器以
	// ThinkingEvidenceComment 内部注释保留思考证据，守卫照常判决
	// （修正：上游对未指定强度的请求按默认强度思考——首轮
	// 36/36、续写 136/153 均有思考，零思考即降智；原整体豁免放行了
	// 15 条零思考交付，见 REASONING0_LEDGER §C2）。非流式 body 没有注释
	// 通道、转换后也不含思考块，证据无法注入——保留豁免，为已知残留
	// 覆盖面缺口（同档 §C2 r4a，1 例）。
	if operation == audit.OperationMessages && !input.Streaming && !qualityMessagesThinkingEnabled(input.Body) {
		return QualityExemptMessagesNoThink
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) {
		return ""
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel) {
		return ""
	}
	return QualityExemptModelNoReasoning
}

func shouldHoldQualityStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime) bool {
	return qualityHoldExemptReason(input, ownership, route, operation, cfg) == ""
}

// qualityModelGuarded 报告模型是否在守卫白名单内（cfg.GuardedModels 为空
// = 全部模型）。条目匹配 public 或 upstream 模型名；"grok-4.6" 前缀覆盖
// "grok-4.6-xhigh" 等档位后缀别名。
func qualityModelGuarded(cfg QualityRetryRuntime, publicModel, upstreamModel string) bool {
	if len(cfg.GuardedModels) == 0 {
		return true
	}
	candidates := []string{strings.ToLower(strings.TrimSpace(publicModel)), strings.ToLower(strings.TrimSpace(upstreamModel))}
	for _, entry := range cfg.GuardedModels {
		name := strings.ToLower(strings.TrimSpace(entry))
		if name == "" {
			continue
		}
		for _, candidate := range candidates {
			if candidate == name || strings.HasPrefix(candidate, name+"-") {
				return true
			}
		}
	}
	return false
}

// qualitySearchSilenceBudget 是携带服务端搜索工具的请求在守卫侧的静默
// 预算：取 24h（事实上的“不截止”），死连接由传输层流空闲超时（Build
// 默认 2m）兜底。见 service.go 扣留点注释（生产回归）。
const qualitySearchSilenceBudget = 24 * time.Hour

// qualityHeavyReasoningCreatedBudget：重推理（high/xhigh）请求的首事件预算。
// 轨迹摸底：high 档出现 P0 排队静默（首事件 >5s），
// 5s 首事件截止误杀后换号又撞出口冷却 → 502。排队是上游负载行为而非
// 账号降智（同池同刻 low 档首事件 0-6ms、created 后增量仍即时）。取 30s，
// 仍远低于传输层流空闲 2m。
const qualityHeavyReasoningCreatedBudget = 30 * time.Second

// qualityLivenessSchedule：请求类感知的活跃度预算制度表（轨迹摸底的
// 架构产物）。守卫的两条轴正交：证据规则（规则 1/2/3，零延迟，
// 请求类无关——33+ 捕获含 Console 通道全部同签名）与活跃度截止（排队界，
// 非降智证据，前提随请求类变化）。表按请求侧信号给预算：
//
//	任意工具（搜索/函数）  | 无界    | 无界     | 服务端搜索三位静默（t 系实证）；客户端函数调用的静默组织期 + chat
//	                               |          | 转换器延迟摘要窗口使转换流零事件（z2 实证）。传输层 2m 兜底；证据规则照常。
//	搜索工具（web/x）     | 无界    | 无界     | P0/P1/P2 三位静默皆合法（搜索排队/执行/思考），传输层 2m 兜底
//	重推理（high/xhigh）  | 30s     | 不变     | P0 排队实测 >5s；created 后干净增量 0-6ms；D-b 降智晚到 5.6-16s 由规则 2 定罪
//	其余                  | 默认 5s | 默认 3.5s| 干净流 created 后首增量 0-6ms（n≈25）
//
// deadline 触发只代表排队界，不构成降智证据；降智判定永远由证据规则承担。
func qualityLivenessSchedule(body []byte, operation string, cfg QualityRetryRuntime) QualityRetryRuntime {
	// 转换后流（chat/messages 操作）的证据可见性被结构性推迟：转换器为
	// raw 优先去重把 summary 增量延迟到 reasoning item.done 才发射（z2/q2
	// 批次实证——q1/q3 high 档仅因 30s heavy 行幸存，q2 low 档无工具被
	// 3.5s 证据截止误杀）。证据截止的“首增量 ~2.1s”前提只对原始流成立；
	// 转换流不适用。规则 1/2/3 在 item.done/正文恢复瞬间照常判降智，死连接
	// 由传输层流空闲兜底。
	converted := operation == "chat" || operation == "messages"
	// 单次解析同时取 tools 与 effort（基准：128KB body 两次全量解析
	// 1.3ms/请求，合并后减半；小 body ~3µs 本可忽略，长对话场景值得）。
	// 历史消息（续写检测）不再解析：语料复核推翻了「续写轮可
	// 合法零思考」假设（未指定强度 136/153、显式低强度 12/18 均思考），
	// 续写豁免随之删除——判决不再需要请求历史，省去逐消息结构化解析。
	var probe struct {
		ReasoningEffort string `json:"reasoning_effort"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Tools []struct {
			Type string `json:"type"`
		} `json:"tools"`
		// tool_choice 可能是字符串（"auto"/"none"/"required"）或对象
		//（{"type":"tool","name":"web_search"} 等强制形态）。用 RawMessage
		// 避免 string 字段遇对象形态产生 UnmarshalTypeError 令整个探测失败
		// ——规模轮 76 实证：forced 对象形态曾使探测回退默认行，chat 转换
		// 请求连 converted 无界证据行都落空，搜索静默期被 3.5s 误杀 504。
		ToolChoice json.RawMessage `json:"tool_choice"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return cfg
	}
	search := false
	// tool_choice:"none" 时工具被显式禁用（规模轮 12 发现归一化层也会
	// 注入禁用态工具）——禁用态不改变时序形态，按无工具处理。其余一切
	// 形态（"auto"/"required"/对象强制）均视为启用。
	toolsEnabled := !bytes.Equal(bytes.TrimSpace(probe.ToolChoice), []byte(`"none"`))
	for _, tool := range probe.Tools {
		// 任意工具（服务端搜索 / 客户端函数 / MCP）都改变时序形态：z2 批次
		// 实证——chat 转换器把 summary 增量延迟到 item.done（raw 优先），函数
		// 调用的静默组织期内转换流零事件，3.5s 证据截止误杀工作中的请求。
		if toolsEnabled && strings.TrimSpace(tool.Type) != "" {
			search = true
			break
		}
	}
	effort := probe.ReasoningEffort
	if probe.Reasoning != nil && probe.Reasoning.Effort != "" {
		effort = probe.Reasoning.Effort
	}
	effort = strings.ToLower(strings.TrimSpace(effort))
	heavy := effort == "high" || effort == "xhigh"
	if search {
		cfg.EvidenceTimeout = qualitySearchSilenceBudget
		cfg.CreatedTimeout = qualitySearchSilenceBudget
	} else if converted {
		// 转换流（chat、messages，与 effort 无关）：证据截止不适用——可见性被
		// 结构性推迟到 item.done。规模轮 106 实证：行序曾让 heavy 压过 converted，
		// xhigh 思考 >30s 未到 item.done 即被 30s 证据截止误杀（ai5 批次，原始流
		// 27 条思考增量被杀于进行中；q1/q3 high 档仅因思考时长 <30s 幸存）。
		// heavy 仅收紧 created 首事件截止（created 事件直通转换器不受推迟影响）。
		cfg.EvidenceTimeout = qualitySearchSilenceBudget
		if heavy {
			cfg.CreatedTimeout = qualityHeavyReasoningCreatedBudget
		}
	} else if heavy {
		cfg.EvidenceTimeout = qualityHeavyReasoningCreatedBudget
		cfg.CreatedTimeout = qualityHeavyReasoningCreatedBudget
	}
	return cfg
}

func jsonStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

// qualityMessagesThinkingEnabled 报告 Anthropic Messages 请求是否显式启用了
// thinking。必须与转换器的发射门（messages_request.go 的 thinkingEnabled）
// 使用同一词表：type=enabled 与 type=adaptive 都会让转换器输出 thinking_delta
// （证据通道存在，守卫应当介入）——此前 adaptive 被这里漏掉，自适应思考的
// 请求整体绕过守卫。未启用时转换器不输出思考增量，扫描器无证据通道。
func qualityMessagesThinkingEnabled(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(payload["thinking"], &nested) != nil {
		return false
	}
	if !jsonStringEquals(nested["type"], "enabled") && !jsonStringEquals(nested["type"], "adaptive") {
		return false
	}
	var budget int64
	if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget <= 0 {
		return false
	}
	return true
}

// commitableSameAccountRetry reports whether the withhold path may retry the
// same account once. Same-account retry is meaningful ONLY through a rotating
// egress pool (Selection.Pool): pool members hand every request a DIFFERENT
// exit IP, so one retry cleanly separates transient exit-IP pollution (retry
// delivers) from a degraded account (retry withholds again). Under direct or
// fixed-egress deployments the retry re-enters the same dirty IP with ~0%
// recovery probability — burning 5-10s of the request budget for nothing —
// so it is force-disabled there (blueprint item #9).
// Quota-probe leases are excluded: RetryAccount only re-queues normal
// candidates, so a probe lease would silently switch accounts while the log
// still claims a same-account retry.
func commitableSameAccountRetry(cfg QualityRetryRuntime, used bool, quotaProbe bool, selection *selectionSession, poolEgress bool) bool {
	return cfg.SameAccountRetry && poolEgress && !used && !quotaProbe && selection != nil
}

func (s *Service) applyMissingThinkingPenalty(ctx context.Context, requestID string, credential accountdomain.Credential, cooldown time.Duration) {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	action, err := s.selector.markMissingThinking(writeCtx, credential, cooldown)
	if err != nil {
		s.logger.Error("quality_degraded_penalty_failed", "request_id", requestID, "account_id", credential.ID, "action", action, "error", err)
		return
	}
	switch action {
	case missingThinkingPenaltyDisabled:
		s.logger.Info("quality_degraded_disabled", "request_id", requestID, "account_id", credential.ID)
	case missingThinkingPenaltyCooled:
		s.logger.Info("quality_degraded_cooldown", "request_id", requestID, "account_id", credential.ID, "cooldown", cooldown.String())
	}
}
