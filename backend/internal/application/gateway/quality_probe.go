package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

const qualityProbeCompletionBytes = 1 << 20

func (s *Service) qualityProbeMeasurement(ctx context.Context, request provider.ResponseResourceRequest, limits QualityRetryRuntime) (result qualitymodel.ProbeMeasurement) {
	fail := func(kind qualitymodel.ProbeFailure, reason string) qualitymodel.ProbeMeasurement {
		result.Outcome, result.Failure, result.Reason = qualitymodel.MeasurementError, kind, reason
		return result
	}
	// Each physical measurement freezes the same policy authority as traffic.
	// Probes use shorter transport deadlines, without losing policy identity.
	hold, _ := s.requestGuardSnapshot()
	if hold.Unavailable() != nil {
		return fail(qualitymodel.ProbeFailurePolicy, "guard policy unavailable")
	}
	spec, frozen := qualitymodel.ProbeExperimentFromContext(ctx)
	if !frozen || (spec.UnsupportedReason() != "" || hold.RuleVersion != spec.Baseline.RuleVersion || hold.Revision != spec.Baseline.Revision) {
		return fail(qualitymodel.ProbeFailureExperiment, "experiment policy changed or unavailable")
	}
	if limits.CreatedTimeout > 0 {
		hold.CreatedTimeout = limits.CreatedTimeout
	}
	if limits.EvidenceTimeout > 0 {
		hold.EvidenceTimeout = limits.EvidenceTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, selector.QualityMeasurementTimeout)
	defer cancel()
	ctx = responsebuffer.WithContext(ctx, responsebuffer.NewLimitedRequest(8<<20))
	ctx, resources := selector.NewAttemptResources(ctx)
	defer resources.Close()
	ctx = attemptmeta.WithRequest(ctx, s.newAuditEventID(), hold.Revision, hold.RuleVersion, hold.PathResolver())
	ctx = attemptmeta.WithAccount(ctx, request.Credential.ID, string(request.Credential.Provider), request.Model)
	ctx = attemptmeta.WithProfile(ctx, spec.Profile())
	ctx = s.startPhysicalTrace(ctx, string(request.Credential.Provider), "responses")
	requestBudget := inferencedomain.NewAttemptBudget(1)
	defer requestBudget.Close()
	ctx = portphysical.WithPhysicalCallBudget(ctx, requestBudget)
	defer func() {
		resources.Close()
		if result.Attempt.ID == "" {
			facts := portphysical.PhysicalFacts(ctx)
			if len(facts) > 0 {
				result.Attempt = facts[len(facts)-1].Attempt
			}
		}
		if err := s.recordPhysicalEvents(ctx); err != nil {
			result = fail(qualitymodel.ProbeFailurePersistence, "physical evidence persistence failed")
		}
	}()
	// One probe task means one physical generation. Its output never belongs
	// in a user conversation cache, even when the probe finishes successfully.
	request.DisableAutomaticReplay = true
	request.DeferOutputCommit = true
	response, err := s.runPhysicalAttempt(ctx, request, resources)
	if response != nil && response.DiscardOutput != nil {
		defer response.DiscardOutput()
	}
	if response != nil {
		result.Attempt = response.Attempt
		response.Body = resources.Own(response.Body)
	}
	if err != nil {
		return fail(probeOperationFailure(ctx, err, qualitymodel.ProbeFailureForward), "forward: "+err.Error())
	}
	if response == nil || response.Body == nil {
		return fail(qualitymodel.ProbeFailureResponse, "empty response")
	}
	if frozen && !spec.Matches(result.Attempt) {
		return fail(qualitymodel.ProbeFailureExperiment, "normalized experiment mismatch")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		kind := qualitymodel.ProbeFailureHTTPRejected
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			kind = qualitymodel.ProbeFailureHTTPServer
		}
		return fail(kind, fmt.Sprintf("upstream HTTP %d", response.StatusCode))
	}
	if responseflow.FromReader(response.Body) == nil {
		response.Body = resources.Own(responseflow.New(response.Body, responsebuffer.FromContext(ctx)))
	}
	sample, err := readResourceCheckStream(ctx, response.Body, hold, resources)
	sample.Sample = spec.Sample
	result.CheckEvidence = &sample
	if err != nil {
		return fail(probeOperationFailure(ctx, err, qualitymodel.ProbeFailureCompletion), "resource check completion unavailable")
	}
	result.Outcome, result.Failure = sample.Outcome, sample.Failure
	return result
}

// qualityProbeOnBuildFace I23:质量取证探针只走 Build 面——Web 与
// Build 是不同风控面,Web 账号的 SSO 风控结论不得由 Build 探针跨面
// 定罪(反之亦然)。
func qualityProbeOnBuildFace(provider account.Provider) bool {
	return provider == account.ProviderBuild
}

// qualityProbeRoute resolves only the frozen Build model; it cannot substitute
// the first available route or an experiment with a missing specification.
func (s *Service) qualityProbeRoute(ctx context.Context) (route modeldomain.Route, err error) {
	lister, ok := s.models.(interface {
		List(ctx context.Context, page, pageSize int, search string, filter modelapp.ListFilter) ([]modeldomain.Route, int64, error)
	})
	if !ok {
		return modeldomain.Route{}, errors.New("route enumeration unavailable")
	}
	spec, frozen := qualitymodel.ProbeExperimentFromContext(ctx)
	if !frozen {
		return modeldomain.Route{}, errors.New("resource proof specification missing")
	}
	if spec.UnsupportedReason() != "" {
		return modeldomain.Route{}, errors.New(spec.UnsupportedReason())
	}
	const pageSize = 2000
	for page := 1; ; page++ {
		routes, total, err := lister.List(ctx, page, pageSize, "", modelapp.ListFilter{Provider: "grok_build", Status: "enabled"})
		if err != nil {
			return modeldomain.Route{}, fmt.Errorf("list build routes: %w", err)
		}
		for _, value := range routes {
			if string(value.Provider) != spec.Baseline.Provider {
				continue
			}
			if value.UpstreamModel != spec.Baseline.Model {
				continue
			}
			if modeldomain.SupportsReasoningForProvider(value.Provider, value.PublicID) || modeldomain.SupportsReasoningForProvider(value.Provider, value.UpstreamModel) {
				return value, nil
			}
		}
		if len(routes) == 0 || total <= int64(page*pageSize) || len(routes) < pageSize {
			break
		}
	}
	return modeldomain.Route{}, errors.New("no enabled reasoning build model")
}

// qualityProbeRequestForContext uses the saved synthetic measurement contract,
// without replaying the trigger's content or enabling its tools.
func (s *Service) qualityProbeRequestForContext(ctx context.Context, route modeldomain.Route, credential account.Credential, billing *account.Billing) (provider.ResponseResourceRequest, error) {
	spec, ok := qualitymodel.ProbeExperimentFromContext(ctx)
	if !ok {
		return provider.ResponseResourceRequest{}, errors.New("resource proof specification missing")
	}
	if reason := spec.UnsupportedReason(); reason != "" {
		return provider.ResponseResourceRequest{}, errors.New(reason)
	}
	maxOutput := 256
	body := map[string]any{"model": route.PublicID, "input": spec.Prompt(), "stream": true, "max_output_tokens": maxOutput}
	if effort := spec.Profile().ReasoningEffort; effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return provider.ResponseResourceRequest{}, err
	}
	return provider.ResponseResourceRequest{Credential: credential, Billing: billing, Method: "POST", Model: route.UpstreamModel,
		Path: "/responses", Body: raw, Streaming: true, NormalizeBody: true, Operation: "responses"}, nil
}
