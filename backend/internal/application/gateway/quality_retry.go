package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

const (
	ErrorQualityDegraded             = "quality_degraded"
	qualityRetryFailOpen             = "fail_open"
	qualityRetryFailClosed           = "fail_closed"
	defaultQualityMaxAttempts        = 6
	defaultQualityHoldTimeout        = 3 * time.Second
	defaultQualityMinOutput          = int64(32)
	defaultMissingThinkingCooldown   = 24 * time.Hour
	lastErrorMissingThinking         = accountdomain.LastErrorMissingThinking
	lastErrorMissingThinkingDisabled = accountdomain.LastErrorMissingThinkingDisabled
	// An empty stream that idles while held is treated as an account-quality
	// failure: the request can still rotate before any bytes reach the client.
	qualityIdleAccountCooldown = 24 * time.Hour
)

var (
	errQualityDegraded    = errors.New("上游响应缺少推理")
	errQualityEmptyStream = errors.New("上游流式响应为空")
	// errQualityHeaderBudget 标记响应头预算早断。它刻意不链 context.Canceled：
	// 取消的是本次调用的子 context，父请求仍在进行，误判成客户端取消会
	// 错误地中断整个重试循环。
	errQualityHeaderBudget = errors.New("上游响应头迟滞（降智路径特征）")
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
	cfg.OnExhausted = normalizeQualityExhaustionPolicy(cfg.OnExhausted)
	return cfg
}

// Attributor is the risk-attribution surface the gateway consumes; the
// concrete risk.Service satisfies it.
type Attributor interface {
	OnDegraded(ctx context.Context, credential accountdomain.Credential)
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
// Thinking deltas always deliver. Reasoning models think on EVERY answer,
// however short: a healthy stream emits reasoning before any content, so any
// observed content without thinking is degraded regardless of length — a
// short "ok" from a degraded account must be withheld exactly like a long
// one. minOutput only bounds how early a mid-stream burst is withheld before
// the stream finishes. A sample with no output at all keeps waiting (also
// after hold expiry): an empty hang must not fail-open as HTTP 200.
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
	if sig.OversizedLine {
		// 无法可靠解析的流不猜测质量：与缓冲上限一致 fail-open。
		return QualityDeliver
	}
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

func shouldHoldQualityStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime) bool {
	if !cfg.Enabled || !input.Streaming || ownership != nil || input.skipQualityHold {
		return false
	}
	switch operation {
	case audit.OperationChat, audit.OperationResponses, audit.OperationMessages, "":
	default:
		return false
	}
	// TUI compaction is a normal /v1/responses body (no compaction_trigger).
	// Keep this defensive body check in addition to skipQualityHold so a caller
	// that bypasses CreateResponse cannot withhold a 100s+ summary as missing-thinking.
	if isResponsesCompactionRequest(input.Body) {
		return false
	}
	if route.Provider != accountdomain.ProviderBuild && route.Provider != accountdomain.ProviderConsole {
		return false
	}
	// Retrying a tool-capable request can repeat external side effects. Tool
	// requests remain outside this feature until they have their own replay
	// safety contract.
	if qualityRequestUsesTools(input.Body) {
		return false
	}
	// Aliases are rewritten before this gate, so inspect the effective request
	// body instead of only the reasoning-capable base model. In particular,
	// grok-4.3-none becomes grok-4.3 plus an explicit disabled setting.
	if qualityRequestDisablesReasoning(input.Body) {
		return false
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) {
		return true
	}
	return modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel)
}

func qualityRequestUsesTools(body []byte) bool {
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	return nonEmptyJSONCollection(payload["tools"]) || nonEmptyJSONCollection(payload["functions"])
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

func nonEmptyJSONCollection(raw json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "[]" || trimmed == "{}" {
		return false
	}
	return strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "{")
}

func jsonStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

// qualityHeaderBudget returns the active response-header budget for this
// attempt: only while the quality hold is engaged, configured, and the
// per-request single-fire arm is still on.
func qualityHeaderBudget(cfg QualityRetryRuntime, holdEnabled, armed bool) time.Duration {
	if !holdEnabled || !armed || cfg.EarlyHeaderAbort <= 0 {
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

func (s *Service) recordQualityDegraded(ctx context.Context, base audit.Record, credential accountdomain.Credential, usage Usage, startedAt time.Time, trace *infraegress.Trace, provider accountdomain.Provider) {
	record := base
	record.EventID = newAuditEventID()
	accountID := credential.ID
	record.AccountID = &accountID
	record.AccountName = credential.Name
	record.StatusCode = http.StatusOK
	record.ErrorCode = ErrorQualityDegraded
	record.OutputTokens = usage.OutputTokens
	record.ReasoningTokens = usage.ReasoningTokens
	record.TotalTokens = usage.TotalTokens
	record.InputTokens = usage.InputTokens
	if usage.Reported {
		record.UsageSource = audit.UsageSourceUpstream
	}
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, trace, provider)
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(writeCtx, record); err != nil {
		s.logger.Error("quality_degraded_audit_failed", "event_id", record.EventID, "request_id", record.RequestID, "error", err)
	}
}
