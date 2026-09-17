package egress

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func (m *clientRegistry) clientForContext(ctx context.Context, id uint64, scope domain.Scope, proxyURL, userAgent, cookies string, sticky bool, accountIdentity string, options clientOptions) (cachedClient, error) {
	if m.closed.Load() {
		return cachedClient{}, ErrRuntimeClosed
	}
	options, sessionDecision := resolveConnectionOptions(scope, options)
	sessionKey := options.sessionKey
	clientKind := "browser"
	buildHeaderTimeout := time.Duration(0)
	if scope == domain.ScopeBuild {
		clientKind = "build"
		buildHeaderTimeout = time.Duration(m.buildHeaderTimeout.Load())
		if buildHeaderTimeout <= 0 {
			buildHeaderTimeout = settingsdomain.DefaultBuildResponseHeaderTimeout
		}
		clientKind += "\x00" + strconv.FormatInt(int64(buildHeaderTimeout), 10)
		if options.buildEnvironmentProxy {
			clientKind += "\x00environment-proxy"
		}
		if options.freshTunnel {
			clientKind += "\x00fresh-connection"
		}
		if sessionKey != "" {
			// 单连接钉扎形态:传输层并发/空闲旋钮与共享池不同,必须进
			// 指纹,否则同节点同账号下两种形态会互相命中对方的缓存条目。
			clientKind += "\x00session-pinned"
		}
	}
	// cookies 刻意不进指纹:客户端构造不接收 cookies(按请求头携带、无 jar,
	// 见 buildCachedClient),把它纳入键只会让 clearance 例行刷新或账号 cookie
	// 变化把同出口的整池热连接无谓作废(每次 3-4 RTT/条重握手)。指纹仍含
	// proxyURL/userAgent——它们真实决定传输层形态(TLS profile、拨号目标)。
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(clientKind+"\x00"+proxyURL+"\x00"+userAgent)))
	cacheScope := scope
	if cacheScope == domain.ScopeWebAsset {
		cacheScope = domain.ScopeWeb
	}
	for attempt := 0; attempt < clientCreationRetryLimit; attempt++ {
		isolated := m.accountIsolated.Load()
		policy := ConnectionPolicy{AccountIsolated: isolated, Fresh: options.freshTunnel, SessionReuse: sessionDecision}
		if options.requireAccountIsolation && !isolated {
			return cachedClient{}, errAccountConnectionIsolationDisabled
		}
		keyAccountIdentity := ""
		// Session reuse is subordinate to the administrator's account boundary.
		if isolated {
			keyAccountIdentity = strings.TrimSpace(accountIdentity)
			if keyAccountIdentity == "" {
				keyAccountIdentity = "shared"
			}
		}
		key := clientCacheKey{nodeID: id, scope: cacheScope, fingerprint: fingerprint, accountIdentity: keyAccountIdentity, sessionKey: sessionKey}
		loadKey := strconv.FormatUint(key.nodeID, 10) + "\x00" + string(key.scope) + "\x00" + key.fingerprint + "\x00" + key.accountIdentity + "\x00" + key.sessionKey
		now := time.Now().UTC()
		m.clientMu.RLock()
		cached, cachedOK := m.clients[key]
		cleanupDue := m.lastClientCleanup.IsZero() || now.Sub(m.lastClientCleanup) >= clientCacheCleanupInterval
		touchDue := cachedOK && (cached.lastUsed.IsZero() || now.Sub(cached.lastUsed) >= clientCacheTouchInterval)
		m.clientMu.RUnlock()
		if cachedOK && !cleanupDue && !touchDue {
			cached.policy = policy
			return cached, nil
		}

		m.clientMu.Lock()
		stale := m.cleanupClientCacheLocked(now)
		if cached, ok := m.clients[key]; ok {
			cached.lastUsed = now
			m.clients[key] = cached
			m.clientMu.Unlock()
			m.closeRequestClients(stale)
			cached.policy = policy
			return cached, nil
		}
		m.clientMu.Unlock()
		m.closeRequestClients(stale)

		done, waitErr := m.network.BeginWait()
		if waitErr != nil {
			return cachedClient{}, waitErr
		}
		waitCtx, cancelWait := context.WithCancel(ctx)
		stop := context.AfterFunc(m.tasks.ctx, cancelWait)
		loaded, err := waitSharedLoad(waitCtx, &m.clientLoads, loadKey, func() (any, error) {
			taskDone, taskErr := m.tasks.begin("client")
			if taskErr != nil {
				return nil, taskErr
			}
			defer taskDone()
			return m.createAndCacheClient(key, id, scope, proxyURL, userAgent, sticky, buildHeaderTimeout, options)
		})
		stop()
		cancelWait()
		done()
		if errors.Is(err, errClientCacheInvalidated) {
			continue
		}
		if err != nil {
			return cachedClient{}, err
		}
		cached = loaded.(cachedClient)
		cached.policy = policy
		return cached, nil
	}
	return cachedClient{}, errClientCacheInvalidated
}

