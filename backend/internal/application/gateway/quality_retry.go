// Package gateway 是请求路由与实时质量守卫的应用层：账号选择、协议转换前的
// 原始响应扫描（quality_retry_scan.go）、流/整包扣留（quality_stream.go、
// quality_body.go）、预算化重试（service.go）与观测（guard_stats.go）。
// 判定规则由 QualityHoldKernel 注入，质量层 guard.Judge 是生产规则源。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"strings"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	qualityguard "github.com/chenyme/grok2api/backend/internal/quality/guard"
)

const (
	ErrorQualityDegraded   = "quality_degraded"
	qualityRetryFailClosed = "fail_closed"
	// Compatibility aliases for standalone embedded gateways; values remain
	// owned by the admission domain. Production consumes a complete snapshot.
	defaultQualityMaxAttempts      = guardpolicy.DefaultMaxAttempts
	defaultQualityEvidenceTimeout  = guardpolicy.DefaultEvidenceTimeout
	defaultQualityCreatedTimeout   = guardpolicy.DefaultCreatedTimeout
	defaultMissingThinkingCooldown = guardpolicy.DefaultAccountCooldown
	qualityIdleAccountCooldown     = guardpolicy.DefaultIdleAccountCooldown
)

var (
	errQualityDegraded    = errors.New("上游响应缺少推理")
	errQualityEmptyStream = errors.New("上游流式响应为空")
	// Resource/protocol failures are inconclusive, never evidence of degradation.
	errQualityHoldLimit = errors.New("上游响应超出质量守卫缓冲上限")
	errQualityBodyShape = errors.New("上游响应不是可识别的推理响应")
	errQualityChoices   = errors.New("质量守卫仅支持单个响应选项")
	// errQualityEvidenceTimeout 标记首个 data 事件之后超过预算仍无
	// 思考证据且无可见输出。按空闲路径处理（短冷却+重试），
	// 不计入 missing-thinking 惩罚，也不作为指纹熔断。
	errQualityEvidenceTimeout = errors.New("上游流式响应零证据超时")
	// errQualityCreatedTimeout 标记首事件截止：预算内没有 SSE data
	// 事件到达，keepalive 注释不算。按空闲路径处理，不作为降智证据。
	errQualityCreatedTimeout = errors.New("上游流式响应首事件超时")
)

// QualityRetryRuntime is the isolated request-path withhold/retry policy.
// Zero Enabled leaves production behavior unchanged.
type QualityRetryRuntime struct {
	Revision             uint64
	RuleVersion          string
	AdmissionTimeout     time.Duration
	ToolAdmissionTimeout time.Duration
	kernel               QualityHoldKernel
	pathResolver         attemptmeta.PathResolver
	unavailable          error

	Enabled     bool
	MaxAttempts int
	OnExhausted string
	// AccountCooldown 是 missing-thinking 定罪的账号冷却。
	AccountCooldown time.Duration
	// IdleAccountCooldown 是空流/静默超时的账号冷却，独立于 missing-thinking
	// 的 AccountCooldown（二者诱因与置信度不同：空流常与出口 IP 相关）。
	// 0 = 默认 15m。
	IdleAccountCooldown time.Duration
	// EvidenceTimeout 从首个 SSE data 事件起计时（0=默认 3.5s），
	// 超时仍无思考证据、可见或语义输出时中止尝试；后续元数据不续期。
	EvidenceTimeout time.Duration
	// CreatedTimeout 从流式准入开始等待首个 SSE data 事件（0=默认 5s）。
	// 首事件到达后由 EvidenceTimeout 接管；两个阶段都受总准入期限限制。
	CreatedTimeout time.Duration
	// ReasoningExpected 是从请求侧解析出的思考期望（resolved effort !=
	// none；空档视为期望——语料：未指定强度的推理模型对每个回答都会
	// 思考）。终态"纯语义输出"（仅工具调用、零思考零文本）在期望思考
	// 时按 missing-thinking 扣留：降智账号的裸工具调用此前经语义放行
	// 出口整包交付且守卫零计数。零值 false 保持旧语义（语义放行），
	// 供 effort=none 请求与探针等无请求语义的调用方使用；业务路径由
	// service 按 resolved effort 显式赋值。
	ReasoningExpected bool
	// GuardedModels 是守卫介入的模型白名单（requestRetry.guardedModels）：
	// 非空时仅名单内模型进入判决，其余模型整体豁免（台账 model_out_of_scope）。
	// 空 = 全部推理模型介入（向后兼容默认）。守卫的价值集中在主力推理模型
	//（grok-4.5/4.6）；对边缘/退役模型介入只会产出噪声拦截与误罚
	//（实证：grok-4.3 四连 quality_degraded 503，而它不在运营
	// 关切内）。条目匹配 public/upstream 模型名，"grok-4.6" 前缀覆盖
	// "grok-4.6-xhigh" 等档位后缀别名。
	GuardedModels []string
}

