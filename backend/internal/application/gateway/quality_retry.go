// Package gateway 是请求路由与实时质量守卫的应用层：账号选择、协议转换前的
// 原始响应扫描（quality_retry_scan.go）、流/整包扣留（quality_stream.go、
// quality_body.go）、预算化重试（service.go）与观测（guard_stats.go）。
// 判定规则由 QualityHoldKernel 注入，质量层 guard.Judge 是生产规则源。
package gateway

import (
	"context"
	"encoding/json"
	"errors"
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

	QualityExemptDisabled         = "disabled"
	QualityExemptSkipInput        = "skip_input"
	QualityExemptOperation        = "operation"
	QualityExemptCompaction       = "compaction"
	QualityExemptProvider         = "provider"
	QualityExemptModelScope       = "model_out_of_scope"
	QualityExemptModelNoReasoning = "model_no_reasoning"
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

// QualityRetryConfig 返回当前生效的守卫运行时配置;唯一来源是
// requestGuardSnapshot(组合根注入的快照源)。
func (s *Service) QualityRetryConfig() QualityRetryRuntime {
	cfg, _ := s.requestGuardSnapshot()
	cfg.GuardedModels = append([]string(nil), cfg.GuardedModels...)
	return cfg
}

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

// qualityHoldRule 把判决归因到稳定标签。semanticOnly 标记终态纯语义输出
// 形态（裸工具调用，零思考零正文）：该判决来自 emptyStreamVerdict 而非
// 准入内核，标签直接命名该路径——借道 Judge 会落成 terminal，误读成
// "终态兜底触发"。
func qualityHoldRule(sig QualityStreamSignals, semanticOnly bool, err error) string {
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
	if semanticOnly && !sig.HasThinking && !sig.ReasoningEndedWithoutThinking && sig.VisibleTokens == 0 && sig.OutputTokens == 0 {
		return "semantic"
	}
	// Ordinary evidence explanations come from the same admission rule that
	// decides the verdict. Transport/resource failures above keep their source.
	_, rule := qualityguard.Judge(qualityHoldSignals(sig))
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

// qualityHoldExemptReason 判定守卫豁免原因。QualityVerdict 是单次上游流
// 的 hold 决策,commitQualityHold 是唯一的重试决策点(admission.CommitHold):
// 调用方只提供真实可用性(是否还有路由候选、是否可安全重放),账号切换
// 预算由 admission.DecideRetry 收口,预算耗尽只剩 Reject(fail-closed,G12)。
// 循环入口的预算闸门复用 admission.BudgetExhausted,与 DecideRetry 同一条规则。
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
	// 上游名任一命中即受管辖;清单为空=全部豁免——守卫关闭的物理表达)。
	// requestGuardSnapshot 恒返回非 nil 管辖:nil 只出现在直接构造 Service
	// 的嵌入/单测路径,此时沿用文件白名单语义(空名单=全部模型受管辖),
	// 匹配规则委托权威 guardpolicy.Jurisdiction——与注入路径同一实现。
	scope := jurisdiction
	if scope == nil && len(cfg.GuardedModels) > 0 {
		scope = guardpolicy.Config{GuardedModels: cfg.GuardedModels}
	}
	if scope != nil {
		provider := string(route.Provider)
		if !scope.Jurisdiction(provider, input.PublicModel) && !scope.Jurisdiction(provider, route.UpstreamModel) {
			return QualityExemptModelScope
		}
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

// reasoningExpectedForEffort 报告该次请求是否期望思考：effort=none 是
// 唯一合法零思考形态；空档/其余档位一律期望（语料：未指定强度的推理
// 模型对每个回答都会思考，零思考仅出现在降智时刻）。
func reasoningExpectedForEffort(effort string) bool {
	return !strings.EqualFold(strings.TrimSpace(effort), modeldomain.ReasoningEffortNone)
}

// Enabled tools may produce long legitimate silence. The request-wide tool
// admission deadline and the Provider stream-idle policy still bound the wait.
const qualitySearchSilenceBudget = 24 * time.Hour

// Heavy reasoning allows more upstream queueing time in each liveness phase.
const qualityHeavyReasoningCreatedBudget = 30 * time.Second

// qualityLivenessSchedule derives first-data/evidence budgets from the client
// body as a pre-normalization fallback. Tool requests use the tool admission
// budget; high/xhigh use 30s per phase; other requests retain the configured
// values. Neither phase extends total admission or changes the evidence
// classifier.
func qualityLivenessSchedule(body []byte, operation string, cfg QualityRetryRuntime) QualityRetryRuntime {
	profile := inferencedomain.ReplayPolicyFromRequest(body)
	return qualityLivenessScheduleForProfile(profile.Tools, profile.ReasoningEffort, cfg)
}

// qualityLivenessScheduleForProfile applies the same budgets from an explicit
// tool/effort profile. The normalized adapter profile is authoritative and
// replaces the client-body fallback before network I/O: Chat
// web_search_options, Messages thinking/output_config and effort aliases such
// as max only become tools or high/xhigh after provider normalization.
func qualityLivenessScheduleForProfile(tools bool, effort string, cfg QualityRetryRuntime) QualityRetryRuntime {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if tools {
		cfg.AdmissionTimeout = cfg.ToolAdmissionTimeout
		cfg.EvidenceTimeout = qualitySearchSilenceBudget
		cfg.CreatedTimeout = qualitySearchSilenceBudget
	} else if effort == "high" || effort == "xhigh" {
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
