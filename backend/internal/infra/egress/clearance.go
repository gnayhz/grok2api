package egress

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxyurl"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (m *clearanceRuntime) clearanceLoadKey(key, proxyURL string) string {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	return fmt.Sprintf("%s:%d:%s", key, m.clearanceVersion, clearanceFingerprint(m.clearanceConfig, proxyURL))
}

func (m *clearanceRuntime) clearanceVersionCurrent(version uint64) bool {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	return version == m.clearanceVersion
}

func (m *clearanceRuntime) clearanceMode() string {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	return m.clearanceConfig.Mode
}

func (m *clearanceRuntime) ensureClearance(ctx context.Context, node domain.Node, proxyURL, existingCookies, existingUserAgent, key string, persist bool) (string, string, uint64, error) {
	m.clearanceMu.Lock()
	cfg := m.clearanceConfig
	version := m.clearanceVersion
	interval := clearanceRefreshInterval(cfg)
	now := time.Now().UTC()
	fingerprint := clearanceFingerprint(cfg, proxyURL)
	bindingFingerprint := clearanceBindingFingerprint(cfg, proxyURL)
	m.cleanupClearanceCacheLocked(now, interval)
	state, known := m.clearances[key]
	if key == "direct" {
		if !known {
			m.ensureClearanceCacheCapacityLocked()
		}
		state.used = true
		m.clearances[key] = state
	}
	if (!known || state.userAgent == "") && persist && (existingCookies != "" || node.ClearanceRefreshedAt != nil) {
		if !known {
			m.ensureClearanceCacheCapacityLocked()
		}
		m.clearanceSequence++
		state = clearanceState{
			generation: m.clearanceSequence,
			cookies:    existingCookies, userAgent: existingUserAgent, used: true, version: version,
			fingerprint: node.ClearanceFingerprint, bindingFingerprint: node.ClearanceBindingFingerprint,
			lastUsedAt: now,
		}
		if node.ClearanceRefreshedAt != nil {
			state.refreshedAt = *node.ClearanceRefreshedAt
		}
		known = true
		m.clearances[key] = state
	}
	if !known && cfg.Mode == "on_demand" {
		m.ensureClearanceCacheCapacityLocked()
		m.clearanceSequence++
		state = clearanceState{generation: m.clearanceSequence, cookies: existingCookies, userAgent: existingUserAgent, used: true, version: version, fingerprint: fingerprint, bindingFingerprint: bindingFingerprint, lastUsedAt: now}
		m.clearances[key] = state
		known = true
	}
	// A successful solve may legitimately return no Cloudflare cookies when the
	// selected egress does not trigger a challenge. The solver User-Agent marks
	// that cookie-less result as complete so requests do not block on re-solving.
	fresh := known && !state.invalid && state.userAgent != "" && state.version == version &&
		state.fingerprint == fingerprint && (state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint) &&
		!state.refreshedAt.IsZero() && now.Sub(state.refreshedAt) < interval
	if fresh {
		state.lastUsedAt = now
		m.clearances[key] = state
		cookies, userAgent := state.cookies, state.userAgent
		m.clearanceMu.Unlock()
		return cookies, userAgent, state.generation, nil
	}
	fallbackAllowed := known && !state.invalid && state.userAgent != "" &&
		(state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint)
	fallback := clearanceSolution{Cookies: state.cookies, UserAgent: state.userAgent, generation: state.generation}
	forceRefresh := known && state.invalid
	refreshAfter := time.Time{}
	if forceRefresh {
		refreshAfter = state.refreshedAt
	}
	if fallbackAllowed {
		state.lastUsedAt = now
		m.clearances[key] = state
	}
	if cfg.Mode == "on_demand" && !forceRefresh {
		m.clearanceMu.Unlock()
		if fallbackAllowed {
			return fallback.Cookies, fallback.UserAgent, fallback.generation, nil
		}
		return existingCookies, existingUserAgent, state.generation, nil
	}
	if cfg.Mode != "flaresolverr" && cfg.Mode != "on_demand" {
		m.clearanceMu.Unlock()
		return existingCookies, existingUserAgent, state.generation, nil
	}
	// 例行过期 serve-stale:已知、未失效、UA 完整、绑定指纹匹配,且只超出
	// 刷新窗口一个有界宽限时,立即交付旧解并触发一次后台合并刷新——例行
	// 过期不再让首个请求同步等完整求解(最坏求解超时+锁宽限)。后台刷新
	// 用去重标记限制每键至多一个在途协程;403 兜底闭环(invalidate→强制
	// 同步刷新)保持不变,旧解失效时最多多付一次 403 往返。
	routineStale := fallbackAllowed && !forceRefresh && !state.refreshedAt.IsZero() &&
		state.version == version && state.fingerprint == fingerprint &&
		now.Sub(state.refreshedAt) < interval+clearanceRoutineStaleGrace
	if routineStale {
		if _, running := m.backgroundClearanceRefreshes[key]; !running {
			if m.backgroundClearanceRefreshes == nil {
				m.backgroundClearanceRefreshes = make(map[string]struct{})
			}
			m.backgroundClearanceRefreshes[key] = struct{}{}
			m.clearanceMu.Unlock()
			if !m.tasks.start("refresh", func() { m.refreshClearanceInBackground(ctx, node, proxyURL, key, persist) }) {
				m.clearanceMu.Lock()
				delete(m.backgroundClearanceRefreshes, key)
				m.clearanceMu.Unlock()
			}
			return state.cookies, state.userAgent, state.generation, nil
		}
		m.clearanceMu.Unlock()
		return state.cookies, state.userAgent, state.generation, nil
	}
	m.clearanceMu.Unlock()

	result, err := m.sharedLoad(ctx, &m.clearanceLoads, m.clearanceLoadKey(key, proxyURL), func() (any, error) {
		// FlareSolverr 求解最长 cfg.Timeout(默认 1m):领头调用方(某个 HTTP 请求)
		// 断开时不能中止求解——所有合并等待者都会拿到 context canceled 类错误,
		// 与真实负载无关。求解与分布式锁都在脱离请求生命周期的 ctx 上进行;
		// 预算 = 求解超时 + 锁等待宽限。
		solveTimeout := m.currentClearanceTimeout()
		solveCtx, cancel := m.tasks.context(ctx, solveTimeout+clearanceLockGrace)
		defer cancel()
		return m.refreshNode(solveCtx, node, proxyURL, key, persist, forceRefresh, !fallbackAllowed, refreshAfter)
	})
	if err != nil {
		if ctx.Err() != nil {
			return "", "", 0, ctx.Err()
		}
		if fallbackAllowed && !errors.Is(err, repository.ErrConflict) {
			return fallback.Cookies, fallback.UserAgent, fallback.generation, nil
		}
		return "", "", 0, err
	}
	solution := result.(clearanceSolution)
	return solution.Cookies, solution.UserAgent, solution.generation, nil
}

