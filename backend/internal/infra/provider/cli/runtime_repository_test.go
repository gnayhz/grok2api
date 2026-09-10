package cli

import (
	"context"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Protocol-only fixtures have no persistent nodes; unexpected writes are errors.
func (emptyEgressRepository) ApplyEgressHealthObservation(context.Context, domain.HealthObservation) (domain.HealthState, error) {
	return domain.HealthState{}, repository.ErrNotFound
}
func (emptyEgressRepository) ApplyEgressClearance(context.Context, domain.ClearanceUpdate) error {
	return repository.ErrNotFound
}
func (emptyEgressRepository) RecordEgressClearanceError(context.Context, domain.Node) error {
	return repository.ErrNotFound
}
