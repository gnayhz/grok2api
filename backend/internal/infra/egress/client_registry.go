package egress

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

var ErrRuntimeClosed = netbudget.ErrClosed

// clientRegistry exclusively owns transport identity, construction, cache
// membership and invalidation. Manager asks for clients; it has no parallel
// client state or cache synchronization policy.
type clientRegistry struct {
	tasks   *taskRuntime
	network *netbudget.Runtime
	owned   sync.Map // requestClient -> *clientHandle

	clientMu           sync.RWMutex
	clients            map[clientCacheKey]cachedClient
	clientLoads        sharedLoadGroup
	clientVersions     map[uint64]uint64
	clientGeneration   uint64
	buildHeaderTimeout atomic.Int64
	accountIsolated    atomic.Bool
	lastClientCleanup  time.Time
	newBuildClient     func(string, time.Duration) (requestClient, error)
	newBuildEnvClient  func(time.Duration) (requestClient, error)
	newBrowserClient   func(string, string) (*browserClient, error)
	log                func() *slog.Logger
	closed             atomic.Bool
}

func (m *clientRegistry) invalidate(nodeIDs map[uint64]struct{}, scope domain.Scope) {
	m.clientMu.Lock()
	for id := range nodeIDs {
		m.invalidateClientVersionLocked(id)
	}
	if scope == domain.ScopeWebAsset {
		scope = domain.ScopeWeb
	}
	var stale []requestClient
	for key, value := range m.clients {
		if _, ok := nodeIDs[key.nodeID]; ok && (scope == "" || key.scope == scope) {
			stale = append(stale, m.evictClientLocked(key, value))
		}
	}
	m.clientMu.Unlock()
	m.closeRequestClients(stale)
}

func (m *clientRegistry) invalidateGeneration() {
	m.clientMu.Lock()
	m.invalidateAllClientVersionsLocked()
	m.clientMu.Unlock()
}

func (m *clientRegistry) close() {
	m.closed.Store(true)
	m.clientMu.Lock()
	clients := make([]requestClient, 0, len(m.clients))
	for key, value := range m.clients {
		clients = append(clients, m.evictClientLocked(key, value))
	}
	m.invalidateAllClientVersionsLocked()
	m.clientMu.Unlock()
	m.closeRequestClients(clients)
	m.owned.Range(func(_, v any) bool { v.(*clientHandle).finalize(); return true })
}

func newClientRegistry(log func() *slog.Logger, network *netbudget.Runtime) *clientRegistry {
	c := &clientRegistry{network: network, clients: make(map[clientCacheKey]cachedClient), clientVersions: make(map[uint64]uint64), log: log}
	c.buildHeaderTimeout.Store(int64(settingsdomain.DefaultBuildResponseHeaderTimeout))
	return c
}
func (m *clientRegistry) UpdateBuildResponseHeaderTimeout(value time.Duration) {
	if value <= 0 {
		value = settingsdomain.DefaultBuildResponseHeaderTimeout
	}
	if previous := time.Duration(m.buildHeaderTimeout.Swap(int64(value))); previous == value {
		return
	}
	m.clientMu.Lock()
	var stale []requestClient
	m.invalidateAllClientVersionsLocked()
	for key, cached := range m.clients {
		if key.scope == domain.ScopeBuild {
			stale = append(stale, m.evictClientLocked(key, cached))
		}
	}
	m.clientMu.Unlock()
	m.closeRequestClients(stale)
}

func (m *clientRegistry) UpdateAccountIsolatedConnections(enabled bool) {
	m.clientMu.Lock()
	if m.accountIsolated.Load() == enabled {
		m.clientMu.Unlock()
		return
	}
	// Change the mode while holding the same lock used to validate client-cache
	// keys. This makes the mode snapshot and cache invalidation one transition.
	m.accountIsolated.Store(enabled)
	stale := make([]requestClient, 0, len(m.clients))
	for key, cached := range m.clients {
		stale = append(stale, m.evictClientLocked(key, cached))
	}
	m.invalidateAllClientVersionsLocked()
	m.clientMu.Unlock()
	m.closeRequestClients(stale)
	m.log().Info("egress_account_connection_isolation_updated", "enabled", enabled, "evicted_clients", len(stale))
}

func (m *clientRegistry) invalidateClientLocked(nodeID uint64) []requestClient {
	m.invalidateClientVersionLocked(nodeID)
	var stale []requestClient
	for key, cached := range m.clients {
		if key.nodeID != nodeID {
			continue
		}
		delete(m.clients, key)
		stale = append(stale, cached.client)
	}
	return stale
}

func (m *clientRegistry) invalidateClientForScopeLocked(nodeID uint64, scope domain.Scope) []requestClient {
	m.invalidateClientVersionLocked(nodeID)
	if scope == domain.ScopeWebAsset {
		scope = domain.ScopeWeb
	}
	var stale []requestClient
	for key, cached := range m.clients {
		if key.nodeID != nodeID || key.scope != scope {
			continue
		}
		delete(m.clients, key)
		stale = append(stale, cached.client)
	}
	return stale
}

func (m *clientRegistry) closeIdle() {
	m.clientMu.RLock()
	clients := make([]requestClient, 0, len(m.clients))
	for _, v := range m.clients {
		clients = append(clients, v.client)
	}
	m.clientMu.RUnlock()
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}

func (m *clientRegistry) sweepIdle() {
	m.clientMu.Lock()
	stale := m.cleanupClientCacheLocked(time.Now().UTC())
	m.clientMu.Unlock()
	m.closeRequestClients(stale)
}

// reserveClient first reclaims an idle entry from the same cache class. Active
// and retired handles retain their permit; saturation fails without spawning.
func (m *clientRegistry) reserveClient(session bool) (func(), error) {
	release, err := m.network.TryClient()
	if err == nil || err != netbudget.ErrCapacity {
		return release, err
	}
	m.clientMu.Lock()
	var oldestKey clientCacheKey
	var oldest cachedClient
	found := false
	for key, v := range m.clients {
		if (key.sessionKey != "") != session || v.handle == nil || !v.handle.idle() {
			continue
		}
		if !found || v.lastUsed.Before(oldest.lastUsed) {
			oldestKey, oldest, found = key, v, true
		}
	}
	if found {
		delete(m.clients, oldestKey)
	}
	m.clientMu.Unlock()
	if found {
		m.closeRequestClients([]requestClient{oldest.client})
		return m.network.TryClient()
	}
	return nil, err
}