// refreshClearanceInBackground 在脱离请求生命周期的预算内执行一次例行
// 刷新(与同步路径共用 clearanceLoads 合并),完成后清除去重标记。
func (m *clearanceRuntime) refreshClearanceInBackground(ctx context.Context, node domain.Node, proxyURL, key string, persist bool) {
	defer func() {
		m.clearanceMu.Lock()
		delete(m.backgroundClearanceRefreshes, key)
		m.clearanceMu.Unlock()
	}()
	solveTimeout := m.currentClearanceTimeout()
	solveCtx, cancel := m.tasks.context(ctx, solveTimeout+clearanceLockGrace)
	defer cancel()
	_, _, _ = m.clearanceLoads.Do(m.clearanceLoadKey(key, proxyURL), func() (any, error) {
		return m.refreshNode(solveCtx, node, proxyURL, key, persist, false, false, time.Time{})
	})
}

// currentClearanceTimeout 返回当前 Clearance 求解超时(零值时按默认 1m),
// 供 singleflight 闭包预算派生 ctx 使用。
func (m *clearanceRuntime) currentClearanceTimeout() time.Duration {
	m.clearanceMu.Lock()
	cfg := m.clearanceConfig
	m.clearanceMu.Unlock()
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return time.Minute
}

func (m *clearanceRuntime) refreshNode(ctx context.Context, node domain.Node, proxyURL, key string, persist, force, waitForPeer bool, refreshAfter time.Time) (clearanceSolution, error) {
	done, err := m.tasks.begin("clearance")
	if err != nil {
		return clearanceSolution{}, err
	}
	defer done()
	workCtx, cancelWork := m.tasks.context(ctx, m.currentClearanceTimeout()+clearanceLockGrace)
	defer cancelWork()
	// Administrative callers still cancel their own solve; shared callers pass
	// an already detached, bounded context.
	stopCaller := context.AfterFunc(ctx, cancelWork)
	defer stopCaller()
	ctx = workCtx
	m.clearanceMu.Lock()
	cfg := m.clearanceConfig
	solveVersion := m.clearanceVersion
	solver := m.solver
	lock := m.clearanceLock
	m.clearanceMu.Unlock()
	if cfg.Mode != "flaresolverr" && cfg.Mode != "on_demand" {
		return clearanceSolution{}, errors.New("FlareSolverr Clearance 未启用")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = time.Minute
	}
	fingerprint := clearanceFingerprint(cfg, proxyURL)
	bindingFingerprint := clearanceBindingFingerprint(cfg, proxyURL)
	interval := clearanceRefreshInterval(cfg)
	// A caller may observe a cache miss just before another shared solve
	// completes and releases its flight. Recheck at the execution boundary.
	m.clearanceMu.Lock()
	cached := m.clearances[key]
	fresh := !cached.invalid && cached.userAgent != "" && cached.version == solveVersion && cached.fingerprint == fingerprint && time.Since(cached.refreshedAt) < interval && (!force || (!refreshAfter.IsZero() && cached.refreshedAt.After(refreshAfter)))
	m.clearanceMu.Unlock()
	if fresh {
		return clearanceSolution{Cookies: cached.cookies, UserAgent: cached.userAgent, generation: cached.generation}, nil
	}
	if persist && node.ID != 0 && lock != nil {
		release, acquired, err := lock.Acquire(ctx, "egress-clearance:"+strconv.FormatUint(node.ID, 10), timeout+clearanceLockGrace)
		if err != nil {
			return clearanceSolution{}, fmt.Errorf("协调 Clearance 刷新: %w", err)
		}
		if !acquired {
			if !force {
				if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
					var cached bool
					solution, cached = m.cacheClearanceSolution(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
					if !cached {
						return clearanceSolution{}, repository.ErrConflict
					}
					return solution, nil
				}
			}
			if waitForPeer {
				if solution, refreshedAt, ok := m.waitPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval, timeout, refreshAfter); ok {
					var cached bool
					solution, cached = m.cacheClearanceSolution(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
					if !cached {
						return clearanceSolution{}, repository.ErrConflict
					}
					return solution, nil
				}
			}
			return clearanceSolution{}, errors.New("另一个实例正在刷新 Cloudflare Clearance")
		}
		defer release()
		if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
			// A peer may have refreshed the rejected Clearance immediately before
			// this instance acquired the distributed lock. Reuse that newer result
			// instead of performing a duplicate browser solve. A force refresh with
			// no newer persisted generation must still reach the solver.
			if !force || (!refreshAfter.IsZero() && refreshedAt.After(refreshAfter)) {
				var cached bool
				solution, cached = m.cacheClearanceSolution(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
				if !cached {
					return clearanceSolution{}, repository.ErrConflict
				}
				return solution, nil
			}
		}
	}
	solveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	solution, err := solver.Solve(solveCtx, cfg, proxyURL)
	if !m.clearanceVersionCurrent(solveVersion) {
		return clearanceSolution{}, repository.ErrConflict
	}
	if err != nil {
		m.recordClearanceError(ctx, node, persist)
		return clearanceSolution{}, fmt.Errorf("刷新出口 %q 的 Cloudflare Clearance: %w", node.Name, err)
	}
	now := time.Now().UTC()
	if persist && node.ID != 0 {
		encryptedCookies, encryptErr := m.cipher.Encrypt(solution.Cookies)
		if encryptErr != nil {
			return clearanceSolution{}, encryptErr
		}
		if err := m.repository.ApplyEgressClearance(ctx, domain.ClearanceUpdate{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, BindingRevision: node.BindingRevision, ExpectedRevision: node.ClearanceRevision, EncryptedCookie: encryptedCookies, UserAgent: solution.UserAgent, Fingerprint: fingerprint, BindingFingerprint: bindingFingerprint, RefreshedAt: now}); err != nil {
			return clearanceSolution{}, err
		}
		m.invalidateNodes()
	}
	var cachedOK bool
	solution, cachedOK = m.cacheClearanceSolution(key, solution, now, solveVersion, fingerprint, bindingFingerprint, interval)
	if !cachedOK {
		return clearanceSolution{}, repository.ErrConflict
	}
	return solution, nil
}

