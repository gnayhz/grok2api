package web

import (
	"context"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Protocol-only fixtures have no persistent nodes; unexpected writes are errors.
func (egressRepositoryStub) ApplyEgressHealthObservation(context.Context, domain.HealthObservation) (domain.HealthState, error) {
	return domain.HealthState{}, repository.ErrNotFound
}
func (egressRepositoryStub) ApplyEgressClearance(context.Context, domain.ClearanceUpdate) error {
	return repository.ErrNotFound
}
func (egressRepositoryStub) RecordEgressClearanceError(context.Context, domain.Node) error {
	return repository.ErrNotFound
}
func (r *recordingWebEgressRepository) ApplyEgressHealthObservation(_ context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.node.ID != o.NodeID || r.node.EncryptedProxyURL != o.EncryptedProxyURL || r.node.BindingRevision != o.BindingRevision {
		return domain.HealthState{}, repository.ErrConflict
	}
	state, accepted := r.node.HealthState().Apply(o)
	if !accepted {
		return state, repository.ErrConflict
	}
	r.node = state.ApplyTo(r.node)
	r.updates++
	return state, nil
}
func (r *recordingWebEgressRepository) ApplyEgressClearance(context.Context, domain.ClearanceUpdate) error {
	return repository.ErrNotFound
}
func (r *recordingWebEgressRepository) RecordEgressClearanceError(context.Context, domain.Node) error {
	return repository.ErrNotFound
}
