package egress

import (
	"context"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Immutable routing fixtures cannot commit observations.
func (egressRepositoryTestStub) ApplyEgressHealthObservation(context.Context, domain.HealthObservation) (domain.HealthState, error) {
	return domain.HealthState{}, repository.ErrNotFound
}
func (egressRepositoryTestStub) ApplyEgressClearance(context.Context, domain.ClearanceUpdate) error {
	return repository.ErrNotFound
}
func (egressRepositoryTestStub) RecordEgressClearanceError(context.Context, domain.Node) error {
	return repository.ErrNotFound
}

func applyFixtureHealth(n *domain.Node, o domain.HealthObservation) (domain.HealthState, error) {
	if n.ID != o.NodeID || n.EncryptedProxyURL != o.EncryptedProxyURL || n.BindingRevision != o.BindingRevision {
		return domain.HealthState{}, repository.ErrConflict
	}
	state, accepted := n.HealthState().Apply(o)
	if !accepted {
		return state, repository.ErrConflict
	}
	*n = state.ApplyTo(*n)
	return state, nil
}
func applyFixtureClearance(n *domain.Node, v domain.ClearanceUpdate) error {
	if n.ID != v.NodeID || n.EncryptedProxyURL != v.EncryptedProxyURL || n.BindingRevision != v.BindingRevision || n.ClearanceRevision != v.ExpectedRevision {
		return repository.ErrConflict
	}
	n.ClearanceRevision++
	n.EncryptedCloudflareCookie = v.EncryptedCookie
	n.UserAgent = v.UserAgent
	n.ClearanceFingerprint = v.Fingerprint
	n.ClearanceBindingFingerprint = v.BindingFingerprint
	n.ClearanceRefreshedAt = &v.RefreshedAt
	if n.LastError == "clearance refresh failed" {
		n.LastError = ""
	}
	return nil
}
func applyFixtureClearanceError(n *domain.Node, old domain.Node) error {
	if n.ID != old.ID || n.EncryptedProxyURL != old.EncryptedProxyURL || n.BindingRevision != old.BindingRevision || n.ClearanceRevision != old.ClearanceRevision || (n.LastError != "" && n.LastError != "clearance refresh failed") {
		return repository.ErrConflict
	}
	n.LastError = "clearance refresh failed"
	return nil
}
func (r *mutableEgressRepository) ApplyEgressHealthObservation(_ context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	state, err := applyFixtureHealth(&r.node, o)
	if err == nil {
		r.updates++
	}
	return state, err
}
func (r *mutableEgressRepository) ApplyEgressClearance(_ context.Context, v domain.ClearanceUpdate) error {
	err := applyFixtureClearance(&r.node, v)
	if err == nil {
		r.updates++
	}
	return err
}
func (r *mutableEgressRepository) RecordEgressClearanceError(_ context.Context, n domain.Node) error {
	err := applyFixtureClearanceError(&r.node, n)
	if err == nil {
		r.updates++
	}
	return err
}
func (r *synchronizedEgressRepository) ApplyEgressHealthObservation(_ context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := applyFixtureHealth(&r.node, o)
	return state, err
}
func (r *synchronizedEgressRepository) ApplyEgressClearance(_ context.Context, v domain.ClearanceUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := applyFixtureClearance(&r.node, v)
	return err
}
func (r *synchronizedEgressRepository) RecordEgressClearanceError(_ context.Context, n domain.Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := applyFixtureClearanceError(&r.node, n)
	return err
}
func (r *blockingEgressRepository) ApplyEgressHealthObservation(_ context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, err := applyFixtureHealth(&r.node, o)
	return state, err
}
func (r *blockingEgressRepository) ApplyEgressClearance(_ context.Context, v domain.ClearanceUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := applyFixtureClearance(&r.node, v)
	return err
}
func (r *blockingEgressRepository) RecordEgressClearanceError(_ context.Context, n domain.Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := applyFixtureClearanceError(&r.node, n)
	return err
}
func (r *evictionScopeRepo) ApplyEgressHealthObservation(_ context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	state, err := applyFixtureHealth(&r.node, o)
	return state, err
}
func (r *evictionScopeRepo) ApplyEgressClearance(_ context.Context, v domain.ClearanceUpdate) error {
	err := applyFixtureClearance(&r.node, v)
	return err
}
func (r *evictionScopeRepo) RecordEgressClearanceError(_ context.Context, n domain.Node) error {
	err := applyFixtureClearanceError(&r.node, n)
	return err
}
func (r *e2eRepo) ApplyEgressHealthObservation(_ context.Context, o domain.HealthObservation) (domain.HealthState, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.egressRepositoryTestStub.nodes {
		n := &r.egressRepositoryTestStub.nodes[i]
		if n.ID == o.NodeID {
			state, err := applyFixtureHealth(n, o)
			if err == nil {
				r.health.Add(1)
			}
			return state, err
		}
	}
	return domain.HealthState{}, repository.ErrNotFound
}
func (r *e2eRepo) ApplyEgressClearance(_ context.Context, v domain.ClearanceUpdate) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.egressRepositoryTestStub.nodes {
		n := &r.egressRepositoryTestStub.nodes[i]
		if n.ID == v.NodeID {
			return applyFixtureClearance(n, v)
		}
	}
	return repository.ErrNotFound
}
func (r *e2eRepo) RecordEgressClearanceError(_ context.Context, v domain.Node) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.egressRepositoryTestStub.nodes {
		n := &r.egressRepositoryTestStub.nodes[i]
		if n.ID == v.ID {
			return applyFixtureClearanceError(n, v)
		}
	}
	return repository.ErrNotFound
}