func (m *clientRegistry) createAndCacheClient(key clientCacheKey, id uint64, scope domain.Scope, proxyURL, userAgent string, sticky bool, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	now := time.Now().UTC()
	m.clientMu.Lock()
	stale := m.cleanupClientCacheLocked(now)
	if (key.accountIdentity != "") != m.accountIsolated.Load() {
		m.clientMu.Unlock()
		m.closeRequestClients(stale)
		return cachedClient{}, errClientCacheInvalidated
	}
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = now
		m.clients[key] = cached
		m.clientMu.Unlock()
		m.closeRequestClients(stale)
		return cached, nil
	}
	version := m.clientVersionLocked(id)
	m.clientMu.Unlock()
	m.closeRequestClients(stale)

	value, err := m.buildCachedClient(scope, proxyURL, userAgent, buildHeaderTimeout, options)
	if err != nil {
		return cachedClient{}, err
	}
	value.lastUsed = time.Now().UTC()

	m.clientMu.Lock()
	stale = m.cleanupClientCacheLocked(value.lastUsed)
	if (key.accountIdentity != "") != m.accountIsolated.Load() {
		m.clientMu.Unlock()
		m.closeRequestClients(append(stale, value.client))
		return cachedClient{}, errClientCacheInvalidated
	}
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = value.lastUsed
		m.clients[key] = cached
		m.clientMu.Unlock()
		m.closeRequestClients(append(stale, value.client))
		return cached, nil
	}
	if m.closed.Load() || m.clientVersionLocked(id) != version {
		m.clientMu.Unlock()
		m.closeRequestClients(append(stale, value.client))
		return cachedClient{}, errClientCacheInvalidated
	}
	// Session and shared clients have separate capacity budgets. Inserting
	// either must not evict the other class; explicit binding/policy updates
	// still invalidate both, and active leases retain their resource owner.
	if id != 0 && !sticky && key.sessionKey == "" {
		for previousKey, previous := range m.clients {
			if previousKey.nodeID != id || previousKey.scope != key.scope {
				continue
			}
			if previousKey.sessionKey != "" {
				continue
			}
			// Keep other accounts' pools when isolation is on.
			if key.accountIdentity != "" && previousKey.accountIdentity != key.accountIdentity {
				continue
			}
			stale = append(stale, m.evictClientLocked(previousKey, previous))
		}
	}
	stale = append(stale, m.ensureClientCacheCapacityLocked(key)...)
	m.clients[key] = value
	m.clientMu.Unlock()
	m.closeRequestClients(stale)
	return value, nil
}

func (m *clientRegistry) buildCachedClient(scope domain.Scope, proxyURL, userAgent string, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	release, err := m.reserveClient(options.sessionKey != "")
	if err != nil {
		return cachedClient{}, err
	}
	value, err := m.constructClient(scope, proxyURL, userAgent, buildHeaderTimeout, options)
	if err != nil {
		release()
		return cachedClient{}, err
	}
	value.handle = &clientHandle{registry: m, client: value.client, releaseBudget: release}
	if actual, loaded := m.owned.LoadOrStore(value.client, value.handle); loaded {
		release()
		value.handle = actual.(*clientHandle)
	}
	return value, nil
}

func (m *clientRegistry) constructClient(scope domain.Scope, proxyURL, userAgent string, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	if scope == domain.ScopeBuild {
		if options.sessionKey != "" || options.freshTunnel {
			client, err := newBuildClientConfigured(proxyURL, buildHeaderTimeout, buildConnectionOptions{environmentProxy: options.buildEnvironmentProxy, sessionPinned: options.sessionKey != "", freshConnection: options.freshTunnel, onDial: options.onSessionDial}, m.network)
			if err != nil {
				return cachedClient{}, err
			}
			return cachedClient{client: client}, nil
		}
		if options.buildEnvironmentProxy {
			factory := m.newBuildEnvClient
			if factory == nil {
				factory = func(timeout time.Duration) (requestClient, error) {
					return newBuildClientConfigured("", timeout, buildConnectionOptions{environmentProxy: true}, m.network)
				}
			}
			client, err := factory(buildHeaderTimeout)
			if err != nil {
				return cachedClient{}, err
			}
			return cachedClient{client: client}, nil
		}
		factory := m.newBuildClient
		if factory == nil {
			factory = func(proxy string, timeout time.Duration) (requestClient, error) {
				return newBuildClientConfigured(proxy, timeout, buildConnectionOptions{}, m.network)
			}
		}
		client, err := factory(proxyURL, buildHeaderTimeout)
		if err != nil {
			return cachedClient{}, err
		}
		return cachedClient{client: client}, nil
	}
	factory := m.newBrowserClient
	if factory == nil {
		factory = func(proxy, ua string) (*browserClient, error) {
			return newBrowserClientWithBudget(proxy, ua, m.network)
		}
	}
	client, err := factory(proxyURL, userAgent)
	if err != nil {
		return cachedClient{}, err
	}
	return cachedClient{client: client, browser: client}, nil
}

