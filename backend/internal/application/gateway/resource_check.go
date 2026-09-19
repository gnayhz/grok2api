package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type ResourceCheckPathResolver interface {
	ProbeBuildTarget(context.Context, account.Credential, uint64) (string, int, uint64, error)
}

// ResourceCheckMeasurer composes gateway-owned measurements with the network
// owner's destination-facing path observation. It owns no goroutines or state.
type ResourceCheckMeasurer struct {
	Gateway *Service
	Paths   ResourceCheckPathResolver
}

func (m ResourceCheckMeasurer) MeasureResourceCheck(ctx context.Context, accountID, nodeID uint64) model.AccountCheckSample {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	spec, _ := model.ProbeExperimentFromContext(ctx)
	sample := model.AccountCheckSample{Sample: spec.Sample, Outcome: model.MeasurementError, Failure: model.ProbeFailurePath}
	if m.Gateway == nil || m.Paths == nil || nodeID == 0 {
		return sample
	}
	view, err := m.Gateway.accounts.Get(ctx, accountID)
	if err != nil {
		sample.Failure = model.ProbeFailureAccount
		return sample
	}
	if view.Credential.BuildRouteMode == account.BuildRouteXAI {
		sample.Failure = model.ProbeFailureConfiguration
		return sample
	}
	ctx = physical.WithQualityVerificationNode(ctx, nodeID)
	request, release, failure := m.Gateway.prepareQualityProbe(ctx, accountID)
	// Waiting for an account/global measurement slot does not repeat an
	// accepted generation. Parallel batch items may share a control identity.
	for failure.Failure == model.ProbeFailureCapacity && ctx.Err() == nil {
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		if ctx.Err() != nil {
			break
		}
		request, release, failure = m.Gateway.prepareQualityProbe(ctx, accountID)
	}
	if failure.Outcome != "" {
		sample.Outcome, sample.Failure = failure.Outcome, failure.Failure
		return sample
	}
	defer release()
	generation := request.Credential.CredentialGeneration
	sample.PathChecks = 1
	before, family, binding, err := m.Paths.ProbeBuildTarget(ctx, request.Credential, nodeID)
	if err != nil || before == "" {
		return sample
	}
	request.PromptCacheKey = "resource-check/" + m.Gateway.newAuditEventID()
	result := m.Gateway.qualityProbeMeasurement(ctx, request, QualityRetryRuntime{CreatedTimeout: 10 * time.Second, EvidenceTimeout: 15 * time.Second})
	if result.CheckEvidence != nil {
		sample = *result.CheckEvidence
	}
	sample.Generated = result.Attempt.ID != ""
	sample.CredentialGeneration = generation
	sample.PathChecks = 2
	sample.Sample, sample.Attempt, sample.Outcome, sample.Failure = spec.Sample, result.Attempt, result.Outcome, result.Failure
	sample.PathKey, sample.PathFamily, sample.PathBinding = before, family, binding
	after, afterFamily, afterBinding, err := m.Paths.ProbeBuildTarget(ctx, request.Credential, nodeID)
	sample.PathVerified = err == nil && before == after && family == afterFamily && binding == afterBinding && sample.Attempt.Path.NodeID == nodeID && !sample.Attempt.Path.Rotating
	if !sample.PathVerified {
		sample.Outcome, sample.Failure = model.MeasurementError, model.ProbeFailurePath
		// A verified change breaks the window assumption; a failed trace is
		// merely an unavailable measurement and supplies no negative evidence.
		sample.Conflict = sample.Conflict || err == nil && after != "" && (before != after || family != afterFamily || binding != afterBinding)
	}
	current, identityErr := m.Gateway.accounts.Get(ctx, accountID)
	sample.IdentityVerified = identityErr == nil && current.Credential.CredentialGeneration == generation && current.Credential.Provider == request.Credential.Provider && current.Credential.BuildRouteMode == request.Credential.BuildRouteMode
	if !sample.IdentityVerified {
		sample.Outcome, sample.Failure = model.MeasurementError, model.ProbeFailureIdentity
		sample.Conflict = sample.Conflict || identityErr == nil
	}
	return sample
}
