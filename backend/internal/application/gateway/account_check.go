package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// PrepareAccountCheck freezes the requested Build model and current rule without
// inventing a traffic incident. It performs no inference and changes no holds.
func (s *Service) PrepareAccountCheck(ctx context.Context, accountID uint64, publicModel string) (qualitymodel.ProbeExperiment, error) {
	spec := qualitymodel.ProbeExperiment{Version: qualitymodel.AccountCheckVersion, Sample: "brief-confirmation"}
	view, err := s.accounts.Get(ctx, accountID)
	if err != nil {
		return spec, err
	}
	if view.Credential.Provider != account.ProviderBuild {
		return spec, errors.New("quality checks require a Build account")
	}
	return s.prepareCheckModel(ctx, accountID, publicModel)
}

func (s *Service) PrepareResourceCheck(ctx context.Context, kind string, id uint64, publicModel string) (qualitymodel.ProbeExperiment, error) {
	var spec qualitymodel.ProbeExperiment
	var err error
	if kind == "account" {
		spec, err = s.PrepareAccountCheck(ctx, id, publicModel)
	} else {
		spec, err = s.prepareCheckModel(ctx, 0, publicModel)
	}
	spec.Version, spec.Sample = qualitymodel.ResourceCheckVersion, "token-short"
	return spec, err
}

func (s *Service) prepareCheckModel(ctx context.Context, accountID uint64, publicModel string) (qualitymodel.ProbeExperiment, error) {
	spec := qualitymodel.ProbeExperiment{Version: qualitymodel.AccountCheckVersion, Sample: "brief-confirmation"}
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		return spec, err
	}
	for _, route := range routes {
		if !route.Enabled || route.Provider != account.ProviderBuild || !modeldomain.SupportsReasoningForProvider(route.Provider, route.UpstreamModel) {
			continue
		}
		hold, _ := s.requestGuardSnapshot()
		if err := hold.Unavailable(); err != nil {
			return spec, err
		}
		spec.Baseline = attemptmeta.Identity{AccountID: accountID, Provider: string(route.Provider), Model: route.UpstreamModel, Revision: hold.Revision, RuleVersion: hold.RuleVersion}
		return spec, nil
	}
	return spec, ErrModelNotFound
}

// Manual diagnostics consume the bounded completed stream so usage can be
// compared independently of early admission. They reuse the production event
// interpretation, but a timeout or incomplete response never becomes a finding.
func readAccountCheckStream(ctx context.Context, body io.ReadCloser, hold QualityRetryRuntime, resources *selector.AttemptResources) (qualitymodel.AccountCheckSample, error) {
	stop := context.AfterFunc(ctx, resources.Close)
	defer stop()
	state := qualityScanState{kernel: hold.Kernel(), protocol: qualityProtocolResponses, startedAt: time.Now()}
	firstVerdict := QualityWait
	stream := responseflow.FromReader(body)
	if stream == nil {
		return qualitymodel.AccountCheckSample{}, errors.New("canonical check stream unavailable")
	}
	err := stream.ConsumeLimit(qualityProbeCompletionBytes, func(event *responseflow.Event) error {
		if event.HasData && !bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
			observeQualityPayload(&state, event.Data)
			if firstVerdict == QualityWait {
				firstVerdict, _ = state.streamVerdict(true)
			}
		}
		return state.protocolErr
	})
	state.terminal = true
	verdict, verdictErr := state.streamVerdict(true)
	if err == nil {
		err = verdictErr
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err == nil && (!state.completed || state.failed) {
		err = errors.New("successful completion not observed")
	}
	fp := state.fingerprint(verdict, err)
	sample := qualitymodel.AccountCheckSample{Outcome: qualitymodel.MeasurementError, Rule: fp.Rule,
		Thinking: fp.HasThinking, Completed: fp.Completed && !fp.Failed, UsageReported: state.usage.Reported,
		InputTokens: state.usage.InputTokens, ReasoningTokens: state.usage.ReasoningTokens}
	if state.usage.CachedInputTokensReported {
		cached := state.usage.CachedInputTokens
		sample.CachedTokens = &cached
	}
	if err == nil {
		switch {
		case firstVerdict == QualityWithhold && sample.Thinking:
			// Late thinking cannot erase the earlier production rejection.
			// Retain both facts as conflicting evidence for manual review.
			sample.Failure = qualitymodel.ProbeFailureAdmission
		case verdict == QualityDeliver && sample.Thinking:
			sample.Outcome = qualitymodel.MeasurementClean
		case verdict == QualityWithhold:
			sample.Outcome = qualitymodel.MeasurementDegraded
		default:
			sample.Failure = qualitymodel.ProbeFailureAdmission
		}
	}
	return sample, err
}

// MeasureAccountCheck uses the same pinned-account resources and production
// scanner as court probes. Quality custody is bypassed only by that existing
// verification channel; credentials, quota and concurrency remain authoritative.
func (s *Service) MeasureAccountCheck(ctx context.Context, accountID uint64) qualitymodel.AccountCheckSample {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	spec, _ := qualitymodel.ProbeExperimentFromContext(ctx)
	sample := qualitymodel.AccountCheckSample{Sample: spec.Sample}
	request, release, result := s.prepareQualityProbe(ctx, accountID)
	if result.Outcome == "" {
		defer release()
		// Each measurement has its own upstream session and cannot reuse a
		// previous check's generated conversation state.
		request.PromptCacheKey = "quality-check/" + s.newAuditEventID()
		result = s.qualityProbeMeasurement(ctx, request, QualityRetryRuntime{CreatedTimeout: 10 * time.Second, EvidenceTimeout: 15 * time.Second})
	}
	if result.CheckEvidence != nil {
		sample = *result.CheckEvidence
	}
	sample.Attempt, sample.Outcome, sample.Failure = result.Attempt, result.Outcome, result.Failure
	return sample
}
