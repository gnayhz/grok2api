package gateway

import (
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
	ErrorQualityDegraded      = "quality_degraded"
	qualityRetryFailOpen      = "fail_open"
	qualityRetryFailClosed    = "fail_closed"
	defaultQualityMaxAttempts = 6
	// 默认 hold 30s：生产实测 grok-4.6 的加密思考密文在流末尾到达（复杂
	// 生成可晚于任何短预算），短 hold 会把健康流扣成"超时+可见输出"路径。
	// 配合截止放行语义（超时+流开放+有输出=不确定→放行不惩罚），30s 是
	// "等密文窗口"而非误扣源；降智路径已由 EarlyHeaderAbort 在头阶段拦截。
	defaultQualityHoldTimeout     = 3 * time.Second
	defaultQualityEvidenceTimeout = 15 * time.Second
	defaultQualityCreatedTimeout  = 5 * time.Second
	defaultQualityMinOutput       = int64(8)
	// defaultTerminalBurstThreshold：交付后"整包末尾爆发+零思考"签名的账号
	// 连击熔断阈值（0/缺省=3）。见 terminalBurstTracker。
	defaultTerminalBurstThreshold    = 3
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
	// errQualityHeaderBudget 标记响应头预算早断。它刻意不链 context.Canceled：
	// 取消的是本次调用的子 context，父请求仍在进行，误判成客户端取消会
	// 错误地中断整个重试循环。
	errQualityHeaderBudget = errors.New("上游响应头迟滞（降智路径特征）")
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
	HoldTimeout time.Duration
	// EarlyHeaderAbort 是“响应头预算”实验性早断：实测健康推理路径的头恒定
	// 在秒级返回（与生成长度无关），而降智路径连头都要等整个生成完成
	// （复杂问题可达 30s+）。quality hold 激活时，头超过该预算未返回即中止
	// 本次上游调用并换路径重试，把“降智判定”从首字节提前到头阶段。每请求
	// 至多触发一次（触发后即解除），避免对系统性慢路径误杀。0 表示关闭。
	EarlyHeaderAbort time.Duration
	MinOutputTokens  int64
	OnExhausted      string
	AccountCooldown  time.Duration
	// SameAccountRetry retries the withholding account once before switching.
	// Tunnel-pool egress rotates the exit IP per request, so one same-account
	// retry distinguishes transient exit-IP pollution (retry delivers) from a
	// degraded account (retry still withholds, then the account is penalized
	// and the next attempt switches). The retry consumes the attempt budget.
	SameAccountRetry bool
	// IdleAccountCooldown 是空流/静默超时的账号冷却，独立于 missing-thinking
	// 的 AccountCooldown（二者诱因与置信度不同：空流常与出口 IP 相关）。
	// 0 = 默认 24h。
	IdleAccountCooldown time.Duration
	// EvidenceTimeout 是流式请求的零证据截止（0=默认 15s）：静默期超过该
	// 时长仍无思考证据且无任何可见输出时中止该次尝试。复杂提示词的降智
	// 静默期实测 75-121s（2026-08-21 魔法球实测，总耗时 582s/9.7min），而
	// 干净流首思考增量 2.1s 即达——截止把降智流式尝试压到预算内，客户端
	// 总死寂从 ~10min 降到 ~2min 以内。
	EvidenceTimeout time.Duration
	// CreatedTimeout 是流式请求的首事件截止（0=默认 5s）：任何 SSE data
	// 事件到达前中止该次尝试。直连复测（Python+curl+h2+socks 绕过网关）
	// 证实该延迟在上游时钟内：clean 0.8-2.2s（与复杂度无关），降智
	// 68-125s（排队期间仅有 keepalive 注释或零字节）。
	CreatedTimeout time.Duration
	// TerminalBurstThreshold 是交付后降智签名的账号熔断阈值（0=默认 3）：
	// 连续 N 条流式交付满足"首字节时间>=总时长且零思考 token 且输出达到
	// 降智最小口径"时，按 missing-thinking 语义处罚账号并触发 RSC 归因。
	// 这是流级守卫之外的纵深防御，捕获豁免路径/超大 body fail-open/
	// deliver_last 的残余放行（2026-08-27 线上续聊链 7 连发零惩罚的对策）。
	TerminalBurstThreshold int
}