func (m *clearanceRuntime) waitPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval, timeout time.Duration, refreshAfter time.Time) (clearanceSolution, time.Time, bool) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return clearanceSolution{}, time.Time{}, false
		case <-ticker.C:
			if solution, refreshedAt, ok := m.loadPersistedClearance(waitCtx, nodeID, fingerprint, bindingFingerprint, interval); ok &&
				(refreshAfter.IsZero() || refreshedAt.After(refreshAfter)) {
				return solution, refreshedAt, true
			}
		}
	}
}

func (m *clearanceRuntime) loadPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval time.Duration) (clearanceSolution, time.Time, bool) {
	latest, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil || latest.ClearanceRefreshedAt == nil || latest.ClearanceFingerprint != fingerprint ||
		(latest.ClearanceBindingFingerprint != "" && latest.ClearanceBindingFingerprint != bindingFingerprint) ||
		time.Since(*latest.ClearanceRefreshedAt) >= interval {
		return clearanceSolution{}, time.Time{}, false
	}
	cookies, err := m.cipher.Decrypt(latest.EncryptedCloudflareCookie)
	if err != nil {
		return clearanceSolution{}, time.Time{}, false
	}
	cookies = cfcookies.Sanitize(cookies)
	userAgent := strings.TrimSpace(latest.UserAgent)
	if userAgent == "" {
		return clearanceSolution{}, time.Time{}, false
	}
	return clearanceSolution{Cookies: cookies, UserAgent: userAgent}, *latest.ClearanceRefreshedAt, true
}