// QualityStreamSignals is the hold classifier input. Tests drive this
// directly and via observeQualityChunk on SSE fixtures.
type QualityStreamSignals struct {
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

// qualityHoldFingerprint 是守卫判决的紧凑存档：生产 quality_hold/idle
// attempt 此前只写 stage+耗时、正文与响应头全空，无法回放「为何扣留」。
// 真实 SSE 归档证明判别力在事件序列与时序（D-a: created→item.done 0ms；
// 干净流: created→summary.delta 0-6ms），不在请求头/请求体键。本结构只
// 记录类型与毫秒，不含增量文本或密文。
type qualityHoldFingerprint struct {
	Completed       bool     `json:"completed,omitempty"`
	Failed          bool     `json:"failed,omitempty"`
	Protocol        string   `json:"protocol"`
	Verdict         string   `json:"verdict,omitempty"`
	Rule            string   `json:"rule,omitempty"`
	HasThinking     bool     `json:"has_thinking"`
	ReasoningEnded  bool     `json:"reasoning_ended_without_thinking"`
	SemanticOutput  bool     `json:"semantic_output,omitempty"`
	SawDataEvent    bool     `json:"saw_data_event"`
	Encrypted       bool     `json:"encrypted_content"`
	VisibleRunes    int      `json:"visible_runes,omitempty"`
	OutputTokens    int64    `json:"output_tokens,omitempty"`
	ReasoningTokens int64    `json:"reasoning_tokens,omitempty"`
	Events          []string `json:"events,omitempty"`
	// FirstItem 是第一个 output item / content_block 的 type。
	// 真实归档里 D-a 为 reasoning，真抢跑为 message；二者 rule 都可能
	// 落成 outrun（tee 在 item.done 之后还读到正文），单靠 rule 分不开。
	FirstItem    string `json:"first_item,omitempty"`
	FirstEventMS int64  `json:"first_event_ms,omitempty"`
	ItemDoneMS   int64  `json:"item_done_ms,omitempty"`
	SummaryMS    int64  `json:"summary_ms,omitempty"`
	PeekMS       int64  `json:"peek_ms,omitempty"`
	Error        string `json:"error,omitempty"`
}

func qualityHoldRule(sig QualityStreamSignals, err error) string {
	switch {
	case errors.Is(err, responsebuffer.ErrExhausted):
		return "resource_exhausted"
	case errors.Is(err, errQualityChoices):
		return "unsupported_choices"
	case errors.Is(err, errQualityHoldLimit):
		return "buffer_limit"
	case errors.Is(err, errQualityBodyShape):
		return "unrecognized"
	case errors.Is(err, errQualityCreatedTimeout):
		return "created_timeout"
	case errors.Is(err, errQualityEvidenceTimeout):
		return "evidence_timeout"
	case errors.Is(err, errQualityEmptyStream):
		return "empty"
	}
	// Ordinary evidence explanations come from the same admission rule that
	// decides the verdict. Transport/resource failures above keep their source.
	_, rule := qualityguard.Judge(qualityguard.Signals{
		HasThinking: sig.HasThinking, ReasoningEndedWithoutThinking: sig.ReasoningEndedWithoutThinking,
		VisibleTokens: sig.VisibleTokens, OutputTokens: sig.OutputTokens, Terminal: sig.Terminal,
	})
	return string(rule)
}

func (fp qualityHoldFingerprint) json() []byte {
	if fp.Protocol == "" && fp.Verdict == "" && fp.Rule == "" && len(fp.Events) == 0 {
		return nil
	}
	raw, err := json.Marshal(fp)
	if err != nil {
		return nil
	}
	return raw
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
	QualityActionDeliver QualityRetryAction = "deliver"
	QualityActionRetry   QualityRetryAction = "retry"
	QualityActionReject  QualityRetryAction = "reject"
)

func normalizeQualityRetry(cfg QualityRetryRuntime) QualityRetryRuntime {
	if cfg.AdmissionTimeout <= 0 {
		cfg.AdmissionTimeout = guardpolicy.DefaultAdmissionTimeout
	}
	if cfg.ToolAdmissionTimeout <= 0 {
		cfg.ToolAdmissionTimeout = guardpolicy.DefaultToolAdmissionTimeout
	}
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
	// The effective configuration must describe the enforced policy, including
	// when an older settings row still contains fail_open.
	cfg.OnExhausted = qualityRetryFailClosed
	return cfg
}

func (s *Service) UpdateQualityRetry(cfg QualityRetryRuntime) {
	normalized := normalizeQualityRetry(cfg)
	normalized.GuardedModels = append([]string(nil), cfg.GuardedModels...)
	s.qualityRetry.Store(&normalized)
	// 生效配置投影进 guard-stats:面板状态卡与指标面据此回答
	// "守卫现在是否在场"。
	guardStats.setEffective(normalized)
}

func (s *Service) qualityRetryConfig() QualityRetryRuntime {
	if s == nil {
		return normalizeQualityRetry(QualityRetryRuntime{})
	}
	if value := s.qualityRetry.Load(); value != nil {
		return *value // Private immutable snapshot; copy only at the public boundary.
	}
	return normalizeQualityRetry(QualityRetryRuntime{})
}

// QualityRetryConfig 返回当前请求路径实际使用的守卫运行时配置。
// 组合根用它把管理面质量守卫配置投影到网关,避免 API 已更新而请求路径
// 仍读取旧快照。返回值包含独立的模型切片,调用方不能修改原子快照。
func (s *Service) QualityRetryConfig() QualityRetryRuntime {
	cfg, _ := s.requestGuardSnapshot()
	cfg.GuardedModels = append([]string(nil), cfg.GuardedModels...)
	return cfg
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
// original + one rotated account). The last withhold (attemptIndex ==
// maxAttempts-1) is fail-closed: 扣留预算耗尽即 Reject(503),绝无
// DeliverLast——"降智不进上下文"铁律(G12,fail-open 已废除)。
func decideQualityRetry(verdict QualityVerdict, attemptIndex, maxAttempts int) QualityRetryAction {
	if verdict == QualityDeliver {
		return QualityActionDeliver
	}
	if verdict != QualityWithhold {
		return QualityActionReject
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
	// attemptIndex == maxAttempts-1 (or past it): exhausted ⇒ reject.
	return QualityActionReject
}

// boundQualityRetry turns a Retry into Reject when the routing loop has no
// remaining account slot, so the loop never spins into exhausted iterations.
func boundQualityRetry(action QualityRetryAction, hasNextRoutingAttempt bool) QualityRetryAction {
	if action != QualityActionRetry || hasNextRoutingAttempt {
		return action
	}
	return QualityActionReject
}

// QualityCommit is the single attempt-loop decision for a held stream.
type QualityCommit struct {
	Action   QualityRetryAction
	Audit    bool
	KeepBody bool
}

// decideQualityCommit is the retry-primitive seam (D3-3b): a quality-layer
// policy decides the retry action when installed; nil keeps the built-in
// commitQualityHold. Either way the base still bounds the action by the
// routing budget (boundQualityRetry). 耗尽动作只剩 Reject(fail-closed,
// G12):策略层若仍返回 DeliverLast 一律按 Reject 收口。
func (s *Service) decideQualityCommit(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool, onExhausted string) QualityCommit {
	commit := commitQualityHold(verdict, qualityAttempt, maxAttempts, hasNextRouting)
	// A policy may stop recovery early, but cannot expand either budget or
	// release a response the classifier has withheld. Unknown/legacy actions
	// fail closed as well.
	if commit.Action == QualityActionRetry {
		if policy := s.qualityPolicyObserver(); policy != nil &&
			policy.DecideQualityRetry(verdict, qualityAttempt, maxAttempts, onExhausted) != QualityActionRetry {
			commit.Action = QualityActionReject
		}
	}
	return commit
}

// commitQualityHold is the shipped withhold/retry/commit unit. The attempt
// loop must not re-derive this from Decide+Bound+switch.
func commitQualityHold(verdict QualityVerdict, qualityAttempt, maxAttempts int, hasNextRouting bool) QualityCommit {
	action := boundQualityRetry(
		decideQualityRetry(verdict, qualityAttempt, maxAttempts),
		hasNextRouting,
	)
	switch action {
	case QualityActionRetry, QualityActionReject:
		return QualityCommit{Action: action, Audit: true, KeepBody: false}
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
	QualityExemptCompaction       = "compaction"            // 请求体命中 compaction 标记（CreateResponse TUI 记 skip_input）
	QualityExemptProvider         = "provider"              // 非 Build/Console 供应商
	QualityExemptMessagesNoThink  = "messages_thinking_off" // 仅保留统计 API 的历史 token；请求不再走此豁免。
	QualityExemptModelScope       = "model_out_of_scope"    // 模型不在守卫白名单（guardedModels）
	QualityExemptModelNoReasoning = "model_no_reasoning"    // 目标模型不支持推理
)

// QualityJurisdiction 是管辖判定缝隙(G13):质量层守卫配置的模型勾选
// 清单成为请求路径的权威——面板改勾选即改扣留范围。条目支持渠道
// 限定("grok_build:grok-4.5" 只拦该渠道;裸名=任意渠道)。
// nil=沿用文件基线(requestRetry.guardedModels)。
type QualityJurisdiction interface {
	Jurisdiction(provider, model string) bool
}

// qualityHoldExemptReason 返回守卫不介入该请求的原因；空串表示应介入。
// shouldHoldQualityStream 是它的布尔投影（reason==""）。
func qualityHoldExemptReason(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime, jurisdiction QualityJurisdiction) string {
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
	// 模型范围(G13 管辖勾选):质量层守卫配置注入时以它为权威(公开名/
	// 上游名任一命中即受管辖;清单为空=全部豁免——守卫关闭的物理表达);
	// 未注入时沿用文件白名单(requestRetry.guardedModels,空名单=全部介入)。
	if jurisdiction != nil {
		provider := string(route.Provider)
		if !jurisdiction.Jurisdiction(provider, input.PublicModel) && !jurisdiction.Jurisdiction(provider, route.UpstreamModel) {
			return QualityExemptModelScope
		}
	} else if !qualityModelGuarded(cfg, input.PublicModel, route.UpstreamModel) {
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
	// Both adapters defer successful JSON conversion, so Messages requests
	// retain native reasoning evidence even when clients hide thinking blocks.
	if modeldomain.SupportsReasoningForProvider(route.Provider, input.PublicModel) {
		return ""
	}
	if modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel) {
		return ""
	}
	return QualityExemptModelNoReasoning
}

func shouldHoldQualityStream(input Input, ownership *inferencedomain.ResponseOwnership, route modeldomain.Route, operation audit.Operation, cfg QualityRetryRuntime, jurisdiction QualityJurisdiction) bool {
	return qualityHoldExemptReason(input, ownership, route, operation, cfg, jurisdiction) == ""
}

// qualityModelGuarded 报告模型是否在守卫白名单内（cfg.GuardedModels 为空
// = 全部模型）。条目匹配 public 或 upstream 模型名；"grok-4.6" 前缀覆盖
// "grok-4.6-xhigh" 等档位后缀别名。
// reasoningExpectedForEffort 报告该次请求是否期望思考：effort=none 是
// 唯一合法零思考形态；空档/其余档位一律期望（语料：未指定强度的推理
// 模型对每个回答都会思考，零思考仅出现在降智时刻）。
func reasoningExpectedForEffort(effort string) bool {
	return !strings.EqualFold(strings.TrimSpace(effort), modeldomain.ReasoningEffortNone)
}

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

// Enabled tools may produce long legitimate silence. The request-wide tool
// admission deadline and the Provider stream-idle policy still bound the wait.
const qualitySearchSilenceBudget = 24 * time.Hour

// Heavy reasoning allows more upstream queueing time in each liveness phase.
const qualityHeavyReasoningCreatedBudget = 30 * time.Second

// qualityLivenessSchedule adjusts sequential first-data/evidence budgets using
// the normalized request profile. Tool requests use the tool admission budget;
// high/xhigh use 30s per phase; other requests retain the configured values.
// Neither phase extends total admission or changes the evidence classifier.
func qualityLivenessSchedule(body []byte, operation string, cfg QualityRetryRuntime) QualityRetryRuntime {
	profile := inferencedomain.ReplayPolicyFromRequest(body)
	search, effort := profile.Tools, profile.ReasoningEffort
	heavy := effort == "high" || effort == "xhigh"
	if search {
		cfg.AdmissionTimeout = cfg.ToolAdmissionTimeout
		cfg.EvidenceTimeout = qualitySearchSilenceBudget
		cfg.CreatedTimeout = qualitySearchSilenceBudget
	} else if heavy {
		cfg.EvidenceTimeout = qualityHeavyReasoningCreatedBudget
		cfg.CreatedTimeout = qualityHeavyReasoningCreatedBudget
	}
	return cfg
}

// Upstream counters are metadata, never quality evidence. Malformed negative
// values must not overflow estimates or escape into attempt accounting.
func boundedQualityUsage(usage Usage) Usage {
	usage.InputTokens = max(0, usage.InputTokens)
	usage.OutputTokens = max(0, usage.OutputTokens)
	usage.ReasoningTokens = max(0, usage.ReasoningTokens)
	usage.TotalTokens = max(0, usage.TotalTokens)
	usage.CostInUSDTicks = max(0, usage.CostInUSDTicks)
	return usage
}
