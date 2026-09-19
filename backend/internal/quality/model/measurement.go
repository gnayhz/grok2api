package model

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// ProbeMeasurement is one physical sample. The investigation owns its
// qualification; the gateway supplies protocol, path and failure facts.
type ProbeMeasurement struct {
	Attempt          attemptmeta.Identity
	Outcome          MeasurementOutcome
	Reason           string
	Failure          ProbeFailure
	Detail           string
	VerifiedIPChange bool
	PathKey          string
	CheckEvidence    *ResourceSample
}

type MeasurementOutcome string

const (
	MeasurementClean        MeasurementOutcome = "clean"
	MeasurementDegraded     MeasurementOutcome = "degraded"
	MeasurementError        MeasurementOutcome = "error"
	MeasurementUnconfigured MeasurementOutcome = "unconfigured"
)

func (r ProbeMeasurement) FailureCode() ProbeFailure {
	if r.Failure != "" {
		return r.Failure
	}
	return ProbeFailureUnknown
}

// TaskResult persists stable vocabulary, never a raw provider error.
func (r ProbeMeasurement) TaskResult() ProbeTaskResult {
	outcome, failure, detail := ProbeResultError, r.Failure, r.Detail
	switch r.Outcome {
	case MeasurementClean:
		outcome = ProbeResultClean
	case MeasurementDegraded:
		outcome = ProbeResultDegraded
	}
	if outcome == ProbeResultError {
		failure = r.FailureCode()
		detail = "cause=" + string(failure)
	}
	return ProbeTaskResult{Outcome: outcome, FailureKind: string(failure), Detail: detail,
		Attempt: r.Attempt, VerifiedIPChange: r.VerifiedIPChange, PathKey: r.PathKey}
}

// ProbeIdentityMatches is shared by execution and adjudication. Only an
// observed, registered identity can support a measurement's attribution.
func ProbeIdentityMatches(actual attemptmeta.Identity, accountID, nodeID, epoch uint64) bool {
	return actual.ID != "" && actual.RuleVersion != "" && actual.Model != "" && actual.Provider != "" &&
		actual.AccountID == accountID && actual.Path.NodeID == nodeID && actual.Path.Epoch == epoch && !actual.Path.Rotating &&
		actual.Path.Status == attemptmeta.PathRegistered
}

type ProbeIdentity struct {
	ID      uint64
	Grouped bool
}
type probeIdentityKey struct{}

// WithProbeIdentity labels the serialization scope; it grants no capacity or
// eligibility. The gateway still acquires its ordinary bounded resources.
func WithProbeIdentity(ctx context.Context, id uint64, grouped bool) context.Context {
	return context.WithValue(ctx, probeIdentityKey{}, ProbeIdentity{ID: id, Grouped: grouped})
}

func ProbeIdentityFromContext(ctx context.Context) (ProbeIdentity, bool) {
	id, ok := ctx.Value(probeIdentityKey{}).(ProbeIdentity)
	return id, ok
}