func (m *clearanceRuntime) cacheClearanceSolution(key string, solution clearanceSolution, refreshedAt time.Time, version uint64, fingerprint, bindingFingerprint string, interval time.Duration) (clearanceSolution, bool) {
	m.clearanceMu.Lock()
	if version != m.clearanceVersion {
		m.clearanceMu.Unlock()
		return clearanceSolution{}, false
	}
	now := time.Now().UTC()
	m.cleanupClearanceCacheLocked(now, interval)
	if _, exists := m.clearances[key]; !exists {
		m.ensureClearanceCacheCapacityLocked()
	}
	m.clearanceSequence++
	solution.generation = m.clearanceSequence
	m.clearances[key] = clearanceState{generation: m.clearanceSequence,
		cookies: solution.Cookies, userAgent: solution.UserAgent, refreshedAt: refreshedAt,
		used: true, version: version, fingerprint: fingerprint, bindingFingerprint: bindingFingerprint, lastUsedAt: now,
	}
	m.clearanceMu.Unlock()
	return solution, true
}

func (m *clearanceRuntime) cleanupClearanceCacheLocked(now time.Time, interval time.Duration) {
	if m.clearances == nil {
		m.clearances = make(map[string]clearanceState)
	}
	if !m.lastClearanceCleanup.IsZero() && now.Sub(m.lastClearanceCleanup) < clearanceCacheCleanupInterval {
		return
	}
	m.lastClearanceCleanup = now
	idleTTL := interval * 2
	if idleTTL < clearanceCacheMinIdleTTL {
		idleTTL = clearanceCacheMinIdleTTL
	}
	for key, state := range m.clearances {
		lastUsedAt := state.lastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = state.refreshedAt
		}
		if !lastUsedAt.IsZero() && now.Sub(lastUsedAt) >= idleTTL {
			delete(m.clearances, key)
		}
	}
}

