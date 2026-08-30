package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// EgressCanaryRuntime is the canary verifier configuration. ModelPublicID
// selects the model route used for the tiny verification request; an empty
// value disables the canary (rotations re-admit tentatively).
type EgressCanaryRuntime struct {
	ModelPublicID  string
	CreatedTimeout time.Duration
}

func (c EgressCanaryRuntime) normalized() EgressCanaryRuntime {
	if c.CreatedTimeout <= 0 {
		c.CreatedTimeout = 10 * time.Second
	}
	return c
}

// UpdateEgressCanary installs or updates the canary verifier configuration.
func (s *Service) UpdateEgressCanary(cfg EgressCanaryRuntime) {
	cfg = cfg.normalized()
	s.egressCanary.Store(cfg)
}

func (s *Service) egressCanaryConfig() EgressCanaryRuntime {
	if value, ok := s.egressCanary.Load().(EgressCanaryRuntime); ok {
		return value.normalized()
	}
	return EgressCanaryRuntime{}
}

// ProbeEgressQuality verifies one egress node's exit IP with a minimal real
// streaming inference request pinned to that node. It implements
// egress.EgressQualityProber. The decision reuses the real-time guard's
// classifier: first SSE event must arrive inside CreatedTimeout and thinking
// evidence must appear — exactly the signals that separate clean exits
// (sub-3s first event) from degraded-model routing (minute-scale queueing).
func (s *Service) ProbeEgressQuality(ctx context.Context, nodeID uint64) (result egressapp.EgressQualityProbeResult) {
	// canary 结论进守卫统计:clean/degraded 的比例直接反映换 IP 验证质量,
	// 也是排查"canary 分类缺口"的量化依据。
	defer func() { guardStats.recordCanary(string(result.Outcome)) }()
	cfg := s.egressCanaryConfig()
	if cfg.ModelPublicID == "" {
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeUnconfigured, Reason: "canary model not configured"}
	}
	route, err := s.models.GetByPublicID(ctx, cfg.ModelPublicID)
	if err != nil {
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeError, Reason: "resolve route: " + err.Error()}
	}
	// canary 判定依赖"可见思考增量"证据, 与主路径 shouldHoldQualityStream 同一
	// 门控:非推理模型永远判 Degraded, 会白烧换 IP 尝试并让节点扣满隔离周期——
	// 按文档"随手复制一条模型 publicId"的运维极易踩中。不满足时按未配置处理
	// (换 IP 成功后暂定放行, 被动守卫兜底)。
	if !modeldomain.SupportsReasoningForProvider(route.Provider, cfg.ModelPublicID) &&
		!modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel) {
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeUnconfigured, Reason: "canary model does not support reasoning"}
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeError, Reason: "provider adapter unavailable"}
	}
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	lease, err := s.selector.Acquire(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", nil, false)
	if err != nil {
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeNoAccount, Reason: "acquire account: " + err.Error()}
	}
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		lease.Release()
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeNoAccount, Reason: "ensure credential: " + err.Error()}
	}
	body, err := json.Marshal(map[string]any{
		"model":             cfg.ModelPublicID,
		"input":             "Reply with the single digit 1 and nothing else.",
		"stream":            true,
		"max_output_tokens": 16,
	})
	if err != nil {
		lease.Release()
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeError, Reason: err.Error()}
	}
	// Pin the egress to the node under verification, bypassing routing exactly
	// like a degraded retry in the main request loop.
	callCtx := infraegress.WithQualityVerificationNode(ctx, nodeID)
	request := provider.ResponseResourceRequest{
		Credential: credential, Billing: lease.Billing, Method: "POST", Model: route.UpstreamModel,
		// Path 必须显式指向 /responses:适配器按 urlWithBase(base, Path) 拼 URL,
		// 空 Path 会 POST 到 base 根路径——cli-chat-proxy 对 /v1/ 恒 404, canary
		// 因此永远判 degraded, 换 IP 成功也被烧满尝试耗尽隔离(
		// 线上双节点 rotation exhausted 实测即此因)。
		Path: "/responses", Body: body, Streaming: true, NormalizeBody: true, Operation: "responses",
	}
	response, err := adapter.ForwardResponse(callCtx, request)
	if err != nil {
		lease.completeSelectorObservation(err == nil)
		lease.Release()
		if errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeError, Reason: "canceled: " + err.Error()}
		}
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeDegraded, Reason: "forward: " + err.Error()}
	}
	if response == nil || response.Body == nil {
		lease.completeSelectorObservation(false)
		lease.Release()
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeError, Reason: "empty response"}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		lease.completeSelectorObservation(false)
		lease.Release()
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeDegraded, Reason: fmt.Sprintf("upstream HTTP %d", response.StatusCode)}
	}
	// 验证路径只消费两个判决预算（CreatedTimeout/EvidenceTimeout）：canary
	// 直接按 verdict 分类，不走重试/耗尽策略，其余 Runtime 字段不适用。
	hold := QualityRetryRuntime{
		CreatedTimeout: cfg.CreatedTimeout, EvidenceTimeout: cfg.CreatedTimeout + 5*time.Second,
	}
	replay, verdict, _, peekErr := peekQualityStream(ctx, response.Body, qualityProtocolResponses, hold)
	if replay != nil {
		replay.Close()
	} else {
		response.Body.Close()
	}
	lease.completeSelectorObservation(true)
	lease.Release()
	switch {
	case peekErr != nil:
		if errors.Is(peekErr, errQualityCreatedTimeout) || errors.Is(peekErr, errQualityEvidenceTimeout) || errors.Is(peekErr, errQualityEmptyStream) {
			return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeDegraded, Reason: peekErr.Error()}
		}
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeError, Reason: peekErr.Error()}
	case verdict == QualityWithhold:
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeDegraded, Reason: "withheld: no thinking evidence"}
	default:
		return egressapp.EgressQualityProbeResult{Outcome: egressapp.EgressQualityProbeClean}
	}
}
