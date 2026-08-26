package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
)

// BuildThinkingProbeResult 是网关侧 Build 原生差分探针的结论。Outcome 语义:
// clean(任一路径出现思考流) / degraded(两路都降智) / error(传输故障或无法
// 差分) / unconfigured(没有可用推理 Build 模型,功能未启用)。装配层把它
// 适配到 risk.BuildProbeResult。
type BuildThinkingProbeResult struct {
	Outcome string
	Reason  string
	Details string
}

const (
	buildProbeClean        = "clean"
	buildProbeDegraded     = "degraded"
	buildProbeError        = "error"
	buildProbeUnconfigured = "unconfigured"
)

const buildProbePrompt = "Reply with the single digit 1 and nothing else."

// ProbeBuildThinking runs the Build-native risk probe for one unlinked
// Build account: a tiny real streaming reasoning request through the
// account credential, classified by the same guard signals as production
// traffic (thinking evidence + first-event budget). IP pollution is the
// confound a pure Build signal cannot escape, so a degraded first attempt
// triggers a differential second attempt over a different exit:
//   - pool node (rotating endpoint): same node re-roll = new exit IP;
//   - fixed node: the first node is excluded, routing picks another path;
//   - direct-only: no differential exists — the probe stays inconclusive
//     (error) and never produces a denied-shaped false positive.
//
// The risk service additionally gates double-degraded verdicts behind a
// recent clean witness.
func (s *Service) ProbeBuildThinking(ctx context.Context, accountID, degradedNodeID uint64) BuildThinkingProbeResult {
	probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	route, reason := s.buildProbeRoute(probeCtx)
	if route.Provider == "" {
		return BuildThinkingProbeResult{Outcome: buildProbeUnconfigured, Reason: reason}
	}
	view, err := s.accounts.Get(probeCtx, accountID)
	if err != nil {
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: "load account: " + err.Error()}
	}
	credential, err := s.accounts.EnsureCredential(probeCtx, view.Credential, false)
	if err != nil {
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: "ensure credential: " + err.Error()}
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: "provider adapter unavailable"}
	}
	body, err := json.Marshal(map[string]any{
		"model":             route.PublicID,
		"input":             buildProbePrompt,
		"stream":            true,
		"max_output_tokens": 16,
	})
	if err != nil {
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: err.Error()}
	}
	request := provider.ResponseResourceRequest{
		Credential: credential, Billing: view.Billing, Method: "POST", Model: route.UpstreamModel,
		// Path 必须显式指向 /responses(与 canary 同因:空 Path 会打到 base 根,
		// cli-chat-proxy 对 /v1/ 恒 404,探针会永远误判降智)。
		Path: "/responses", Body: body, Streaming: true, NormalizeBody: true, Operation: "responses",
	}
	hold := QualityRetryRuntime{
		Enabled: true, MaxAttempts: 1, HoldTimeout: 2 * time.Second,
		CreatedTimeout: 10 * time.Second, EvidenceTimeout: 15 * time.Second,
		MinOutputTokens: 1, OnExhausted: qualityRetryFailClosed,
	}
	// 尝试一:钉扎降智节点复现该路径(canary 同款 verification 钉扎,绕过
	// 降智事件留下的节点软冷却——否则探针会被自己触发的冷却挤到别的
	// 路径,差分基准失真)。降智发生在直连时按直连复现。
	firstCtx := probeCtx
	if degradedNodeID != 0 {
		firstCtx = infraegress.WithQualityVerificationNode(probeCtx, degradedNodeID)
	}
	traceCtx, trace := infraegress.WithTrace(firstCtx)
	first, firstReason := s.buildProbeAttempt(traceCtx, adapter, request, hold)
	if first == buildProbeClean {
		return BuildThinkingProbeResult{Outcome: buildProbeClean, Details: "attempt1=thinking"}
	}
	if first == buildProbeError {
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: firstReason, Details: "attempt1=error"}
	}
	// 尝试一降智 → 差分尝试二。差分基准取探针 attempt1 的实际出口
	// (trace),无 trace 时退回降智节点。
	differential := ""
	secondCtx := probeCtx
	selection, traced := trace.Selection(domainegress.ScopeBuild)
	baseNode := degradedNodeID
	if traced {
		baseNode = selection.NodeID
	}
	if baseNode == 0 {
		// 降智与探针都走直连(默认路由也是直连):单路无法差分。绝不据此定罪。
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: "single-path (direct) probe cannot differentiate IP vs account", Details: "attempt1=degraded path=direct"}
	}
	if traced && selection.Pool {
		// 旋转池:同节点重摇即新出口 IP(池节点豁免跨请求排除,重摇可行)。
		differential = fmt.Sprintf("reroll pool node %d", baseNode)
	} else {
		// 固定节点:排除后换路。
		secondCtx = infraegress.WithNodeExclusions(probeCtx, map[uint64]struct{}{baseNode: {}})
		differential = fmt.Sprintf("exclude fixed node %d", baseNode)
	}
	second, secondReason := s.buildProbeAttempt(secondCtx, adapter, request, hold)
	switch second {
	case buildProbeClean:
		return BuildThinkingProbeResult{Outcome: buildProbeClean, Details: "attempt1=degraded attempt2=thinking " + differential}
	case buildProbeError:
		return BuildThinkingProbeResult{Outcome: buildProbeError, Reason: secondReason, Details: "attempt1=degraded attempt2=error " + differential}
	default:
		return BuildThinkingProbeResult{Outcome: buildProbeDegraded, Reason: "no thinking evidence on both paths", Details: "attempt1=degraded attempt2=degraded " + differential}
	}
}