func (m *clearanceRuntime) ensureClearanceCacheCapacityLocked() {
	if len(m.clearances) < maxCachedClearances {
		return
	}
	type candidate struct {
		key      string
		lastUsed time.Time
	}
	candidates := make([]candidate, 0, len(m.clearances))
	for key, state := range m.clearances {
		lastUsedAt := state.lastUsedAt
		if lastUsedAt.IsZero() {
			lastUsedAt = state.refreshedAt
		}
		candidates = append(candidates, candidate{key: key, lastUsed: lastUsedAt})
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].lastUsed.Before(candidates[j].lastUsed)
	})
	removeCount := min(clearanceCacheEvictionBatch, len(candidates))
	for _, entry := range candidates[:removeCount] {
		delete(m.clearances, entry.key)
	}
}

func clearanceRefreshInterval(cfg ClearanceConfig) time.Duration {
	if cfg.RefreshInterval > 0 {
		return cfg.RefreshInterval
	}
	return 10 * time.Minute
}

func clearanceFingerprint(cfg ClearanceConfig, proxyURL string) string {
	value := strings.TrimSpace(cfg.FlareSolverrURL) + "\x00" + clearanceBindingFingerprint(cfg, proxyURL)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func clearanceBindingFingerprint(cfg ClearanceConfig, proxyURL string) string {
	value := strings.TrimRight(strings.TrimSpace(cfg.TargetURL), "/") + "\x00" + strings.TrimSpace(proxyURL)
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

func (m *clearanceRuntime) recordClearanceError(ctx context.Context, node domain.Node, persist bool) {
	if node.ID == 0 || !persist {
		return
	}
	if m.repository.RecordEgressClearanceError(ctx, node) == nil {
		m.invalidateNodes()
	}
}

func (m *clearanceRuntime) RefreshClearance(ctx context.Context, nodeID uint64) error {
	if nodeID == 0 {
		_, err, _ := m.clearanceLoads.Do(m.clearanceLoadKey("direct", ""), func() (any, error) {
			return m.refreshNode(ctx, domain.Node{Name: "direct", Enabled: true}, "", "direct", false, true, true, time.Time{})
		})
		return err
	}
	node, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	proxyURL, err := m.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return err
	}
	if domain.IsAccountTemplateProxy(proxyURL) {
		return fmt.Errorf("出口节点 %q 使用账号粘性代理，将在账号请求时按租约自动刷新 Clearance", node.Name)
	}
	proxyURL, err = proxyurl.NormalizeProxyURL(proxyURL)
	if err != nil {
		return err
	}
	// 强制刷新只接受比入口读取时更新的世代:锁被对端持有时,等待路径复用
	// 对端的新求解结果;对端超时未交付则如实报错。旧实现传零值,等待路径
	// 在第一个 200ms tick 就把管理员明确要求替换的旧 cookie 当作刷新成功
	// 返回并重新缓存为有效——与锁获取路径的 force 语义(2121 行:必求解
	// 或复用严格更新世代)相矛盾。
	refreshAfter := time.Time{}
	if node.ClearanceRefreshedAt != nil {
		refreshAfter = *node.ClearanceRefreshedAt
	}
	key := clearanceCacheKey(node.ID, proxyURL, false)
	_, err, _ = m.clearanceLoads.Do(m.clearanceLoadKey(key, proxyURL), func() (any, error) {
		return m.refreshNode(ctx, node, proxyURL, key, true, true, true, refreshAfter)
	})
	return err
}

func (m *clearanceRuntime) InvalidateClearance(nodeID uint64) {
	m.clearanceMu.Lock()
	m.invalidateNodeClearancesLocked(nodeID)
	m.clearanceMu.Unlock()
	m.transport.invalidate(map[uint64]struct{}{nodeID: {}}, "")
}

// invalidateNodeClearancesLocked 把节点名下所有 Clearance 缓存标记为失效,
// 覆盖节点级键(node:N)与粘性账号键(node:N:account:<digest>)。调用方
// 必须持有 m.clearanceMu。
func (m *clearanceRuntime) invalidateNodeClearancesLocked(nodeID uint64) {
	prefix := "node:" + strconv.FormatUint(nodeID, 10)
	if nodeID == 0 {
		prefix = "direct"
	}
	for key, state := range m.clearances {
		if key == prefix || strings.HasPrefix(key, prefix+":") {
			state.invalid = true
			state.used = true
			m.clearances[key] = state
		}
	}
}

// ForgetClearance evicts runtime state after an administrator changes or
// removes a node. Unlike a 403 rejection, it does not mark the persisted
// last-known-good cookie as invalid; ensureClearance will still verify its
// binding before using it as a solver-failure fallback.
func (m *clearanceRuntime) ForgetClearance(nodeID uint64) {
	m.ForgetClearances([]uint64{nodeID})
}

// ForgetClearances evicts a batch of node-scoped runtime state with one cache
// scan and one lock acquisition. Administrative bulk updates can contain
// thousands of nodes, so repeating the global snapshot invalidation per ID
// would add avoidable lock contention and CPU work.
func (m *clearanceRuntime) ForgetClearances(nodeIDs []uint64) {
	ids := make(map[uint64]struct{}, len(nodeIDs))
	prefixes := make(map[string]struct{}, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		if _, exists := ids[nodeID]; exists {
			continue
		}
		ids[nodeID] = struct{}{}
		prefix := "node:" + strconv.FormatUint(nodeID, 10)
		if nodeID == 0 {
			prefix = "direct"
		}
		prefixes[prefix] = struct{}{}
	}
	if len(ids) == 0 {
		return
	}
	m.clearanceMu.Lock()
	m.clearanceVersion++
	for key := range m.clearances {
		prefix := key
		if strings.HasPrefix(key, "node:") {
			if separator := strings.IndexByte(key[len("node:"):], ':'); separator >= 0 {
				prefix = key[:len("node:")+separator]
			}
		} else if strings.HasPrefix(key, "direct:") {
			prefix = "direct"
		}
		if _, selected := prefixes[prefix]; selected {
			delete(m.clearances, key)
		}
	}
	m.clearanceMu.Unlock()
	m.invalidateNodes()
	m.transport.invalidate(ids, "")
	m.invalidateBindings()
}

func (m *clearanceRuntime) invalidateClearanceKey(key string, client requestClient, expected uint64) bool {
	m.clearanceMu.Lock()
	state, known := m.clearances[key]
	if !known || expected != state.generation {
		m.clearanceMu.Unlock()
		return false
	}
	if state.invalid {
		m.clearanceMu.Unlock()
		return true
	}
	state.invalid = true
	state.used = true
	state.lastUsedAt = time.Now().UTC()
	m.clearances[key] = state
	m.clearanceMu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
	return true
}

func (m *clearanceRuntime) RefreshDueClearances(ctx context.Context, force bool) error {
	m.clearanceMu.Lock()
	cfg := m.clearanceConfig
	direct := m.clearances["direct"]
	version := m.clearanceVersion
	m.clearanceMu.Unlock()
	if cfg.Mode != "flaresolverr" {
		return nil
	}
	interval := clearanceRefreshInterval(cfg)
	now := time.Now().UTC()
	// 走 1s 快照缓存而非直查仓储:本循环每分钟触发,直查会与请求路径的
	// 快照装载形成重复回源。新鲜度语义安全:新鲜度判定窗口 ≥ 刷新间隔
	// (默认 10m,可配置下限即分钟级),1s 快照滞后远小于判定粒度;
	// Enabled/EncryptedProxyURL 过滤同样容忍 1s 滞后(启用/停用经失效器
	// 即时失效快照,不存在长滞留)。
	nodes, err := m.listNodes(ctx, now)
	if err != nil {
		return err
	}
	var refreshErrors []error
	webNodeCount := 0
	for _, node := range nodes {
		if !node.Enabled || node.EncryptedProxyURL == "" {
			continue
		}
		webNodeCount++
		proxyURL, decryptErr := m.cipher.Decrypt(node.EncryptedProxyURL)
		if decryptErr != nil {
			refreshErrors = append(refreshErrors, decryptErr)
			continue
		}
		if domain.IsAccountTemplateProxy(proxyURL) {
			// Resin clearance is account/IP bound and has no safe node-wide value
			// for a background task to solve or persist.
			continue
		}
		proxyURL, normalizeErr := proxyurl.NormalizeProxyURL(proxyURL)
		if normalizeErr != nil {
			refreshErrors = append(refreshErrors, normalizeErr)
			continue
		}
		m.clearanceMu.Lock()
		key := clearanceCacheKey(node.ID, proxyURL, false)
		state, known := m.clearances[key]
		m.clearanceMu.Unlock()
		fingerprint := clearanceFingerprint(cfg, proxyURL)
		memoryFresh := known && !state.invalid && state.version == version && state.fingerprint == fingerprint && now.Sub(state.refreshedAt) < interval
		persistedFresh := (!known || !state.invalid) && node.ClearanceRefreshedAt != nil && node.ClearanceFingerprint == fingerprint && now.Sub(*node.ClearanceRefreshedAt) < interval
		if !force && (memoryFresh || persistedFresh) {
			continue
		}
		refreshForce := force || (known && state.invalid)
		refreshAfter := time.Time{}
		if refreshForce && known && state.invalid {
			refreshAfter = state.refreshedAt
		}
		_, refreshErr, _ := m.clearanceLoads.Do(m.clearanceLoadKey(key, proxyURL), func() (any, error) {
			return m.refreshNode(ctx, node, proxyURL, key, true, refreshForce, false, refreshAfter)
		})
		if refreshErr != nil {
			refreshErrors = append(refreshErrors, refreshErr)
		}
	}
	shouldUseDirect := direct.used || force && webNodeCount == 0
	if shouldUseDirect && (force || direct.invalid || direct.userAgent == "" || direct.version != version || now.Sub(direct.refreshedAt) >= interval) {
		_, err, _ := m.clearanceLoads.Do(m.clearanceLoadKey("direct", ""), func() (any, error) {
			return m.refreshNode(ctx, domain.Node{Name: "direct", Enabled: true}, "", "direct", false, force, false, time.Time{})
		})
		if err != nil {
			refreshErrors = append(refreshErrors, err)
		}
	}
	return errors.Join(refreshErrors...)
}

// isGrokWebScope 判定 scope 是否走 Grok Web/Console 浏览器通道。
// 「需要浏览器 clearance」与「grok-web 系 scope」是同一谓词：Build 走 CLI
// 通道、Console 资产走公共媒体主机，二者都不携带账号/节点 clearance cookie
// （向其它 origin 转发凭据只会暴露 cookie，并让匿名下载依赖 cookie 存储）。
func isGrokWebScope(scope domain.Scope) bool {
	return scope == domain.ScopeWeb || scope == domain.ScopeWebAsset || scope == domain.ScopeConsole
}