func (m *clientRegistry) cleanupClientCacheLocked(now time.Time) []requestClient {
	if m.clients == nil {
		m.clients = make(map[clientCacheKey]cachedClient)
	}
	if !m.lastClientCleanup.IsZero() && now.Sub(m.lastClientCleanup) < clientCacheCleanupInterval {
		return nil
	}
	m.lastClientCleanup = now
	var stale []requestClient
	for key, value := range m.clients {
		if !value.lastUsed.IsZero() && now.Sub(value.lastUsed) >= clientCacheIdleTTL {
			stale = append(stale, m.evictClientLocked(key, value))
		}
	}
	return stale
}

// ensureClientCacheCapacityLocked 按两类条目分账控制容量:共享池(节点/
// 账号级)与会话客户端(每会话一条)。若混用一个总上限,高峰期的会话
// 条目会把共享池按 LRU 挤出,关掉活跃账号池的空闲连接,让账号下所有
// 会话同时冷启动;分开计账后各自只在自己的预算内淘汰最久未用者。
func (m *clientRegistry) ensureClientCacheCapacityLocked(incoming ...clientCacheKey) []requestClient {
	var stale []requestClient
	// Reserve a slot only in the category being inserted. Filling the shared
	// budget must not make an unrelated session insertion evict shared clients.
	if len(incoming) == 0 || incoming[0].sessionKey == "" {
		stale = append(stale, m.evictOldestClientsLocked(func(key clientCacheKey) bool { return key.sessionKey == "" }, maxCachedClients)...)
	}
	if len(incoming) == 0 || incoming[0].sessionKey != "" {
		stale = append(stale, m.evictOldestClientsLocked(func(key clientCacheKey) bool { return key.sessionKey != "" }, maxSessionCachedClients)...)
	}
	return stale
}

func (m *clientRegistry) evictOldestClientsLocked(match func(clientCacheKey) bool, limit int) []requestClient {
	count := 0
	for key := range m.clients {
		if match(key) {
			count++
		}
	}
	var stale []requestClient
	for count >= limit {
		var oldestKey clientCacheKey
		var oldest cachedClient
		found := false
		for key, value := range m.clients {
			if !match(key) {
				continue
			}
			if !found || value.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest, found = key, value, true
			}
		}
		if !found {
			break
		}
		stale = append(stale, m.evictClientLocked(oldestKey, oldest))
		count--
	}
	return stale
}

func (m *clientRegistry) evictClientLocked(key clientCacheKey, value cachedClient) requestClient {
	delete(m.clients, key)
	return value.client
}

func (m *clientRegistry) closeRequestClients(values []requestClient) {
	for _, value := range values {
		if value != nil {
			if handle, ok := m.owned.Load(value); ok {
				handle.(*clientHandle).retire()
			} else {
				value.CloseIdleConnections()
			}
		}
	}
}

func (m *clientRegistry) clientVersionLocked(nodeID uint64) [2]uint64 {
	return [2]uint64{m.clientGeneration, m.clientVersions[nodeID]}
}

func (m *clientRegistry) invalidateClientVersionLocked(nodeID uint64) {
	if m.clientVersions == nil {
		m.clientVersions = make(map[uint64]uint64)
	}
	if _, exists := m.clientVersions[nodeID]; !exists && len(m.clientVersions) >= maxClientVersionEntries {
		// A generation bump invalidates in-flight creations before the tombstone map is reset.
		m.clientGeneration++
		clear(m.clientVersions)
	}
	m.clientVersions[nodeID]++
}

func (m *clientRegistry) invalidateAllClientVersionsLocked() {
	m.clientGeneration++
	clear(m.clientVersions)
}
