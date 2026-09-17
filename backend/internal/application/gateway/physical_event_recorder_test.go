package gateway

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

type physicalEventRecorder struct {
	facts []attemptmeta.PhysicalFact
}

func (*physicalEventRecorder) RecordQualityEvent(context.Context, QualityObservation, time.Duration) error {
	return nil
}
func (r *physicalEventRecorder) RecordPhysicalEvents(_ context.Context, facts []attemptmeta.PhysicalFact) error {
	r.facts = append(r.facts, facts...)
	return nil
}