// QualityStreamSignals is the hold classifier input. Tests drive this
// directly and via ObserveQualityChunk on SSE fixtures.
type QualityStreamSignals struct {
	HasThinking     bool
	VisibleTokens   int64
	ReasoningTokens int64
	OutputTokens    int64
	Terminal        bool
	HoldExpired     bool
	// OversizedLine 表示扫描器遇到无法解析的超长行：内容证据不可靠，分类
	// 器 fail-open 放行（与 4MiB 缓冲上限同语义）。
	OversizedLine bool
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
	if cfg.HoldTimeout <= 0 {
		cfg.HoldTimeout = defaultQualityHoldTimeout
	}
	if cfg.MinOutputTokens <= 0 {
		cfg.MinOutputTokens = defaultQualityMinOutput
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
	if cfg.TerminalBurstThreshold <= 0 {
		cfg.TerminalBurstThreshold = defaultTerminalBurstThreshold
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

// ClassifyQualityHold decides whether a held stream may be forwarded.
// Streamed visible thinking (reasoning/summary text deltas) always delivers.
// An empty reasoning item header, encrypted_content ciphertext, the Chat SSE
// reasoning-start comment, and usage claims are NOT evidence — degraded
// upstreams fill all four while never streaming visible thinking. 2026-08-20
// 实测（测试环境 A/B 抓流）：RSC risk 降智账号的流携带 encrypted_content
// 密文但零可见思考增量；clean 账号的流在密文之外还有成串的
// reasoning_summary_text.delta。密文两边都有，毫无判别力——只有可见思考
// 增量能把降智流与健康流分开。
// Reasoning models think on EVERY answer, however short: a healthy stream
// emits reasoning before any content, so any observed content without visible
// thinking is degraded regardless of length, and withholding fires as early
// as minOutput allows (尽早拦截，避免污染上下文). A sample with no output
// at all keeps waiting (also after hold expiry): an empty hang must not
// fail-open as HTTP 200.
func ClassifyQualityHold(sig QualityStreamSignals, minOutput int64) QualityVerdict {
	if minOutput <= 0 {
		minOutput = defaultQualityMinOutput
	}
	// ReasoningTokens is audit metadata that may come from the upstream
	// usage claim; degraded streams report large counts while never emitting
	// reasoning events, so only observed events (HasThinking) deliver.
	if sig.HasThinking {
		return QualityDeliver
	}
	// OversizedLine 不再 fail-open。生产 grok-4.6 xhigh 降智流会在
	// output_item.done 上推数 MiB encrypted_content（无可见思考），扫描器
	// 在换行到达前就会积满 1MiB 未完成行；旧逻辑据此放行，把降智答案
	// 原样交给客户端并污染会话。超长行在扫描器里按类型丢弃/分流，这里
	// 只看思考证据与可见输出。
	output := sig.OutputTokens
	if output < sig.VisibleTokens {
		output = sig.VisibleTokens
	}
	if output <= 0 {
		return QualityWait
	}
	if sig.Terminal || sig.HoldExpired || output >= minOutput {
		return QualityWithhold
	}
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

// DecideQualityRetry caps withhold recovery at maxAttempts (default 6:
// original + five extra accounts). The last withhold
// (attemptIndex == maxAttempts-1) is fail-open unless OnExhausted is fail_closed.
func DecideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int, onExhausted string) QualityRetryAction {
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

// BoundQualityRetry turns a Retry into DeliverLast/Reject when the routing
// loop has no remaining account slot, so the already-held body is not dropped
// on continue-into-exhausted-loop.
func BoundQualityRetry(action QualityRetryAction, hasNextRoutingAttempt bool, onExhausted string) QualityRetryAction {
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

// CommitQualityHold is the shipped withhold/retry/commit unit. The attempt
// loop must not re-derive this from Decide+Bound+switch.
func CommitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool, onExhausted string) QualityCommit {
	action := BoundQualityRetry(
		DecideQualityRetry(verdict, qualityAttempt, maxAttempts, onExhausted),
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
// 豁免完全无痕——2026-08-27 线上 previous_response_id 续聊链 7 连发降智全部
// 裸奔（旧构建 ownership 豁免），事后只能靠 token 链反推是哪条路径放行。
const (
	QualityExemptDisabled         = "disabled"              // requestRetry.enabled=false
	QualityExemptSkipInput        = "skip_input"            // 受信网关侧分类器显式跳过
	QualityExemptOperation        = "operation"             // 非推理操作（image/media/embedding...）
	QualityExemptCompaction       = "compaction"            // TUI compaction 请求
	QualityExemptProvider         = "provider"              // 非 Build/Console 供应商
	QualityExemptReasoningOff     = "reasoning_disabled"    // 请求体显式关闭推理（effort=none 等）
	QualityExemptMessagesNoThink  = "messages_thinking_off" // Messages 协议未请求 thinking
	QualityExemptModelNoReasoning = "model_no_reasoning"    // 目标模型不支持推理
)

// qualityHoldExemptReason 返回守卫不介入该请求的原因；空串表示应介入。
// 判定顺序与 shouldHoldQualityStream 完全一致（后者是它的布尔投影），
// 新增豁免路径必须两处同步演进。
func qualityHoldExemptReason(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime) string {
	// 非流式与流式同样纳入 hold：peekQualityBody 对完整 body 判决，证据规则
	// 一致（此前 !input.Streaming 豁免导致非流式降智响应直接交付，2026-08-20
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
	// 判定只看流特征,不看请求体里的工具标记(2026-08-25 线上实证:同一 agent
	// 会话 52 轮只有第 1 轮有守卫,后 51 轮全部裸奔,唯一一条降智原样交付)。
	// 上游曾按"带 function_call_output/hosted 工具"豁免整轮——扣留的响应从不
	// 发给客户端,客户端不可能重放其中的工具调用;而纯语义输出(工具调用形态)
	// 的流本来就会按特征 Deliver。请求体携带什么与这条响应是否降智无关。
	// Aliases are rewritten before this gate, so inspect the effective request
	// body instead of only the reasoning-capable base model. In particular,
	// grok-4.3-none becomes grok-4.3 plus an explicit disabled setting.
	if qualityRequestDisablesReasoning(input.Body) {
		return QualityExemptReasoningOff
	}
	// Anthropic Messages 协议只有在客户端显式请求 thinking 时才会把上游思考
	// 转换为可见的 thinking_delta；未请求 thinking 的请求没有思考证据通道，
	// 守卫无从区分健康与降智（2026-08-20 实测：clean 账号的合法 messages 请求
	// 被误判缺少推理，二连扣留后 503）。此类请求豁免 hold。
	if operation == audit.OperationMessages && !qualityMessagesThinkingEnabled(input.Body) {
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

func qualityRequestDisablesReasoning(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if jsonStringEquals(payload["reasoning_effort"], modeldomain.ReasoningEffortNone) {
		return true
	}
	for _, key := range []string{"reasoning", "output_config", "thinking"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(payload[key], &nested) != nil {
			continue
		}
		if jsonStringEquals(nested["effort"], modeldomain.ReasoningEffortNone) || jsonStringEquals(nested["type"], "disabled") {
			return true
		}
		var budget int64
		if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget == 0 {
			return true
		}
	}
	return jsonStringEquals(payload["thinking"], "disabled")
}

func jsonStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

// qualityMessagesThinkingEnabled 报告 Anthropic Messages 请求是否显式启用了
// thinking（{"thinking":{"type":"enabled",...}}）。未启用时转换器不会向
// 客户端输出任何思考增量，质量扫描器在该协议上没有有效证据通道。
func qualityMessagesThinkingEnabled(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	var nested map[string]json.RawMessage
	if json.Unmarshal(payload["thinking"], &nested) != nil {
		return false
	}
	if !jsonStringEquals(nested["type"], "enabled") {
		return false
	}
	var budget int64
	if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget <= 0 {
		return false
	}
	return true
}

// qualityHeaderBudget returns the active response-header budget for this
// attempt: only while the quality hold is engaged, configured, streaming.
// 预算对每次流式尝试生效（非单发）：健康流式的响应头恒定秒级返回
// （0.7-2.2s 含代理），降智复杂生成的头要等整个生成完成（实测 75-300s）；
// 单发解除会让第二次慢头尝试悬挂满 5 分钟 ResponseHeaderTimeout
// （2026-08-21 魔法球实测 368s 中的 300s 即来源于此）。
func qualityHeaderBudget(cfg QualityRetryRuntime, holdEnabled, streaming, armed bool) time.Duration {
	if !holdEnabled || !streaming || !armed || cfg.EarlyHeaderAbort <= 0 {
		return 0
	}
	return cfg.EarlyHeaderAbort
}

// commitableSameAccountRetry reports whether the withhold path may retry the
// same account once: enabled, unused for this request, and a selection
// session exists to re-queue the account (pinned/forced-egress requests skip
// the hold entirely, so a nil session here is defensive only). Quota-probe
// leases are excluded: RetryAccount only re-queues normal candidates, so a
// probe lease would silently switch accounts while the log still claims a
// same-account retry.
func commitableSameAccountRetry(cfg QualityRetryRuntime, used bool, quotaProbe bool, selection *selectionSession) bool {
	return cfg.SameAccountRetry && !used && !quotaProbe && selection != nil
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
