package egress

import (
	"context"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"

	"strings"
	"sync"
	"time"
)

// clearanceRuntime exclusively owns solver configuration, versioned session
// state and shared solves. Routing and transports are explicit collaborators;
// their locks and cache contents do not belong to the solver component.
type clearanceRuntime struct {
	clearanceSequence            uint64
	invalidateBindings           func()
	clearanceMu                  sync.Mutex
	clearanceLoads               sharedLoadGroup
	backgroundClearanceRefreshes map[string]struct{}
	clearanceConfig              ClearanceConfig
	clearanceVersion             uint64
	clearances                   map[string]clearanceState
	lastClearanceCleanup         time.Time
	solver                       clearanceSolver
	clearanceLock                repository.DistributedLock
	repository                   repository.EgressRuntimeRepository
	cipher                       security.Cryptor
	tasks                        *taskRuntime
	transport                    *clientRegistry
	invalidateNodes              func()
	listNodes                    func(context.Context, time.Time) ([]domain.Node, error)
	getRuntimeNode               func(context.Context, uint64) (domain.Node, error)
	sharedLoad                   func(context.Context, *sharedLoadGroup, string, func() (any, error)) (any, error)
}

func newClearanceRuntime(m *Manager) *clearanceRuntime {
	return &clearanceRuntime{
		repository: m.repository, cipher: m.cipher, tasks: m.tasks, transport: m.transport,
		invalidateNodes: m.invalidateNodes, invalidateBindings: m.invalidateBindings, listNodes: m.listNodes, getRuntimeNode: m.getRuntimeNode, sharedLoad: m.sharedLoad,
		clearances: make(map[string]clearanceState), solver: flaresolverrSolver{manageTransport: m.ManageHTTPTransport},
		clearanceConfig: ClearanceConfig{Mode: "manual", TargetURL: "https://grok.com", Timeout: time.Minute, RefreshInterval: 10 * time.Minute},
	}
}
func (m *clearanceRuntime) SetClearanceLock(value repository.DistributedLock) {
	m.clearanceMu.Lock()
	m.clearanceLock = value
	m.clearanceMu.Unlock()
}

func (m *clearanceRuntime) UpdateClearanceConfig(value ClearanceConfig) {
	value.Mode = strings.TrimSpace(value.Mode)
	value.FlareSolverrURL = strings.TrimSpace(value.FlareSolverrURL)
	value.TargetURL = strings.TrimRight(strings.TrimSpace(value.TargetURL), "/")
	m.clearanceMu.Lock()
	previous := m.clearanceConfig
	m.clearanceConfig = value
	configurationChanged := previous.Mode != value.Mode || previous.FlareSolverrURL != value.FlareSolverrURL || previous.TargetURL != value.TargetURL
	if configurationChanged {
		m.clearanceVersion++
		m.transport.invalidateGeneration()
	}
	m.clearanceMu.Unlock()
}

func (m *Manager) RefreshClearance(ctx context.Context, id uint64) error {
	return m.clearance.RefreshClearance(ctx, id)
}
func (m *Manager) InvalidateClearance(id uint64) { m.clearance.InvalidateClearance(id) }
func (m *Manager) ForgetClearance(id uint64)     { m.clearance.ForgetClearance(id) }
func (m *Manager) ForgetClearances(ids []uint64) { m.clearance.ForgetClearances(ids) }
func (m *Manager) RefreshDueClearances(ctx context.Context, force bool) error {
	return m.clearance.RefreshDueClearances(ctx, force)
}

// observeRejection is reserved for legacy FeedbackForScope callers which have
// no lease identity. Versioned provider observations never enter this path.
func (m *clearanceRuntime) observeRejection(nodeID uint64, scope domain.Scope) {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	if !isGrokWebScope(scope) || m.clearanceConfig.Mode != "flaresolverr" {
		return
	}
	if nodeID == 0 {
		state := m.clearances["direct"]
		state.invalid, state.used = true, true
		m.clearances["direct"] = state
	} else {
		m.invalidateNodeClearancesLocked(nodeID)
	}
}
