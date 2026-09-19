package model

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// ProbeMeasurement is one physical sample. The investigation owns its
// qualification; the gateway supplies protocol, path and failure facts.
type ProbeMeasurement struct {
	Attempt       attemptmeta.Identity
	Outcome       MeasurementOutcome
	Reason        string
	Failure       ProbeFailure
	CheckEvidence *ResourceSample
}

type MeasurementOutcome string

const (
	MeasurementClean        MeasurementOutcome = "clean"
	MeasurementDegraded     MeasurementOutcome = "degraded"
	MeasurementError        MeasurementOutcome = "error"
	MeasurementUnconfigured MeasurementOutcome = "unconfigured"
)

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