// buildProbeAttempt sends one tiny streaming request and classifies it with
// the production guard signals: thinking evidence delivers, created/evidence
// budget misses and withheld streams are degraded, anything else is error.
func (s *Service) buildProbeAttempt(ctx context.Context, adapter provider.ResponseAdapter, request provider.ResponseResourceRequest, hold QualityRetryRuntime) (outcome, reason string) {
	response, err := adapter.ForwardResponse(ctx, request)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return buildProbeError, "canceled: " + err.Error()
		}
		// 传输失败对降智判定无信息量(网络抖动/代理故障都会发生),保持 error。
		return buildProbeError, "forward: " + err.Error()
	}
	if response == nil || response.Body == nil {
		return buildProbeError, "empty response"
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		return buildProbeError, fmt.Sprintf("upstream HTTP %d", response.StatusCode)
	}
	replay, verdict, _, _, peekErr := peekQualityStream(ctx, response.Body, qualityProtocolResponses, hold)
	if replay != nil {
		replay.Close()
	} else {
		response.Body.Close()
	}
	switch {
	case peekErr != nil:
		if errors.Is(peekErr, errQualityCreatedTimeout) || errors.Is(peekErr, errQualityEvidenceTimeout) || errors.Is(peekErr, errQualityEmptyStream) {
			return buildProbeDegraded, peekErr.Error()
		}
		return buildProbeError, peekErr.Error()
	case verdict == QualityWithhold:
		return buildProbeDegraded, "withheld: no thinking evidence"
	default:
		return buildProbeClean, ""
	}
}

// buildProbeRoute resolves the reasoning Build model used by the probe: the
// canary model when it is a Build reasoning route, otherwise the first
// enabled Build reasoning route. Empty provider means unconfigured.
func (s *Service) buildProbeRoute(ctx context.Context) (route modeldomain.Route, reason string) {
	if cfg := s.egressCanaryConfig(); cfg.ModelPublicID != "" {
		if value, err := s.models.GetByPublicID(ctx, cfg.ModelPublicID); err == nil &&
			value.Provider == "grok_build" &&
			(modeldomain.SupportsReasoningForProvider(value.Provider, value.PublicID) || modeldomain.SupportsReasoningForProvider(value.Provider, value.UpstreamModel)) {
			return value, ""
		}
	}
	// 路由枚举是可选能力(routeResolver 的测试伪实现不携带):不支持时仅
	// 依赖 canary 模型配置。
	lister, ok := s.models.(interface {
		List(ctx context.Context, page, pageSize int, search string, filter modelapp.ListFilter) ([]modeldomain.Route, int64, error)
	})
	if !ok {
		return modeldomain.Route{}, "route enumeration unavailable; configure egressRotation.canaryModelPublicId to a build reasoning model"
	}
	routes, _, err := lister.List(ctx, 1, 50, "", modelapp.ListFilter{Provider: "grok_build", Status: "enabled"})
	if err != nil {
		return modeldomain.Route{}, "list build routes: " + err.Error()
	}
	for _, value := range routes {
		if modeldomain.SupportsReasoningForProvider(value.Provider, value.PublicID) || modeldomain.SupportsReasoningForProvider(value.Provider, value.UpstreamModel) {
			return value, ""
		}
	}
	return modeldomain.Route{}, "no enabled reasoning build model (configure egressRotation.canaryModelPublicId or add one)"
}
