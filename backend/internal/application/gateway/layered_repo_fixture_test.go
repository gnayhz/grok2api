package gateway

import (
	"context"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type layeredAccountRepository struct {
	repository.AccountRepository
	mu             sync.Mutex
	baseCalls      int
	overlayCalls   map[string]int
	bases          []account.RoutingAccountBase
	nextBases      []account.RoutingAccountBase
	overlays       map[string]account.RoutingOverlaySnapshot
	routeOverlays  map[uint64]account.RoutingOverlaySnapshot
	firstBaseStart chan struct{}
	firstBaseReady chan struct{}
	baseHook       func()
	baseErr        error
	overlayErr     error
	combined       []account.RoutingCandidate
	combinedCalls  int
	materialErrors map[uint64]error
	materials      map[uint64]account.CredentialMaterial
	materialCalls  []uint64
	healthUpdates  []repository.InvalidationEvent
	lastUsedAt     map[uint64]time.Time
}

func (r *layeredAccountRepository) ListRoutingAccountBases(context.Context, account.Provider, string) ([]account.RoutingAccountBase, error) {
	r.mu.Lock()
	r.baseCalls++
	call := r.baseCalls
	values := r.bases
	if call > 1 && r.nextBases != nil {
		values = r.nextBases
	}
	start, ready := r.firstBaseStart, r.firstBaseReady
	hook := r.baseHook
	loadErr := r.baseErr
	r.mu.Unlock()
	if hook != nil {
		hook()
	}
	if call == 1 && start != nil {
		close(start)
		<-ready
	}
	return values, loadErr
}

func (r *layeredAccountRepository) ListRoutingCandidates(context.Context, account.Provider, uint64, string, string) ([]account.RoutingCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.combinedCalls++
	return r.combined, nil
}

// Current claim facts are independent of cached load counters and injected
// material errors. This fixture models its backing state under the same lock.
func (r *layeredAccountRepository) GetRoutingCandidate(_ context.Context, id uint64, provider account.Provider, routeID uint64, model, mode string) (account.RoutingCandidate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, base := range r.bases {
		if base.Credential.ID == id && base.Credential.Provider == provider {
			return account.RoutingCandidate{Credential: base.Credential}, nil
		}
	}
	return account.RoutingCandidate{}, repository.ErrNotFound
}

func (r *layeredAccountRepository) GetCredentialMaterial(_ context.Context, accountID uint64, provider account.Provider) (account.CredentialMaterial, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.materialCalls = append(r.materialCalls, accountID)
	if err := r.materialErrors[accountID]; err != nil {
		return account.CredentialMaterial{}, err
	}
	if material, ok := r.materials[accountID]; ok {
		return material, nil
	}
	return account.CredentialMaterial{AccountID: accountID, Provider: provider, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: "encrypted"}, nil
}

func (r *layeredAccountRepository) ApplyHealth(_ context.Context, id uint64, provider account.Provider, event account.HealthEvent) (account.HealthResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state := account.HealthState{AccountID: id, Provider: provider}
	for _, base := range r.bases {
		if base.Credential.ID == id {
			state = base.Credential.HealthState()
			break
		}
	}
	for _, update := range r.healthUpdates {
		if update.AccountID == id {
			state.Revision, state.FailureCount, state.CooldownUntil, state.LastError = update.HealthRevision, update.FailureCount, update.CooldownUntil, update.HealthMarker
		}
	}
	result, err := account.TransitionHealth(state, event, time.Now().UTC())
	if err != nil {
		return result, err
	}
	state = result.State
	r.healthUpdates = append(r.healthUpdates, repository.InvalidationEvent{Kind: repository.InvalidationAccountHealthChanged, Provider: provider, AccountID: id,
		HealthRevision: state.Revision, FailureCount: state.FailureCount, CooldownUntil: state.CooldownUntil, HealthMarker: account.NormalizeHealthMarker(state.LastError)})
	return result, nil
}

func (r *layeredAccountRepository) TouchLastUsed(_ context.Context, id uint64, usedAt time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastUsedAt == nil {
		r.lastUsedAt = make(map[uint64]time.Time)
	}
	r.lastUsedAt[id] = usedAt
	return nil
}

func (r *layeredAccountRepository) ListRoutingAccountOverlays(_ context.Context, _ account.Provider, modelRouteID uint64, upstreamModel string) (account.RoutingOverlaySnapshot, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.overlayCalls == nil {
		r.overlayCalls = make(map[string]int)
	}
	r.overlayCalls[upstreamModel]++
	if r.overlayErr != nil {
		return account.RoutingOverlaySnapshot{}, r.overlayErr
	}
	if modelRouteID > 0 && r.routeOverlays != nil {
		return r.routeOverlays[modelRouteID], nil
	}
	return r.overlays[upstreamModel], nil
}

func newLayeredRepositoryFixture() *layeredAccountRepository {
	return &layeredAccountRepository{
		bases: []account.RoutingAccountBase{{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, Enabled: true, AuthStatus: account.AuthStatusActive}}},
		overlays: map[string]account.RoutingOverlaySnapshot{
			"model-a": {Values: []account.RoutingAccountOverlay{{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true}}},
			"model-b": {Values: []account.RoutingAccountOverlay{{AccountID: 1, ModelCapabilityKnown: true, SupportsModel: true}}},
		},
		overlayCalls: make(map[string]int),
	}
}
