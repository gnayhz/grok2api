package egress

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxyurl"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"golang.org/x/sync/singleflight"
)

const DefaultUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
const nodeSnapshotTTL = time.Second
const operationsConfigSnapshotTTL = time.Second
const proxyPoolRetryLimit = 2
const clientCacheIdleTTL = 30 * time.Minute
const clientCacheCleanupInterval = time.Minute
const clientCacheTouchInterval = time.Minute
const maxCachedClients = 4096
const clearanceLockGrace = 30 * time.Second
const clearanceCacheCleanupInterval = time.Minute
const clearanceCacheMinIdleTTL = 30 * time.Minute
const maxCachedClearances = 16384
const clearanceCacheEvictionBatch = 256

// clientClosedRequestStatus is the conventional proxy status for a client-aborted request.
const clientClosedRequestStatus = 499
const egressIPv4ProbeEndpoint = "https://ipinfo.io/json"
const egressIPv6ProbeEndpoint = "https://v6.ipinfo.io/json"
const cloudflareIPv4ProbeEndpoint = "https://1.1.1.1/cdn-cgi/trace"
const cloudflareIPv6ProbeEndpoint = "https://[2606:4700:4700::1111]/cdn-cgi/trace"
const egressProbeTimeout = 15 * time.Second
const failureProbeCompletionGrace = 5 * time.Second
const failureProbeTimeout = 20 * time.Second
const failureProbeWaitTimeout = 5 * time.Second
const clientCreationRetryLimit = 3
const maxClientVersionEntries = 4096

var errNodeSnapshotInvalidated = errors.New("egress node snapshot invalidated")
var errClientCacheInvalidated = errors.New("egress client cache invalidated")
var errAccountConnectionIsolationDisabled = errors.New("egress account connection isolation disabled")

type Lease struct {
	NodeID           uint64
	NodeName         string
	Scope            domain.Scope
	ProxyURL         string
	UserAgent        string
	CFCookies        string
	client           requestClient
	browser          *browserClient
	sticky           bool
	proxyPool        bool
	freshTunnel      bool
	clearanceKey     string
	clearanceManager *Manager
	release          func()
}

type requestClient interface {
	Do(*http.Request) (*http.Response, error)
	CloseIdleConnections()
}

type FailureProber func(context.Context, uint64) (domain.ProbeResult, error)

type failureProbeState struct {
	running       bool
	lastCompleted time.Time
	done          chan struct{}
}

func (l *Lease) Do(request *http.Request) (*http.Response, error) {
	return l.doRequest(request, true)
}

// DoDeferredForbidden executes an HTTP request while leaving 403 clearance
// invalidation to the caller after it has classified the response body.
func (l *Lease) DoDeferredForbidden(request *http.Request) (*http.Response, error) {
	return l.doRequest(request, false)
}

func (l *Lease) doRequest(request *http.Request, invalidateForbidden bool) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	// Rotating proxy endpoints choose an exit when a new CONNECT tunnel is
	// established. Reusing a Build keep-alive/HTTP2 connection would pin many
	// otherwise independent requests to one exit and defeat proxy-pool
	// rotation. Proxy-pool mode is explicit, so trade the extra handshake for a
	// fresh tunnel without changing fixed-proxy, direct, Web, or Console paths.
	if l.Scope == domain.ScopeBuild && l.freshTunnel {
		request.Close = true
	}
	response, err := l.do(request)
	recordPhysicalCall(request.Context(), response, err)
	if invalidateForbidden && err == nil && response != nil && response.StatusCode == http.StatusForbidden {
		l.InvalidateClearance()
	}
	return response, err
}

// InvalidateClearance invalidates the exact browser-session binding used by
// this lease after a 403 has been classified as egress-related.
func (l *Lease) InvalidateClearance() {
	if l != nil && l.clearanceManager != nil && l.clearanceKey != "" {
		l.clearanceManager.invalidateClearanceKey(l.clearanceKey, l.client)
	}
}

func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
		l.release = nil
	}
}

// errOnlyCryptor 表示「未配置凭据加密器」：空值幂等、非空一律
// 明确报错。此前 nil *Cipher 直传会在非空密文上 panic。
type errOnlyCryptor struct{}

func (errOnlyCryptor) Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	return "", fmt.Errorf("出口管理器未配置凭据加密器")
}

func (errOnlyCryptor) Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	return "", fmt.Errorf("出口管理器未配置凭据加密器，无法解密已存储的出口凭据")
}

type Manager struct {
	repository   repository.EgressRepository
	cipher       security.Cryptor
	logger       *slog.Logger
	nodeMu       sync.RWMutex
	clientMu     sync.RWMutex
	clearanceMu  sync.Mutex
	operationsMu sync.RWMutex
	clients      map[clientCacheKey]cachedClient
	inflight     sync.Map
	nodes        map[string]cachedNodeSnapshot
	healthyNodes map[uint64]time.Time
	// proxyFlagMemo 按节点 ID 记忆 {account} 粘性判定, 键含密文:
	// 代理 URL 变更(轮换/编辑)自然失效。池路由路径的成员来自独立的
	// 池缓存, 不经过全量节点快照, 没有这层记忆则每个请求都要对每个
	// 成员做一次 AES-GCM 解密。由 nodeMu 保护。
	proxyFlagMemo          map[uint64]proxyFlagMemoEntry
	nodeVersions           map[string]uint64
	nodeLoads              singleflight.Group
	clientLoads            singleflight.Group
	clientVersions         map[uint64]uint64
	clientGeneration       uint64
	buildHeaderTimeout     atomic.Int64
	buildStreamIdleTimeout atomic.Int64
	accountIsolated        atomic.Bool
	operationsConfig       cachedOperationsConfig
	operationsConfigLoad   singleflight.Group
	operationsConfigVer    uint64
	routeRuleNodeMu        sync.Mutex
	routeRuleNodeCache     map[uint64]cachedRoutingTargetNode
	failureProbeMu         sync.Mutex
	failureProber          FailureProber
	failureProbes          map[uint64]failureProbeState
	lastClientCleanup      time.Time
	clearanceLoads         singleflight.Group
	clearanceConfig        ClearanceConfig
	clearanceVersion       uint64
	clearances             map[string]clearanceState
	lastClearanceCleanup   time.Time
	solver                 clearanceSolver
	clearanceLock          repository.DistributedLock
	newBuildClient         func(string, time.Duration) (requestClient, error)
	newBuildEnvClient      func(time.Duration) (requestClient, error)
	newBrowserClient       func(string, string) (*browserClient, error)
	// softMu 保护 softCooldowns:降智证据的未决软冷却(进程内,分钟级)。
	softMu          sync.RWMutex
	softCooldowns   map[uint64]softCooldown
	softBase        time.Duration
	softMax         time.Duration
	fallbackMu      sync.Mutex
	poolFallbacks   map[uint64]cachedPoolFallback
	rotationMu      sync.Mutex
	rotationCursors map[uint64]uint64
	// rotationPersists 去抖游标持久化:last 记录最近成功写入 DB 的游标,
	// writing/pending 把并发推进合并为一次异步写,避免选路热路径被
	// 同步 DB 写阻塞。
	rotationPersists map[uint64]*rotationPersistState
}

// rotationPersistState 由 rotationMu 保护。
type rotationPersistState struct {
	last    uint64
	pending uint64
	writing bool
}
type clearanceState struct {
	cookies            string
	userAgent          string
	refreshedAt        time.Time
	invalid            bool
	used               bool
	version            uint64
	fingerprint        string
	bindingFingerprint string
	lastUsedAt         time.Time
}

type egressStateRepository interface {
	UpdateEgressNodeClearance(context.Context, uint64, string, string, string, string, time.Time) error
	UpdateEgressNodeHealth(context.Context, uint64, float64, int, *time.Time, string) error
	UpdateEgressNodeLastError(context.Context, uint64, string) error
}

// egressQualityStateRepository persists exit-IP quality quarantine state. It
// is deliberately separate from egressStateRepository so lightweight test and
// routing repositories keep their narrow contracts.
type egressQualityStateRepository interface {
	UpdateEgressNodeQualityState(context.Context, uint64, float64, int, *time.Time, string, int, *time.Time) error
}

// operationsConfigRepository is optional so lightweight routing repositories
// retain their narrow contract. The relational implementation supplies it,
// allowing fallback policy to be read only when primary selection fails.

// egressPoolStore supplies dedicated-pool lookups. The relational repository
// implements it; lightweight test repositories keep their narrow contracts.
type egressPoolStore interface {
	GetEgressPool(ctx context.Context, id uint64) (domain.Pool, error)
	ListEgressNodesByPool(ctx context.Context, poolID uint64) ([]domain.Node, error)
}

// softCooldown carries pending degrade-evidence isolation for one node:
// evidence arrived but attribution has not confirmed (or cannot run). All
// accounts avoid the node until the deadline; repeats escalate exponentially.
type softCooldown struct {
	count int
	until time.Time
}
type operationsConfigRepository interface {
	GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error)
}

type cachedClient struct {
	client   requestClient
	browser  *browserClient
	lastUsed time.Time
}

type clientCacheKey struct {
	nodeID          uint64
	scope           domain.Scope
	fingerprint     string
	accountIdentity string
}

type cachedNodeSnapshot struct {
	values []domain.Node
	// poolFlags 缓存每个节点的"代理池模式"判定(ProxyPool 列位或 {account}
	// 模板)。判定需要解密代理地址,装快照时算一次,请求热路径只查表。
	poolFlags map[uint64]bool
	expiresAt time.Time
}

type cachedOperationsConfig struct {
	value     domain.OperationsConfig
	expiresAt time.Time
}

func NewManager(repository repository.EgressRepository, cipher security.Cryptor) *Manager {
	// cipher 为 nil 时归一化为「无凭据加解密能力」占位：对空串与
	// *Cipher 一致（幂等返回空），对非空密文返回明确错误而非 panic。
	// HEAD 上 nil *Cipher + 非空密文会在 Decrypt 内 nil deref panic
	// （stickyFlagMemoized 等路径的地雷，Cryptor 接线后 race 套件
	// 捕获）；显式错误让「无出口凭据部署/测试传 nil」语义安全。
	if cipher == nil {
		cipher = errOnlyCryptor{}
	}
	manager := &Manager{
		repository: repository, cipher: cipher,
		clients: make(map[clientCacheKey]cachedClient),
		nodes:   make(map[string]cachedNodeSnapshot), healthyNodes: make(map[uint64]time.Time), proxyFlagMemo: make(map[uint64]proxyFlagMemoEntry),
		nodeVersions: make(map[string]uint64), clientVersions: make(map[uint64]uint64), clearances: make(map[string]clearanceState),
		routeRuleNodeCache: make(map[uint64]cachedRoutingTargetNode),
		softCooldowns:      make(map[uint64]softCooldown),
		poolFallbacks:      make(map[uint64]cachedPoolFallback),
		rotationCursors:    make(map[uint64]uint64),
		rotationPersists:   make(map[uint64]*rotationPersistState),
		softBase:           5 * time.Minute, softMax: time.Hour,
		failureProbes:  make(map[uint64]failureProbeState),
		newBuildClient: newBuildRequestClient, newBuildEnvClient: newBuildEnvironmentRequestClient, newBrowserClient: newBrowserClient,
		solver:          flaresolverrSolver{},
		clearanceConfig: ClearanceConfig{Mode: "manual", TargetURL: "https://grok.com", Timeout: time.Minute, RefreshInterval: 10 * time.Minute},
	}
	manager.buildHeaderTimeout.Store(int64(settingsdomain.DefaultBuildResponseHeaderTimeout))
	manager.buildStreamIdleTimeout.Store(int64(settingsdomain.DefaultBuildStreamIdleTimeout))
	return manager
}

func (m *Manager) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.Default()
	}
	m.logger = logger
}

func (m *Manager) log() *slog.Logger {
	if m == nil || m.logger == nil {
		return slog.Default()
	}
	return m.logger
}

// SetFailureProber enables an immediate, deduplicated connectivity probe after
// a fixed proxy reports a transport failure. The callback persists the probe
// result; it must not depend on the failed request context.
func (m *Manager) SetFailureProber(value FailureProber) {
	m.failureProbeMu.Lock()
	m.failureProber = value
	if value == nil {
		clear(m.failureProbes)
	}
	m.failureProbeMu.Unlock()
}

func (m *Manager) scheduleFailureProbe(node domain.Node) {
	m.failureProbeMu.Lock()
	prober := m.failureProber
	state := m.failureProbes[node.ID]
	if prober == nil || state.running {
		m.failureProbeMu.Unlock()
		return
	}
	state.running = true
	state.done = make(chan struct{})
	m.failureProbes[node.ID] = state
	done := state.done
	m.failureProbeMu.Unlock()

	m.log().Info("egress_failure_probe_scheduled", "node_id", node.ID, "node_name", node.Name)
	go func() {
		// 探针回调走 repo/probe 全链路, panic 不得击穿进程:batch.Do 转 PanicError。
		var (
			result domain.ProbeResult
			err    error
		)
		if probeErr := batch.Do(context.Background(), func(taskCtx context.Context) error {
			taskCtx, probeCancel := context.WithTimeout(taskCtx, failureProbeTimeout)
			defer probeCancel()
			result, err = prober(taskCtx, node.ID)
			return nil
		}); probeErr != nil {
			var panicErr *batch.PanicError
			if errors.As(probeErr, &panicErr) {
				err = panicErr
			}
		}

		m.failureProbeMu.Lock()
		state := m.failureProbes[node.ID]
		if state.done == done {
			state.running = false
			state.lastCompleted = time.Now().UTC()
			m.failureProbes[node.ID] = state
		}
		close(done)
		m.failureProbeMu.Unlock()

		if err != nil {
			m.log().Warn("egress_failure_probe_failed", "node_id", node.ID, "node_name", node.Name, "error", err)
			return
		}
		if result.Status == domain.ProbeStatusHealthy {
			m.invalidateNodes()
		}
		m.log().Info("egress_failure_probe_completed", "node_id", node.ID, "node_name", node.Name, "probe_status", result.Status, "latency_ms", result.LatencyMS)
	}()
}

func (m *Manager) waitForFailureProbe(ctx context.Context, nodeID uint64) (bool, error) {
	now := time.Now().UTC()
	m.failureProbeMu.Lock()
	state, exists := m.failureProbes[nodeID]
	m.failureProbeMu.Unlock()
	if !exists {
		return false, nil
	}
	if !state.running {
		return !state.lastCompleted.IsZero() && now.Sub(state.lastCompleted) < failureProbeCompletionGrace, nil
	}
	if state.done == nil {
		return false, nil
	}
	timer := time.NewTimer(failureProbeWaitTimeout)
	defer timer.Stop()
	select {
	case <-state.done:
		return true, nil
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, nil
	}
}

// UpdateBuildResponseHeaderTimeout rebuilds only cached Build clients. Active
// requests keep their current transport and are not interrupted.
func (m *Manager) UpdateBuildResponseHeaderTimeout(value time.Duration) {
	if value <= 0 {
		value = settingsdomain.DefaultBuildResponseHeaderTimeout
	}
	if previous := time.Duration(m.buildHeaderTimeout.Swap(int64(value))); previous == value {
		return
	}
	m.clientMu.Lock()
	var stale []requestClient
	for key, cached := range m.clients {
		if key.scope == domain.ScopeBuild {
			stale = append(stale, m.evictClientLocked(key, cached))
		}
	}
	m.clientMu.Unlock()
	closeRequestClients(stale)
}

// UpdateBuildStreamIdleTimeout affects subsequent Build streams. Active
// response bodies retain the deadline captured by their existing wrapper and
// are not interrupted; the underlying HTTP connection pool is unchanged.
func (m *Manager) UpdateBuildStreamIdleTimeout(value time.Duration) {
	if value <= 0 {
		value = settingsdomain.DefaultBuildStreamIdleTimeout
	}
	m.buildStreamIdleTimeout.Store(int64(value))
}

// BuildStreamIdleTimeout returns the configured stream idle deadline for Grok
// Build responses. Returns zero when idle enforcement is disabled.
func (m *Manager) BuildStreamIdleTimeout() time.Duration {
	return time.Duration(m.buildStreamIdleTimeout.Load())
}

// UpdateAccountIsolatedConnections toggles per-account upstream connection pools.
// When enabled, different accounts do not share TCP/HTTP clients so upstream
// egress load balancers can spread traffic by connection; the same account still
// reuses its own pool. Changing the setting rebuilds cached clients without
// interrupting in-flight requests.
func (m *Manager) UpdateAccountIsolatedConnections(enabled bool) {
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
	closeRequestClients(stale)
	m.log().Info("egress_account_connection_isolation_updated", "enabled", enabled, "evicted_clients", len(stale))
}

// AccountIsolatedConnections reports whether upstream clients are partitioned by account.
func (m *Manager) AccountIsolatedConnections() bool {
	return m != nil && m.accountIsolated.Load()
}

func isolationAccountIdentity(ctx context.Context, scope domain.Scope, affinity string) string {
	identity := accountFromContext(ctx)
	if identity != "" {
		return identity
	}
	affinity = strings.TrimSpace(affinity)
	if affinity != "" {
		return string(scope) + "_" + affinity
	}
	return "shared"
}

// SetClearanceLock enables cross-instance coordination for shared, fixed egress
// nodes. Account-bound Resin clearances remain process-local because they must
// never be persisted into the node-wide cookie fields.
func (m *Manager) SetClearanceLock(value repository.DistributedLock) {
	m.clearanceMu.Lock()
	m.clearanceLock = value
	m.clearanceMu.Unlock()
}

func (m *Manager) UpdateClearanceConfig(value ClearanceConfig) {
	value.Mode = strings.TrimSpace(value.Mode)
	value.FlareSolverrURL = strings.TrimSpace(value.FlareSolverrURL)
	value.TargetURL = strings.TrimRight(strings.TrimSpace(value.TargetURL), "/")
	m.clearanceMu.Lock()
	previous := m.clearanceConfig
	m.clearanceConfig = value
	configurationChanged := previous.Mode != value.Mode || previous.FlareSolverrURL != value.FlareSolverrURL || previous.TargetURL != value.TargetURL
	if configurationChanged {
		m.clearanceVersion++
		m.clientMu.Lock()
		m.invalidateAllClientVersionsLocked()
		m.clientMu.Unlock()
	}
	m.clearanceMu.Unlock()
}

func (m *Manager) Acquire(ctx context.Context, scope domain.Scope, affinity string) (*Lease, error) {
	lease, _, err := m.acquire(ctx, scope, affinity, true, "")
	return lease, err
}

// AcquireBuildEnvironmentDirectIfIsolated creates an account-partitioned direct
// Build lease while preserving the legacy direct transport's environment-proxy
// semantics. The bool is false when isolation was disabled before the lease was
// acquired, allowing the caller to retain its original fallback transport.
func (m *Manager) AcquireBuildEnvironmentDirectIfIsolated(ctx context.Context, affinity string) (*Lease, bool, error) {
	selected := domain.Node{ID: 0, Name: "direct", Enabled: true, Health: 1}
	lease, _, err := m.leaseForNodeWithOptions(ctx, domain.ScopeBuild, affinity, "", false, selected, clientOptions{
		buildEnvironmentProxy:   true,
		requireAccountIsolation: true,
	})
	if errors.Is(err, errAccountConnectionIsolationDisabled) {
		return nil, false, nil
	}
	return lease, err == nil, err
}

// AcquireCredential binds the outbound proxy identity to one persisted
// Provider credential. Resin templates use this identity as their Account.
func (m *Manager) AcquireCredential(ctx context.Context, scope domain.Scope, credential accountdomain.Credential) (*Lease, error) {
	identity := strings.TrimSpace(credential.EgressIdentity)
	if identity == "" {
		identity = string(credential.Provider) + "_" + strconv.FormatUint(credential.ID, 10)
	}
	// Web and Console accounts can be two database projections of the same SSO
	// login. Resin must see one stable account identity across both channels;
	// otherwise the proxy rotates the IP while the clearance remains bound to
	// the other lease. The digest is non-reversible and is only used as a proxy
	// template account label.
	if strings.TrimSpace(credential.EgressIdentity) == "" && credential.AuthType == accountdomain.AuthTypeSSO && strings.TrimSpace(credential.EncryptedAccessToken) != "" {
		token, decryptErr := m.cipher.Decrypt(credential.EncryptedAccessToken)
		if decryptErr != nil {
			return nil, decryptErr
		}
		identity = "sso_" + security.HashToken(token)[:32]
	}
	ctx = WithAccountIdentity(ctx, identity)
	lease, _, err := m.acquire(ctx, scope, strconv.FormatUint(credential.ID, 10), true, credential.EncryptedCloudflareCookie)
	return lease, err
}

func (m *Manager) AcquireIfConfigured(ctx context.Context, scope domain.Scope, affinity string) (*Lease, bool, error) {
	return m.acquire(ctx, scope, affinity, false, "")
}

type preparedEgressProbe struct {
	nodeID   uint64
	nodeName string
	proxyURL string
}

// ProbeEgressNode verifies IPv4 and IPv6 independently through fixed provider
// endpoints. Both requests share one immutable node snapshot so a concurrent
// administrator edit cannot mix results from different proxy configurations.
func (m *Manager) ProbeEgressNode(ctx context.Context, node domain.Node) (domain.ProbeResult, error) {
	type outcome struct {
		family string
		result domain.ProbeFamilyResult
		err    error
	}
	startedAt := time.Now().UTC()
	var provider domain.ProbeProvider
	config, supported, configErr := m.loadOperationsConfig(ctx, time.Now().UTC())
	if configErr != nil {
		message := "读取代理探测服务配置失败"
		result := failedEgressProbeResult(provider, message, startedAt)
		m.logProbeSetupFailure(ctx, node, provider, "load_probe_config", message, configErr, result.LatencyMS)
		return result, configErr
	}
	if supported {
		provider = config.ProbeProvider.Normalized()
	} else {
		provider = domain.ProbeProviderCloudflare
	}
	target, stage, message, prepareErr := m.prepareEgressProbe(node)
	if prepareErr != nil {
		result := failedEgressProbeResult(provider, message, startedAt)
		m.logProbeSetupFailure(ctx, node, provider, stage, message, prepareErr, result.LatencyMS)
		return result, prepareErr
	}
	ipv4Endpoint, ipv6Endpoint := probeEndpoints(provider)
	outcomes := make(chan outcome, 2)
	for _, probe := range []struct{ family, endpoint string }{{"ipv4", ipv4Endpoint}, {"ipv6", ipv6Endpoint}} {
		go func() {
			result, err := m.probeEgressEndpoint(ctx, target, provider, probe.family, probe.endpoint)
			outcomes <- outcome{family: probe.family, result: result, err: err}
		}()
	}
	var ipv4Err, ipv6Err error
	result := domain.ProbeResult{Status: domain.ProbeStatusUnhealthy, Provider: provider}
	for range 2 {
		current := <-outcomes
		if current.family == "ipv4" {
			result.IPv4, ipv4Err = current.result, current.err
		} else {
			result.IPv6, ipv6Err = current.result, current.err
		}
	}
	result.TestedAt = time.Now().UTC()
	result.LatencyMS = max(result.IPv4.LatencyMS, result.IPv6.LatencyMS)
	if result.IPv4.Status == domain.ProbeStatusHealthy {
		result.Status, result.ExitIP = domain.ProbeStatusHealthy, result.IPv4.ExitIP
	} else if result.IPv6.Status == domain.ProbeStatusHealthy {
		result.Status, result.ExitIP = domain.ProbeStatusHealthy, result.IPv6.ExitIP
	}
	if result.Status == domain.ProbeStatusHealthy {
		return result, nil
	}
	errorsByFamily := make([]string, 0, 2)
	if result.IPv4.Error != "" {
		errorsByFamily = append(errorsByFamily, "IPv4: "+result.IPv4.Error)
	}
	if result.IPv6.Error != "" {
		errorsByFamily = append(errorsByFamily, "IPv6: "+result.IPv6.Error)
	}
	result.Error = strings.Join(errorsByFamily, "; ")
	if result.Error == "" {
		result.Error = "IPv4 和 IPv6 代理探测均失败"
	}
	return result, errors.Join(ipv4Err, ipv6Err, errors.New(result.Error))
}

func (m *Manager) prepareEgressProbe(node domain.Node) (preparedEgressProbe, string, string, error) {
	target := preparedEgressProbe{nodeID: node.ID, nodeName: node.Name}
	if node.ID == 0 {
		message := "代理节点 ID 无效"
		return target, "validate_node", message, errors.New(message)
	}
	proxyURL, err := m.cipher.Decrypt(node.EncryptedProxyURL)
	if err != nil {
		return target, "decrypt_proxy", "读取代理配置失败", err
	}
	proxyURL, err = proxyurl.NormalizeProxyURL(proxyURL)
	if err != nil {
		return target, "normalize_proxy", "代理地址无效", err
	}
	if proxyURL == "" {
		message := "未配置代理地址"
		return target, "normalize_proxy", message, errors.New(message)
	}
	if domain.IsAccountTemplateProxy(proxyURL) {
		proxyURL, err = renderAccountProxyURL(proxyURL, "egress_probe")
		if err != nil {
			return target, "render_proxy_identity", "账号代理模板无效", err
		}
	}
	target.proxyURL = proxyURL
	return target, "", "", nil
}

func failedEgressProbeResult(provider domain.ProbeProvider, message string, startedAt time.Time) domain.ProbeResult {
	completedAt := time.Now().UTC()
	latencyMS := max(1, int(completedAt.Sub(startedAt).Milliseconds()))
	family := domain.ProbeFamilyResult{
		Status: domain.ProbeStatusUnhealthy, TestedAt: completedAt, LatencyMS: latencyMS, Error: message,
	}
	return domain.ProbeResult{
		Status: domain.ProbeStatusUnhealthy, TestedAt: completedAt, LatencyMS: latencyMS, Error: message,
		Provider: provider, IPv4: family, IPv6: family,
	}
}

func (m *Manager) logProbeSetupFailure(ctx context.Context, node domain.Node, provider domain.ProbeProvider, stage, message string, err error, durationMS int) {
	attributes := []any{
		"node_id", node.ID, "node_name", node.Name,
		"probe_provider", provider, "address_family", "all", "endpoint", "",
		"stage", stage, "status_code", 0, "duration_ms", durationMS,
		"connect_ms", 0, "tls_ms", 0, "first_byte_ms", 0,
		"probe_error", message,
	}
	if err != nil {
		attributes = append(attributes, "error", sanitizeFlareSolverrMessage(err.Error()))
	}
	m.log().WarnContext(ctx, "egress_probe_failed", attributes...)
}

func probeEndpoints(provider domain.ProbeProvider) (string, string) {
	if provider.Normalized() == domain.ProbeProviderCloudflare {
		return cloudflareIPv4ProbeEndpoint, cloudflareIPv6ProbeEndpoint
	}
	return egressIPv4ProbeEndpoint, egressIPv6ProbeEndpoint
}

func (m *Manager) probeEgressEndpoint(ctx context.Context, target preparedEgressProbe, provider domain.ProbeProvider, family, targetURL string) (result domain.ProbeFamilyResult, probeErr error) {
	startedAt := time.Now().UTC()
	result = domain.ProbeFamilyResult{Status: domain.ProbeStatusUnhealthy, TestedAt: startedAt}
	stage := "create_client"
	statusCode := 0
	var traceMu sync.Mutex
	var connectStartedAt, tlsStartedAt time.Time
	connectDone, connectFailed := false, false
	tlsDone, tlsFailed := false, false
	gotFirstByte := false
	connectMS, tlsMS, firstByteMS := 0, 0, 0
	defer func() {
		completedAt := time.Now().UTC()
		durationMS := max(1, int(completedAt.Sub(startedAt).Milliseconds()))
		result.TestedAt = completedAt
		if result.LatencyMS == 0 {
			result.LatencyMS = durationMS
		}
		traceMu.Lock()
		if !connectStartedAt.IsZero() && connectMS == 0 {
			connectMS = max(1, int(time.Since(connectStartedAt).Milliseconds()))
		}
		if !tlsStartedAt.IsZero() && tlsMS == 0 {
			tlsMS = max(1, int(time.Since(tlsStartedAt).Milliseconds()))
		}
		if stage == "first_byte" && firstByteMS == 0 {
			firstByteMS = durationMS
		}
		connectDurationMS, tlsDurationMS, firstByteDurationMS := connectMS, tlsMS, firstByteMS
		traceMu.Unlock()
		attributes := []any{
			"node_id", target.nodeID,
			"node_name", target.nodeName,
			"probe_provider", provider,
			"address_family", family,
			"endpoint", targetURL,
			"stage", stage,
			"status_code", statusCode,
			"duration_ms", durationMS,
			"connect_ms", connectDurationMS,
			"tls_ms", tlsDurationMS,
			"first_byte_ms", firstByteDurationMS,
		}
		if probeErr != nil || result.Status != domain.ProbeStatusHealthy {
			attributes = append(attributes, "probe_error", result.Error)
			if probeErr != nil {
				attributes = append(attributes, "error", sanitizeFlareSolverrMessage(probeErr.Error()))
			}
			m.log().WarnContext(ctx, "egress_probe_failed", attributes...)
			return
		}
		attributes = append(attributes, "latency_ms", result.LatencyMS, "exit_ip", result.ExitIP)
		m.log().InfoContext(ctx, "egress_probe_succeeded", attributes...)
	}()
	clientFactory := m.newBuildClient
	if clientFactory == nil {
		clientFactory = newBuildRequestClient
	}
	client, err := clientFactory(target.proxyURL, egressProbeTimeout)
	if err != nil {
		result.Error = "创建代理连接失败"
		return result, err
	}
	defer client.CloseIdleConnections()
	probeCtx, cancel := context.WithTimeout(ctx, egressProbeTimeout)
	defer cancel()
	probeCtx = httptrace.WithClientTrace(probeCtx, &httptrace.ClientTrace{
		ConnectStart: func(_, _ string) {
			traceMu.Lock()
			if connectStartedAt.IsZero() {
				connectStartedAt = time.Now()
			}
			traceMu.Unlock()
		},
		ConnectDone: func(_, _ string, traceErr error) {
			traceMu.Lock()
			connectDone = true
			connectFailed = traceErr != nil
			if !connectStartedAt.IsZero() && connectMS == 0 {
				connectMS = max(1, int(time.Since(connectStartedAt).Milliseconds()))
			}
			traceMu.Unlock()
		},
		TLSHandshakeStart: func() {
			traceMu.Lock()
			if tlsStartedAt.IsZero() {
				tlsStartedAt = time.Now()
			}
			traceMu.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, traceErr error) {
			traceMu.Lock()
			tlsDone = true
			tlsFailed = traceErr != nil
			if !tlsStartedAt.IsZero() && tlsMS == 0 {
				tlsMS = max(1, int(time.Since(tlsStartedAt).Milliseconds()))
			}
			traceMu.Unlock()
		},
		GotFirstResponseByte: func() {
			traceMu.Lock()
			gotFirstByte = true
			if firstByteMS == 0 {
				firstByteMS = max(1, int(time.Since(startedAt).Milliseconds()))
			}
			traceMu.Unlock()
		},
	})
	stage = "build_request"
	request, err := http.NewRequestWithContext(probeCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		result.Error = "构造探测请求失败"
		return result, err
	}
	request.Header.Set("User-Agent", DefaultUserAgent)
	stage = "execute_request"
	response, err := client.Do(request)
	if err != nil {
		traceMu.Lock()
		switch {
		case !connectStartedAt.IsZero() && (!connectDone || connectFailed):
			stage = "connect"
		case !tlsStartedAt.IsZero() && (!tlsDone || tlsFailed):
			stage = "tls"
		case (connectDone || tlsDone) && !gotFirstByte:
			stage = "first_byte"
		default:
			stage = "execute_request"
		}
		traceMu.Unlock()
		result.Error = "代理连接失败"
		return result, err
	}
	statusCode = response.StatusCode
	defer func() { _ = response.Body.Close() }()
	stage = "read_response"
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	if readErr != nil {
		result.Error = "读取探测响应失败"
		return result, readErr
	}
	stage = "validate_status"
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		result.Error = fmt.Sprintf("探测服务返回 HTTP %d", response.StatusCode)
		return result, errors.New(result.Error)
	}
	stage = "decode_response"
	exitIP, err := decodeProbeIP(body)
	if err != nil {
		result.Error = "探测服务响应格式无效"
		return result, err
	}
	stage = "validate_exit_ip"
	address, err := netip.ParseAddr(exitIP)
	if err != nil || (family == "ipv4" && !address.Is4()) || (family == "ipv6" && !address.Is6()) {
		result.Error = fmt.Sprintf("探测服务未返回有效 %s 出口 IP", strings.ToUpper(family))
		if err == nil {
			err = errors.New(result.Error)
		}
		return result, err
	}
	result.Status = domain.ProbeStatusHealthy
	result.LatencyMS = max(1, int(time.Since(startedAt).Milliseconds()))
	result.ExitIP = address.String()
	result.Error = ""
	stage = "complete"
	return result, nil
}

func decodeProbeIP(body []byte) (string, error) {
	var payload struct {
		IP string `json:"ip"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.IP) != "" {
		return strings.TrimSpace(payload.IP), nil
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if found && key == "ip" && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value), nil
		}
	}
	return "", errors.New("probe response does not contain an IP address")
}

// managedClearanceMode 报告当前 Clearance 是否为托管模式(flaresolverr/
// on_demand)。池路由与固定目标路由共用这一判定,保证同一作用域的
// Clearance 行为不因出口形态不同而分叉。
func (m *Manager) managedClearanceMode() bool {
	mode := m.clearanceMode()
	return mode == "flaresolverr" || mode == "on_demand"
}

func (m *Manager) acquire(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string) (*Lease, bool, error) {
	now := time.Now().UTC()
	managedClearance := usesBrowserClearance(scope) && m.managedClearanceMode()
	// 质量验证(canary)钉住受检节点:绕过路由层, 且由 acquireFixedTarget 的
	// verification 分支绕过冷却/排除守卫(见其注释)。
	if verificationNode := qualityVerificationNodeFromContext(ctx); verificationNode != 0 {
		lease, err := m.acquireFixedTarget(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, verificationNode, true)
		if err != nil {
			return nil, true, err
		}
		return lease, true, nil
	}
	if pinned := pinnedNodeFromContext(ctx); pinned != 0 {
		lease, err := m.acquireFixedTarget(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, pinned, false)
		if err != nil {
			return nil, true, err
		}
		return lease, true, nil
	}
	config, supported, configErr := m.loadOperationsConfig(ctx, now)
	if configErr != nil {
		return nil, false, fmt.Errorf("读取出口路由配置: %w", configErr)
	}
	target := domain.RoutingTarget{Mode: domain.RoutingTargetAuto}
	if supported {
		target = config.TargetFor(scope, TrafficClassFromContext(ctx))
	}
	// 统计按"实际作出决策的层级"归因:类别规则命中记 class:*,否则作用域
	// 规则命中记 scope:*,再否则记 default。未配置任何规则时走自动调度,
	// 不产生统计(徽标本身已说明)。
	level := "default"
	ruleConfigured := false
	if supported {
		level, ruleConfigured = decidingRoutingLevel(config, scope, TrafficClassFromContext(ctx))
	}
	// 路由层级解析：语义(流量类别) → 作用域 → 总出口 → 自动调度。「回退」
	// 只发生在配置阶梯的降级(更具体层级未配置时落到下一层级);一旦某层级
	// 配置了明确目标,该目标就是强绑定:固定节点不可用或代理池整体失效时
	// 快速失败,绝不静默改道到边界外的节点——账号出口 IP 的无声突变本身就是
	// 风险。需要容错应配置代理池:池是 any-of 契约,成员轮换/链式池/池内
	// 直连回退都在配置边界之内。
	switch target.Mode.Normalized() {
	case domain.RoutingTargetDirect:
		// 直连是显式路由决策，无需 allowDirect —— 与旧版直连路由规则一致。
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, domain.Node{ID: 0, Name: "route-direct", Enabled: true, Health: 1})
		if err != nil {
			return nil, true, fmt.Errorf("获取直连出口: %w", err)
		}
		if ruleConfigured {
			RecordRoutingOutcome(level, target, RoutingOutcomeHit)
		}
		return lease, true, nil
	case domain.RoutingTargetNode:
		lease, err := m.acquireFixedTarget(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, target.NodeID, false)
		if err == nil {
			if ruleConfigured {
				RecordRoutingOutcome(level, target, RoutingOutcomeHit)
			}
			return lease, true, nil
		}
		if !errors.Is(err, ErrRoutingTargetUnavailable) {
			return nil, true, err
		}
		// 固定节点目标=强绑定:节点被质量守卫隔离/冷却/停用而不可用时快速
		// 失败,让操作者立即看到配置失效,而不是流量悄悄改道其它出口。回退
		// 计数如实记录「配置的出口没有接住流量」。需要容错请配置代理池。
		if ruleConfigured {
			RecordRoutingOutcome(level, target, RoutingOutcomeFallback)
		}
		m.log().Warn("egress_strict_target_unavailable", "level", level, "node_id", target.NodeID, "error", err.Error())
		return nil, true, fmt.Errorf("路由固定出口不可用(严格绑定,不自动改道): %w", err)
	case domain.RoutingTargetPool:
		// 池的 direct 回退是降级而非主路由决策(与显式 direct 路由不同),
		// 必须遵守调用方的 allowDirect 契约:AcquireIfConfigured 不接受
		// manager 直连租约,否则会绕过调用方 fallback transport 的
		// HTTP_PROXY 语义。
		lease, outcome, err := m.AcquirePoolRouted(ctx, scope, affinity, target.PoolID, allowDirect, encryptedCredentialCookies)
		if err != nil && lease != nil {
			// 防御:AcquirePoolRouted 的 direct 回退分支可能同时透传租约与错误,
			// 先释放租约再失败,避免 inflight 计数泄漏。
			lease.Release()
		}
		if err != nil {
			if ctx.Err() != nil {
				// 请求已取消:不得为死请求租约节点、抬高 inflight 计数
				// 并触发无意义的健康反馈。
				return nil, true, ctx.Err()
			}
			// 池路由读失败(DB 抖动)同样严格失败:配置了池目标就是圈定了
			// 出口边界,静默退回自动调度等于流量无声逃出边界(且 DB 故障时
			// 自动调度的节点列表读取也会失败,回退并不能换来可用性)。
			if !errors.Is(err, context.Canceled) {
				m.log().Warn("egress_pool_route_failed", "pool_id", target.PoolID, "error", err.Error())
			}
			if ruleConfigured {
				RecordRoutingOutcome(level, target, RoutingOutcomeFallback)
			}
			return nil, true, fmt.Errorf("%w 路由代理池不可用(严格绑定,不自动改道): %v", ErrRoutingTargetUnavailable, err)
		}
		if lease != nil && outcome != PoolRouteNone {
			// 只有目标池自身选出成员才算命中;链式回退池/回退直连是配置边界
			// 之内的降级,记 Fallback 让行内统计如实反映"目标池没有亲自接住
			// 流量"。
			if ruleConfigured {
				outcomeKind := RoutingOutcomeFallback
				if outcome == PoolRouteMember {
					outcomeKind = RoutingOutcomeHit
				}
				RecordRoutingOutcome(level, target, outcomeKind)
			}
			return lease, true, nil
		}
		// 池整体未产出租约(池被删除/停用、成员全部冷却且无可用的链式/直连
		// 回退):严格失败。自动调度里的节点不在管理员圈定的边界内,静默改道
		// 与固定节点不可用改道是同一种意外。
		if ruleConfigured {
			RecordRoutingOutcome(level, target, RoutingOutcomeFallback)
		}
		return nil, true, fmt.Errorf("%w 路由代理池 %d 未产出出口(严格绑定,不自动改道): 池不存在/停用或全部成员不可用", ErrRoutingTargetUnavailable, target.PoolID)
	}
	// 自动调度：所有未入池的启用节点按健康度与调用方亲和选择。
	nodes, err := m.listNodes(ctx, now)
	if err != nil {
		return nil, false, err
	}
	available := make([]domain.Node, 0, len(nodes))
	hasNodes := false
	for _, node := range nodes {
		if !node.Enabled {
			continue
		}
		if nodeExcluded(ctx, node.ID) {
			continue
		}
		proxyPool := m.snapshotProxyPoolFlag(node)
		// 代理池模式成员豁免 L2 软冷却:旋转端点的单次降智不代表端点坏,
		// 仅保留请求内排除(L1)与既有硬冷却豁免。
		if !proxyPool && m.nodeSoftCooled(node.ID, now) {
			continue
		}
		// 自动调度是全量兜底池:节点是纯资源,入池只是分组,
		// 不把节点从自动调度里"消费"掉——否则建池会让兜底容量缩水。
		hasNodes = true
		if node.CooldownUntil == nil || !now.Before(*node.CooldownUntil) || proxyPool {
			if proxyPool {
				node.Health, node.FailureCount, node.CooldownUntil, node.LastError = 1, 0, nil, ""
			}
			available = append(available, node)
		}
	}
	if len(available) == 0 {
		if hasNodes {
			return nil, false, fmt.Errorf("当前没有可用的出口节点")
		}
		if !allowDirect {
			recordSelection(ctx, Selection{NodeName: "direct", Scope: scope})
			return nil, false, nil
		}
		available = []domain.Node{{ID: 0, Name: "direct", Enabled: true, Health: 1}}
	}
	sort.SliceStable(available, func(i, j int) bool { return available[i].ID < available[j].ID })
	selected := m.selectNode(available, affinity)
	return m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
}

// ErrRoutingTargetUnavailable reports that a configured fixed routing
// target cannot currently serve the request. Callers fail fast: a configured
// target is a strict binding, never silently rerouted to other exits.
var ErrRoutingTargetUnavailable = errors.New("egress routing target unavailable")

// acquireFixedTarget leases one fixed routing-target node. It uses the
// cached target lookup so rule hits do not turn into a DB round trip per
// request, and waits for an in-flight failure probe like the automatic
// path so a transport hiccup does not immediately degrade the route.
func (m *Manager) acquireFixedTarget(ctx context.Context, scope domain.Scope, affinity, encryptedCredentialCookies string, managedClearance bool, nodeID uint64, verification bool) (*Lease, error) {
	waitedForProbe := false
	for {
		selected, ok, err := m.cachedRoutingTargetNode(ctx, nodeID)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, fmt.Errorf("%w: node %d not found", ErrRoutingTargetUnavailable, nodeID)
		}
		if !domain.CanNodeServeFixedTarget(selected) {
			return nil, fmt.Errorf("%w: node %d not schedulable", ErrRoutingTargetUnavailable, selected.ID)
		}
		// 质量验证模式(canary 钉住):被验证节点必然处于质量隔离冷却中,
		// L2 软冷却也可能仍在生效(它们在验证通过/暂定放行时才被清除);
		// 跳过排除与冷却检查直接取租约, 否则"验证通过→解除隔离"的回池
		// 链路整体失效。非验证路径(路由固定目标/降智同号重试)不受影响。
		if verification {
			lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrRoutingTargetUnavailable, err)
			}
			return lease, nil
		}
		// 请求内排除(降智守卫)对固定目标同样生效:重试必须离开坏出口,
		// 否则守卫对固定路由配置完全失效。以不可用告终,调用方快速失败。
		if nodeExcluded(ctx, selected.ID) {
			return nil, fmt.Errorf("%w: node %d excluded by degrade guard", ErrRoutingTargetUnavailable, nodeID)
		}
		if !m.isProxyPoolNode(selected) && selected.CooldownUntil != nil && time.Now().UTC().Before(*selected.CooldownUntil) {
			if !waitedForProbe && selected.LastError == domain.LastErrorTransport {
				completed, waitErr := m.waitForFailureProbe(ctx, nodeID)
				if waitErr != nil {
					return nil, waitErr
				}
				if completed {
					waitedForProbe = true
					continue
				}
			}
			return nil, fmt.Errorf("%w: node %d cooling down", ErrRoutingTargetUnavailable, nodeID)
		}
		// L2 未决软冷却同样使固定目标不可用:降智证据尚未归因,继续命中只会
		// 重复撞坏出口;以不可用告终,由调用方快速失败。池模式(旋转)节点
		// 豁免——与上方硬冷却豁免及自动调度口径一致:单个坏 IP 不代表端点坏。
		if !m.isProxyPoolNode(selected) && m.nodeSoftCooled(nodeID, time.Now().UTC()) {
			return nil, fmt.Errorf("%w: node %d pending degrade evidence", ErrRoutingTargetUnavailable, nodeID)
		}
		lease, _, err := m.leaseForNode(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrRoutingTargetUnavailable, err)
		}
		return lease, nil
	}
}

func (m *Manager) loadOperationsConfig(ctx context.Context, now time.Time) (domain.OperationsConfig, bool, error) {
	configRepository, ok := m.repository.(operationsConfigRepository)
	if !ok {
		return domain.OperationsConfig{}, false, nil
	}
	m.operationsMu.RLock()
	cached := m.operationsConfig
	m.operationsMu.RUnlock()
	if !cached.expiresAt.IsZero() && now.Before(cached.expiresAt) {
		return cached.value, true, nil
	}
	loaded, err, _ := m.operationsConfigLoad.Do("operations", func() (any, error) {
		checkTime := time.Now().UTC()
		m.operationsMu.RLock()
		cached := m.operationsConfig
		m.operationsMu.RUnlock()
		if !cached.expiresAt.IsZero() && checkTime.Before(cached.expiresAt) {
			return cached.value, nil
		}
		m.operationsMu.RLock()
		version := m.operationsConfigVer
		m.operationsMu.RUnlock()
		// 同 listNodes:脱离领头调用方生命周期, 避免取消连坐合并等待者。
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		value, err := configRepository.GetEgressOperationsConfig(loadCtx)
		if err != nil {
			return domain.OperationsConfig{}, err
		}
		m.operationsMu.Lock()
		if version == m.operationsConfigVer {
			m.operationsConfig = cachedOperationsConfig{value: value, expiresAt: checkTime.Add(operationsConfigSnapshotTTL)}
		}
		m.operationsMu.Unlock()
		return value, nil
	})
	if err != nil {
		return domain.OperationsConfig{}, true, err
	}
	return loaded.(domain.OperationsConfig), true, nil
}

type clientOptions struct {
	buildEnvironmentProxy   bool
	requireAccountIsolation bool
}

func (m *Manager) leaseForNode(ctx context.Context, scope domain.Scope, affinity, encryptedCredentialCookies string, managedClearance bool, selected domain.Node) (*Lease, bool, error) {
	return m.leaseForNodeWithOptions(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected, clientOptions{})
}

func (m *Manager) leaseForNodeWithOptions(ctx context.Context, scope domain.Scope, affinity, encryptedCredentialCookies string, managedClearance bool, selected domain.Node, options clientOptions) (*Lease, bool, error) {
	credentialCookies := ""
	if !managedClearance && usesBrowserClearance(scope) && strings.TrimSpace(encryptedCredentialCookies) != "" {
		decryptedCookies, decryptErr := m.cipher.Decrypt(encryptedCredentialCookies)
		if decryptErr != nil {
			return nil, true, decryptErr
		}
		credentialCookies = cfcookies.Sanitize(decryptedCookies)
	}
	proxyURL, err := m.cipher.Decrypt(selected.EncryptedProxyURL)
	if err != nil {
		return nil, false, err
	}
	proxyURL, err = proxyurl.NormalizeProxyURL(proxyURL)
	if err != nil {
		return nil, false, err
	}
	sticky := domain.IsAccountTemplateProxy(proxyURL)
	proxyPool := selected.ProxyPool || sticky
	freshTunnel := selected.ProxyPool && !sticky
	if sticky {
		accountKey := accountFromContext(ctx)
		if accountKey == "" && strings.TrimSpace(affinity) != "" {
			accountKey = string(scope) + "_" + strings.TrimSpace(affinity)
		}
		proxyURL, err = renderAccountProxyURL(proxyURL, accountKey)
		if err != nil {
			return nil, false, err
		}
	}
	cookies := ""
	if usesBrowserClearance(scope) {
		if credentialCookies != "" {
			// 账号自带 cookie 必然覆盖节点 cookie,不必先解密再丢弃。
			cookies = credentialCookies
		} else {
			cookies, err = m.cipher.Decrypt(selected.EncryptedCloudflareCookie)
			if err != nil {
				// Managed mode can recover a damaged persisted cookie by asking the
				// solver for a fresh one. Manual mode must still surface the storage
				// error because it has no safe replacement source.
				if !managedClearance {
					return nil, false, err
				}
				cookies = ""
			}
			cookies = cfcookies.Sanitize(cookies)
		}
	}
	userAgent := ""
	if scope != domain.ScopeBuild {
		userAgent = strings.TrimSpace(selected.UserAgent)
	}
	if scope != domain.ScopeBuild && userAgent == "" {
		userAgent = DefaultUserAgent
	}
	clearanceKey := ""
	// Manual mode may prefer account-bound cookies. Managed mode always enters
	// the FlareSolverr lifecycle so stale imported cookies cannot bypass refresh.
	if managedClearance {
		clearanceKey = clearanceCacheKey(selected.ID, proxyURL, sticky)
		cookies, userAgent, err = m.ensureClearance(ctx, selected, proxyURL, cookies, userAgent, clearanceKey, !sticky)
		if err != nil {
			return nil, false, err
		}
	}
	// Derive identity independently of the current toggle. clientFor applies one
	// authoritative toggle snapshot, so enabling isolation between these two
	// stages cannot accidentally place an account request in the shared bucket.
	accountIdentity := ""
	if scope != domain.ScopeConsoleAsset {
		accountIdentity = isolationAccountIdentity(ctx, scope, affinity)
	}
	client, err := m.clientForWithOptions(selected.ID, scope, proxyURL, userAgent, cookies, sticky, accountIdentity, options)
	if err != nil {
		return nil, false, err
	}
	m.incrementInflight(selected.ID)
	recordSelection(ctx, Selection{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, Proxied: proxyURL != ""})
	var once sync.Once
	return &Lease{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, ProxyURL: proxyURL, UserAgent: userAgent, CFCookies: cookies, client: client.client, browser: client.browser, sticky: sticky, proxyPool: proxyPool, freshTunnel: freshTunnel, clearanceKey: clearanceKey, clearanceManager: m, release: func() {
		once.Do(func() {
			m.decrementInflight(selected.ID)
		})
	}}, true, nil
}

// Console assets are served from public media hosts. They still need the
// selected proxy and browser user agent, but forwarding account or node
// clearance cookies would unnecessarily expose credentials to a different
// origin and make an otherwise anonymous download depend on cookie storage.
func usesBrowserClearance(scope domain.Scope) bool {
	return scope != domain.ScopeBuild && scope != domain.ScopeConsoleAsset
}

func (m *Manager) inflightCounter(nodeID uint64) *atomic.Int64 {
	// Counters remain address-stable for the manager lifetime so a concurrent
	// release can never decrement a replacement counter after an ABA deletion.
	if value, ok := m.inflight.Load(nodeID); ok {
		return value.(*atomic.Int64)
	}
	candidate := &atomic.Int64{}
	actual, _ := m.inflight.LoadOrStore(nodeID, candidate)
	return actual.(*atomic.Int64)
}

func (m *Manager) incrementInflight(nodeID uint64) {
	m.inflightCounter(nodeID).Add(1)
}

func (m *Manager) decrementInflight(nodeID uint64) {
	if value, ok := m.inflight.Load(nodeID); ok {
		value.(*atomic.Int64).Add(-1)
	}
}

func clearanceCacheKey(nodeID uint64, proxyURL string, sticky bool) string {
	if nodeID == 0 {
		return "direct"
	}
	base := "node:" + strconv.FormatUint(nodeID, 10)
	if !sticky {
		return base
	}
	digest := sha256.Sum256([]byte(proxyURL))
	return base + ":account:" + fmt.Sprintf("%x", digest[:16])
}

func renderAccountProxyURL(template, accountKey string) (string, error) {
	if !domain.IsAccountTemplateProxy(template) {
		return template, nil
	}
	accountKey = normalizeProxyAccount(accountKey)
	if accountKey == "" {
		return "", errors.New("粘性代理需要有效的账号身份")
	}
	return strings.ReplaceAll(template, domain.ProxyAccountPlaceholder, accountKey), nil
}

func normalizeProxyAccount(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.Map(func(character rune) rune {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '-' {
			return character
		}
		return '_'
	}, value)
	if len(value) <= 128 {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	return value[:95] + "_" + fmt.Sprintf("%x", digest[:16])
}

// nodeSnapshotKey is the single global node-snapshot cache key: nodes are
// scope-free resources, so one snapshot serves every request family.
const nodeSnapshotKey = "nodes"

func (m *Manager) listNodes(ctx context.Context, now time.Time) ([]domain.Node, error) {
	for {
		m.nodeMu.RLock()
		if snapshot, ok := m.nodes[nodeSnapshotKey]; ok && now.Before(snapshot.expiresAt) {
			// Node snapshots are replaced with a copied slice and treated as immutable
			// by callers. Returning the shared read-only slice avoids a per-request copy.
			values := snapshot.values
			m.nodeMu.RUnlock()
			return values, nil
		}
		m.nodeMu.RUnlock()
		loaded, err, _ := m.nodeLoads.Do(nodeSnapshotKey, func() (any, error) {
			checkTime := time.Now().UTC()
			m.nodeMu.RLock()
			if snapshot, ok := m.nodes[nodeSnapshotKey]; ok && checkTime.Before(snapshot.expiresAt) {
				values := snapshot.values
				m.nodeMu.RUnlock()
				return values, nil
			}
			version := m.nodeVersions[nodeSnapshotKey]
			m.nodeMu.RUnlock()
			// 脱离领头调用方的请求生命周期:singleflight 合并的所有等待者共享这次
			// 回源, 领头者断开不应把 DB 读取消连坐给它们(等待者自身的取消仍由
			// 各自的 select 处理)。5s 足够覆盖一次快照查询。
			loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			values, err := m.repository.ListEgressNodes(loadCtx, repository.SortQuery{})
			if err != nil {
				return nil, err
			}
			m.nodeMu.Lock()
			if m.nodeVersions[nodeSnapshotKey] != version {
				m.nodeMu.Unlock()
				return nil, errNodeSnapshotInvalidated
			}
			m.replaceNodeSnapshotLocked(values, checkTime.Add(nodeSnapshotTTL))
			values = m.nodes[nodeSnapshotKey].values
			m.nodeMu.Unlock()
			return values, nil
		})
		if err != nil {
			if errors.Is(err, errNodeSnapshotInvalidated) && ctx.Err() == nil {
				now = time.Now().UTC()
				continue
			}
			return nil, err
		}
		return loaded.([]domain.Node), nil
	}
}

func (m *Manager) invalidateNodes() {
	m.nodeMu.Lock()
	m.nodeVersions[nodeSnapshotKey]++
	snapshot, ok := m.nodes[nodeSnapshotKey]
	if ok {
		delete(m.nodes, nodeSnapshotKey)
	}
	for _, node := range snapshot.values {
		delete(m.healthyNodes, node.ID)
	}
	m.nodeMu.Unlock()
	// Routing-target node caching lives outside the node snapshot; drop it
	// whenever node state changes so a disabled or deleted target stops
	// serving immediately.
	m.routeRuleNodeMu.Lock()
	m.routeRuleNodeCache = make(map[uint64]cachedRoutingTargetNode)
	m.routeRuleNodeMu.Unlock()
}

func (m *Manager) replaceNodeSnapshotLocked(values []domain.Node, expiresAt time.Time) {
	for _, node := range m.nodes[nodeSnapshotKey].values {
		delete(m.healthyNodes, node.ID)
	}
	poolFlags := make(map[uint64]bool, len(values))
	for _, node := range values {
		if nodeIsHealthy(node) {
			m.healthyNodes[node.ID] = expiresAt
		} else {
			delete(m.healthyNodes, node.ID)
		}
		poolFlags[node.ID] = m.isProxyPoolNodeDirect(node)
	}
	m.nodes[nodeSnapshotKey] = cachedNodeSnapshot{values: append([]domain.Node(nil), values...), poolFlags: poolFlags, expiresAt: expiresAt}
	m.sweepDeletedInflightLocked(values)
}

// sweepDeletedInflightLocked 清理"已不存在且计数为零"的节点 inflight 计数
// 条目。计数器永不删除是为避免 ABA(并发 release 对替换计数器递减), 但订阅
// 换血会持续产生新节点 ID, 零散条目无界累积——与 poolNodeStats 容量逐出
// 同类。只删 [快照中不存在 && count==0] 的条目:
//   - count>0 说明仍有删除前创建的租约在途, 保留避免丢计数;
//   - 对已删条目的迟来递减已被 decrementInflight 的 Load 守卫吞掉;
//   - 节点 ID 不复用(autoincrement), 不存在 ABA 复活。
func (m *Manager) sweepDeletedInflightLocked(values []domain.Node) {
	live := make(map[uint64]struct{}, len(values))
	for _, node := range values {
		live[node.ID] = struct{}{}
	}
	m.inflight.Range(func(key, value any) bool {
		nodeID, ok := key.(uint64)
		if !ok {
			return true
		}
		if _, exists := live[nodeID]; exists {
			return true
		}
		if value.(*atomic.Int64).Load() != 0 {
			return true
		}
		m.inflight.Delete(nodeID)
		return true
	})
}

func (m *Manager) InvalidateOperationsConfig() {
	m.operationsMu.Lock()
	m.operationsConfig = cachedOperationsConfig{}
	m.operationsConfigVer++
	m.operationsMu.Unlock()
}

func (m *Manager) selectNode(nodes []domain.Node, affinity string) domain.Node {
	if affinity != "" {
		digest := sha256.Sum256([]byte(affinity))
		selected := nodes[int(binary.BigEndian.Uint64(digest[:8])%uint64(len(nodes)))]
		if selected.Health >= 0.8 || len(nodes) == 1 {
			return selected
		}
		for _, node := range nodes {
			if node.Health > selected.Health {
				selected = node
			}
		}
		return selected
	}
	best := nodes[0]
	bestCurrent := m.inflightCount(best.ID)
	for _, node := range nodes[1:] {
		current := m.inflightCount(node.ID)
		if current < bestCurrent || (current == bestCurrent && node.Health > best.Health) {
			best = node
			bestCurrent = current
		}
	}
	return best
}

func (m *Manager) inflightCount(nodeID uint64) int64 {
	if value, ok := m.inflight.Load(nodeID); ok {
		return value.(*atomic.Int64).Load()
	}
	return 0
}

func (m *Manager) clientFor(id uint64, scope domain.Scope, proxyURL, userAgent, cookies string, sticky bool, accountIdentity string) (cachedClient, error) {
	return m.clientForWithOptions(id, scope, proxyURL, userAgent, cookies, sticky, accountIdentity, clientOptions{})
}

func (m *Manager) clientForWithOptions(id uint64, scope domain.Scope, proxyURL, userAgent, cookies string, sticky bool, accountIdentity string, options clientOptions) (cachedClient, error) {
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
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(clientKind+"\x00"+proxyURL+"\x00"+userAgent+"\x00"+cookies)))
	cacheScope := scope
	if cacheScope == domain.ScopeWebAsset {
		cacheScope = domain.ScopeWeb
	}
	for attempt := 0; attempt < clientCreationRetryLimit; attempt++ {
		isolated := m.accountIsolated.Load()
		if options.requireAccountIsolation && !isolated {
			return cachedClient{}, errAccountConnectionIsolationDisabled
		}
		keyAccountIdentity := ""
		if isolated {
			keyAccountIdentity = strings.TrimSpace(accountIdentity)
			if keyAccountIdentity == "" {
				keyAccountIdentity = "shared"
			}
		}
		key := clientCacheKey{nodeID: id, scope: cacheScope, fingerprint: fingerprint, accountIdentity: keyAccountIdentity}
		loadKey := strconv.FormatUint(key.nodeID, 10) + "\x00" + string(key.scope) + "\x00" + key.fingerprint + "\x00" + key.accountIdentity
		now := time.Now().UTC()
		m.clientMu.RLock()
		cached, cachedOK := m.clients[key]
		cleanupDue := m.lastClientCleanup.IsZero() || now.Sub(m.lastClientCleanup) >= clientCacheCleanupInterval
		touchDue := cachedOK && (cached.lastUsed.IsZero() || now.Sub(cached.lastUsed) >= clientCacheTouchInterval)
		m.clientMu.RUnlock()
		if cachedOK && !cleanupDue && !touchDue {
			return cached, nil
		}

		m.clientMu.Lock()
		stale := m.cleanupClientCacheLocked(now)
		if cached, ok := m.clients[key]; ok {
			cached.lastUsed = now
			m.clients[key] = cached
			m.clientMu.Unlock()
			closeRequestClients(stale)
			return cached, nil
		}
		m.clientMu.Unlock()
		closeRequestClients(stale)

		loaded, err, _ := m.clientLoads.Do(loadKey, func() (any, error) {
			return m.createAndCacheClient(key, id, scope, proxyURL, userAgent, sticky, buildHeaderTimeout, options)
		})
		if errors.Is(err, errClientCacheInvalidated) {
			continue
		}
		if err != nil {
			return cachedClient{}, err
		}
		return loaded.(cachedClient), nil
	}
	return cachedClient{}, errClientCacheInvalidated
}

func (m *Manager) createAndCacheClient(key clientCacheKey, id uint64, scope domain.Scope, proxyURL, userAgent string, sticky bool, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	now := time.Now().UTC()
	m.clientMu.Lock()
	stale := m.cleanupClientCacheLocked(now)
	if (key.accountIdentity != "") != m.accountIsolated.Load() {
		m.clientMu.Unlock()
		closeRequestClients(stale)
		return cachedClient{}, errClientCacheInvalidated
	}
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = now
		m.clients[key] = cached
		m.clientMu.Unlock()
		closeRequestClients(stale)
		return cached, nil
	}
	version := m.clientVersionLocked(id)
	m.clientMu.Unlock()
	closeRequestClients(stale)

	value, err := m.buildCachedClient(scope, proxyURL, userAgent, buildHeaderTimeout, options)
	if err != nil {
		return cachedClient{}, err
	}
	value.lastUsed = time.Now().UTC()

	m.clientMu.Lock()
	stale = m.cleanupClientCacheLocked(value.lastUsed)
	if (key.accountIdentity != "") != m.accountIsolated.Load() {
		m.clientMu.Unlock()
		closeRequestClients(append(stale, value.client))
		return cachedClient{}, errClientCacheInvalidated
	}
	if cached, ok := m.clients[key]; ok {
		cached.lastUsed = value.lastUsed
		m.clients[key] = cached
		m.clientMu.Unlock()
		closeRequestClients(append(stale, value.client))
		return cached, nil
	}
	if m.clientVersionLocked(id) != version {
		m.clientMu.Unlock()
		closeRequestClients(append(stale, value.client))
		return cachedClient{}, errClientCacheInvalidated
	}
	if id != 0 && !sticky {
		for previousKey, previous := range m.clients {
			if previousKey.nodeID != id || previousKey.scope != key.scope {
				continue
			}
			// Keep other accounts' pools when isolation is on.
			if key.accountIdentity != "" && previousKey.accountIdentity != key.accountIdentity {
				continue
			}
			stale = append(stale, m.evictClientLocked(previousKey, previous))
		}
	}
	stale = append(stale, m.ensureClientCacheCapacityLocked()...)
	m.clients[key] = value
	m.clientMu.Unlock()
	closeRequestClients(stale)
	return value, nil
}

func (m *Manager) buildCachedClient(scope domain.Scope, proxyURL, userAgent string, buildHeaderTimeout time.Duration, options clientOptions) (cachedClient, error) {
	if scope == domain.ScopeBuild {
		if options.buildEnvironmentProxy {
			factory := m.newBuildEnvClient
			if factory == nil {
				factory = newBuildEnvironmentRequestClient
			}
			client, err := factory(buildHeaderTimeout)
			if err != nil {
				return cachedClient{}, err
			}
			return cachedClient{client: client}, nil
		}
		factory := m.newBuildClient
		if factory == nil {
			factory = newBuildRequestClient
		}
		client, err := factory(proxyURL, buildHeaderTimeout)
		if err != nil {
			return cachedClient{}, err
		}
		return cachedClient{client: client}, nil
	}
	factory := m.newBrowserClient
	if factory == nil {
		factory = newBrowserClient
	}
	client, err := factory(proxyURL, userAgent)
	if err != nil {
		return cachedClient{}, err
	}
	return cachedClient{client: client, browser: client}, nil
}

func newBuildRequestClient(proxyURL string, responseHeaderTimeout time.Duration) (requestClient, error) {
	return newBuildClient(proxyURL, responseHeaderTimeout)
}

func newBuildEnvironmentRequestClient(responseHeaderTimeout time.Duration) (requestClient, error) {
	return newBuildEnvironmentClient(responseHeaderTimeout)
}

func (m *Manager) cleanupClientCacheLocked(now time.Time) []requestClient {
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

func (m *Manager) ensureClientCacheCapacityLocked() []requestClient {
	var stale []requestClient
	for len(m.clients) >= maxCachedClients {
		var oldestKey clientCacheKey
		var oldest cachedClient
		found := false
		for key, value := range m.clients {
			if !found || value.lastUsed.Before(oldest.lastUsed) {
				oldestKey, oldest, found = key, value, true
			}
		}
		if !found {
			break
		}
		stale = append(stale, m.evictClientLocked(oldestKey, oldest))
	}
	return stale
}

func (m *Manager) evictClientLocked(key clientCacheKey, value cachedClient) requestClient {
	delete(m.clients, key)
	return value.client
}

func closeRequestClients(values []requestClient) {
	for _, value := range values {
		if value != nil {
			value.CloseIdleConnections()
		}
	}
}

func (m *Manager) clientVersionLocked(nodeID uint64) uint64 {
	return m.clientGeneration + m.clientVersions[nodeID]
}

func (m *Manager) invalidateClientVersionLocked(nodeID uint64) {
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

func (m *Manager) invalidateAllClientVersionsLocked() {
	m.clientGeneration++
	clear(m.clientVersions)
}

// QuarantineNodeForQuality marks a fixed egress node's exit IP as
// quality-degraded: the degraded account's RSC attribution came back clean, so
// the exit IP is the suspect. The node is cooled out of rotation until the
// given deadline (normally the quarantine cooldown, possibly shortened once a
// rotation succeeded and verification passed). Pool nodes are skipped — their
// exit IP is not fixed, so there is nothing to quarantine. The returned node
// is the pre-quarantine snapshot so callers can compare the old ExitIP.
func (m *Manager) QuarantineNodeForQuality(ctx context.Context, nodeID uint64, until time.Time) (domain.Node, error) {
	if m == nil || nodeID == 0 {
		return domain.Node{}, nil
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return domain.Node{}, err
	}
	previous := value
	if m.isProxyPoolNode(value) {
		return previous, nil
	}
	now := time.Now().UTC()
	value.FailureCount++
	RecordPoolNodeFailure(value.ID)
	value.Health = max(0.05, value.Health*0.5)
	cooldown := until
	value.CooldownUntil = &cooldown
	value.LastError = domain.LastErrorExitIPQuality
	value.DegradeCount++
	value.LastDegradedAt = &now
	// 新隔离周期重置换 IP 尝试计数(保留 lastRotatedAt 以维持最小间隔护栏):
	// 否则上一周期耗尽(attempts==max)的节点再次被隔离时永远不会轮换。
	if rotationRepo, ok := m.repository.(interface {
		UpdateEgressNodeRotationState(context.Context, uint64, *time.Time, int, string) error
	}); ok {
		_ = rotationRepo.UpdateEgressNodeRotationState(ctx, value.ID, value.LastRotatedAt, 0, "")
	} else if stateRepo, ok := m.repository.(egressStateRepository); ok {
		_ = stateRepo.UpdateEgressNodeLastError(ctx, value.ID, value.LastError)
	}
	if qualityRepo, ok := m.repository.(egressQualityStateRepository); ok {
		if err := qualityRepo.UpdateEgressNodeQualityState(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError, value.DegradeCount, value.LastDegradedAt); err != nil {
			return domain.Node{}, err
		}
	} else if _, err := m.repository.UpdateEgressNode(ctx, value); err != nil {
		return domain.Node{}, err
	}
	m.invalidateNodes()
	return previous, nil
}

// CooldownNodeForQuality applies a plain quality cooldown without touching
// degrade counters or health. It backs tentative re-admission after an
// inconclusive canary verification: the node serves traffic again soon, and
// the passive guard can re-quarantine cheaply if the IP is still bad.
func (m *Manager) CooldownNodeForQuality(ctx context.Context, nodeID uint64, until time.Time) error {
	if m == nil || nodeID == 0 {
		return nil
	}
	m.ClearDegradeEvidence(nodeID)
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	value.Health = 1
	value.FailureCount = 0
	cooldown := until
	value.CooldownUntil = &cooldown
	if value.LastError == domain.LastErrorExitIPQuality {
		value.LastError = ""
	}
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err != nil {
			return err
		}
	} else if _, err := m.repository.UpdateEgressNode(ctx, value); err != nil {
		return err
	}
	m.invalidateNodes()
	return nil
}

// CooldownNodeForProbeFailure applies a transport cooldown to a node whose
// exit was confirmed dead by probes (both address families failing twice in
// a row). It mirrors the request-transport feedback branch so recovery
// semantics match exactly: UpdateEgressNodeProbe clears it on the next
// healthy probe, without any manual intervention.
func (m *Manager) CooldownNodeForProbeFailure(ctx context.Context, nodeID uint64, until time.Time) error {
	if m == nil || nodeID == 0 {
		return nil
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	value.FailureCount++
	value.Health = max(0.05, value.Health*0.7)
	cooldown := until
	value.CooldownUntil = &cooldown
	value.LastError = domain.LastErrorTransport
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err != nil {
			return err
		}
	} else if _, err := m.repository.UpdateEgressNode(ctx, value); err != nil {
		return err
	}
	m.invalidateNodes()
	return nil
}

// ReleaseQualityQuarantine re-admits a node whose rotated exit IP passed
// verification: quality state resets so the node competes normally again.
// A non-quality last error is preserved — it belongs to the transport layer.
func (m *Manager) ReleaseQualityQuarantine(ctx context.Context, nodeID uint64) error {
	if m == nil || nodeID == 0 {
		return nil
	}
	// 回池即清未决软冷却:canary 已证明新出口健康,残留的指数冷却证据
	// 只会无谓压低调度优先级。
	m.ClearDegradeEvidence(nodeID)
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return err
	}
	value.Health = 1
	value.FailureCount = 0
	value.CooldownUntil = nil
	if value.LastError == domain.LastErrorExitIPQuality {
		value.LastError = ""
	}
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err != nil {
			return err
		}
	} else if _, err := m.repository.UpdateEgressNode(ctx, value); err != nil {
		return err
	}
	m.invalidateNodes()
	return nil
}

func (m *Manager) Feedback(ctx context.Context, nodeID uint64, status int, transportErr error) {
	m.FeedbackForScope(ctx, domain.ScopeWeb, nodeID, status, transportErr)
}

func (m *Manager) FeedbackForScope(ctx context.Context, scope domain.Scope, nodeID uint64, status int, transportErr error) {
	if status == clientClosedRequestStatus || errors.Is(transportErr, context.Canceled) {
		return
	}
	// Console media hosts are public and do not use clearance credentials. A
	// 403 there commonly describes the object URL (expired, rejected, or
	// missing), not the proxy's ability to reach the origin, so it must not cool
	// or rotate an otherwise healthy primary Console node.
	if scope == domain.ScopeConsoleAsset && transportErr == nil && status == http.StatusForbidden {
		return
	}
	if neterrorpkg.IsUpstreamStreamIdleTimeout(transportErr) {
		return
	}
	if scope == domain.ScopeBuild && neterrorpkg.IsResponseHeaderTimeout(transportErr) {
		return
	}
	if nodeID == 0 {
		if transportErr != nil || (scope != domain.ScopeBuild && status == http.StatusForbidden) {
			m.clearanceMu.Lock()
			if isGrokWebScope(scope) && status == http.StatusForbidden && m.clearanceConfig.Mode == "flaresolverr" {
				state := m.clearances["direct"]
				state.invalid = true
				state.used = true
				m.clearances["direct"] = state
			}
			m.clearanceMu.Unlock()
			m.clientMu.Lock()
			stale := m.invalidateClientForScopeLocked(0, scope)
			m.clientMu.Unlock()
			closeRequestClients(stale)
		}
		return
	}
	succeeded := transportErr == nil && status >= 200 && status < 400
	if succeeded && m.cachedNodeIsHealthy(nodeID) {
		return
	}
	value, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return
	}
	if succeeded && nodeIsHealthy(value) {
		m.nodeMu.Lock()
		m.healthyNodes[nodeID] = time.Now().UTC().Add(nodeSnapshotTTL)
		m.nodeMu.Unlock()
		return
	}
	now := time.Now().UTC()
	// 成功反馈不得终止出口质量隔离(exit_ip_quality):隔离可能晚于租约建立才
	// 落地(在途请求/多实例),一次偶然成功的请求无权否定跨账号确认或轮换
	// 中的坏 IP 判定——隔离解除只属于轮换验证/冷却到期/归因撤销三条显式
	// 路径。传输类失败状态照常恢复(它们只描述链路抖动,与质量隔离正交)。
	qualityQuarantined := value.LastError == domain.LastErrorExitIPQuality
	var stale []requestClient
	switch {
	case succeeded:
		if qualityQuarantined {
			// 保留隔离语义:只恢复传输性健康指标,冷却与质量标记原样。
			value.Health = min(1, value.Health+0.1)
			value.FailureCount = 0
			break
		}
		value.Health = min(1, value.Health+0.1)
		value.FailureCount = 0
		value.CooldownUntil = nil
		value.LastError = ""
	case status == http.StatusUnauthorized || status == http.StatusTooManyRequests:
		return
	case scope == domain.ScopeBuild && status == http.StatusForbidden:
		// Build 403 may indicate account permissions, quota, token, or egress policy. The gateway classifies the body;
		// status alone must not misclassify standard CLI egress as Web anti-bot behavior.
		return
	case scope == domain.ScopeBuild && status == http.StatusBadRequest:
		// Device OAuth polls with 400 + authorization_pending before user confirmation.
		// This is a normal protocol state and must not cool the egress node.
		return
	case status == http.StatusForbidden:
		if m.isProxyPoolNode(value) {
			// A request-level 403 does not prove that a shared proxy pool is unhealthy.
			// 健康惩罚豁免,但统计照记:验证策略需要看到失败次数。
			RecordPoolNodeFailure(value.ID)
			return
		}
		value.FailureCount++
		RecordPoolNodeFailure(value.ID)
		value.Health = max(0.05, value.Health*0.7)
		value.CooldownUntil = nil
		value.LastError = "anti-bot rejection"
		m.clearanceMu.Lock()
		if isGrokWebScope(scope) && m.clearanceConfig.Mode == "flaresolverr" {
			// 粘性租约的 Clearance 缓存在 node:N:account:<digest> 键下,
			// 反馈时无法得知账号摘要,必须按节点前缀整体失效,否则 403
			// 永远打不中 sticky 缓存,FlareSolverr 也不会触发刷新。
			m.invalidateNodeClearancesLocked(nodeID)
		}
		m.clearanceMu.Unlock()
		m.clientMu.Lock()
		stale = m.invalidateClientLocked(nodeID)
		m.clientMu.Unlock()
	case transportErr != nil:
		if m.isProxyPoolNode(value) {
			// 同上:共享代理传输失败不冷却,但计入统计。
			RecordPoolNodeFailure(value.ID)
			return
		}
		value.FailureCount++
		RecordPoolNodeFailure(value.ID)
		value.Health = max(0.05, value.Health*0.7)
		cooldown := min(10*time.Minute, 30*time.Second*time.Duration(1<<min(value.FailureCount-1, 4)))
		until := now.Add(cooldown)
		value.CooldownUntil = &until
		value.LastError = domain.LastErrorTransport
		m.clientMu.Lock()
		stale = m.invalidateClientLocked(nodeID)
		m.clientMu.Unlock()
	default:
		// An HTTP status describes the upstream response, not the health of the
		// configured proxy endpoint. Account routing handles upstream failures.
		return
	}
	closeRequestClients(stale)
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeHealth(ctx, value.ID, value.Health, value.FailureCount, value.CooldownUntil, value.LastError); err == nil {
			m.invalidateNodes()
			if transportErr != nil {
				m.scheduleFailureProbe(value)
			}
		}
		return
	}
	if _, err := m.repository.UpdateEgressNode(ctx, value); err == nil {
		m.invalidateNodes()
		if transportErr != nil {
			m.scheduleFailureProbe(value)
		}
	}
}

func (m *Manager) cachedNodeIsHealthy(nodeID uint64) bool {
	m.nodeMu.Lock()
	validUntil, ok := m.healthyNodes[nodeID]
	healthy := ok && time.Now().UTC().Before(validUntil)
	if ok && !healthy {
		delete(m.healthyNodes, nodeID)
	}
	m.nodeMu.Unlock()
	return healthy
}

func nodeIsHealthy(value domain.Node) bool {
	return value.Health >= 1 && value.FailureCount == 0 && value.CooldownUntil == nil && value.LastError == ""
}

func (m *Manager) clearanceMode() string {
	m.clearanceMu.Lock()
	defer m.clearanceMu.Unlock()
	return m.clearanceConfig.Mode
}

func (m *Manager) ensureClearance(ctx context.Context, node domain.Node, proxyURL, existingCookies, existingUserAgent, key string, persist bool) (string, string, error) {
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
		state = clearanceState{
			cookies: existingCookies, userAgent: existingUserAgent, used: true, version: version,
			fingerprint: node.ClearanceFingerprint, bindingFingerprint: node.ClearanceBindingFingerprint,
			lastUsedAt: now,
		}
		if node.ClearanceRefreshedAt != nil {
			state.refreshedAt = *node.ClearanceRefreshedAt
		}
		known = true
		m.clearances[key] = state
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
		return cookies, userAgent, nil
	}
	fallbackAllowed := known && !state.invalid && state.userAgent != "" &&
		(state.bindingFingerprint == "" || state.bindingFingerprint == bindingFingerprint)
	fallback := clearanceSolution{Cookies: state.cookies, UserAgent: state.userAgent}
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
			return fallback.Cookies, fallback.UserAgent, nil
		}
		return existingCookies, existingUserAgent, nil
	}
	if cfg.Mode != "flaresolverr" && cfg.Mode != "on_demand" {
		m.clearanceMu.Unlock()
		return existingCookies, existingUserAgent, nil
	}
	m.clearanceMu.Unlock()

	result, err, _ := m.clearanceLoads.Do(key, func() (any, error) {
		// FlareSolverr 求解最长 cfg.Timeout(默认 1m):领头调用方(某个 HTTP 请求)
		// 断开时不能中止求解——所有合并等待者都会拿到 context canceled 类错误,
		// 与真实负载无关。求解与分布式锁都在脱离请求生命周期的 ctx 上进行;
		// 预算 = 求解超时 + 锁等待宽限。
		solveTimeout := m.currentClearanceTimeout()
		solveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), solveTimeout+clearanceLockGrace)
		defer cancel()
		return m.refreshNode(solveCtx, node, proxyURL, key, persist, forceRefresh, !fallbackAllowed, refreshAfter)
	})
	if err != nil {
		if fallbackAllowed {
			return fallback.Cookies, fallback.UserAgent, nil
		}
		return "", "", err
	}
	solution := result.(clearanceSolution)
	return solution.Cookies, solution.UserAgent, nil
}

// currentClearanceTimeout 返回当前 Clearance 求解超时(零值时按默认 1m),
// 供 singleflight 闭包预算派生 ctx 使用。
func (m *Manager) currentClearanceTimeout() time.Duration {
	m.clearanceMu.Lock()
	cfg := m.clearanceConfig
	m.clearanceMu.Unlock()
	if cfg.Timeout > 0 {
		return cfg.Timeout
	}
	return time.Minute
}

func (m *Manager) refreshNode(ctx context.Context, node domain.Node, proxyURL, key string, persist, force, waitForPeer bool, refreshAfter time.Time) (clearanceSolution, error) {
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
	if persist && node.ID != 0 && lock != nil {
		release, acquired, err := lock.Acquire(ctx, "egress-clearance:"+strconv.FormatUint(node.ID, 10), timeout+clearanceLockGrace)
		if err != nil {
			return clearanceSolution{}, fmt.Errorf("协调 Clearance 刷新: %w", err)
		}
		if !acquired {
			if !force {
				if solution, refreshedAt, ok := m.loadPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval); ok {
					m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
					return solution, nil
				}
			}
			if waitForPeer {
				if solution, refreshedAt, ok := m.waitPersistedClearance(ctx, node.ID, fingerprint, bindingFingerprint, interval, timeout, refreshAfter); ok {
					m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
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
				m.cacheClearance(key, solution, refreshedAt, solveVersion, fingerprint, bindingFingerprint, interval)
				return solution, nil
			}
		}
	}
	solveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	solution, err := solver.Solve(solveCtx, cfg, proxyURL)
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
		if stateRepository, ok := m.repository.(egressStateRepository); ok {
			if updateErr := stateRepository.UpdateEgressNodeClearance(ctx, node.ID, encryptedCookies, solution.UserAgent, fingerprint, bindingFingerprint, now); updateErr != nil {
				return clearanceSolution{}, updateErr
			}
		} else {
			latest, loadErr := m.repository.GetEgressNode(ctx, node.ID)
			if loadErr != nil {
				return clearanceSolution{}, loadErr
			}
			latest.EncryptedCloudflareCookie = encryptedCookies
			latest.UserAgent = solution.UserAgent
			latest.ClearanceFingerprint = fingerprint
			latest.ClearanceBindingFingerprint = bindingFingerprint
			latest.ClearanceRefreshedAt = &now
			latest.LastError = ""
			if _, updateErr := m.repository.UpdateEgressNode(ctx, latest); updateErr != nil {
				return clearanceSolution{}, updateErr
			}
		}
		m.invalidateNodes()
	}
	m.cacheClearance(key, solution, now, solveVersion, fingerprint, bindingFingerprint, interval)
	return solution, nil
}

func (m *Manager) waitPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval, timeout time.Duration, refreshAfter time.Time) (clearanceSolution, time.Time, bool) {
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

func (m *Manager) loadPersistedClearance(ctx context.Context, nodeID uint64, fingerprint, bindingFingerprint string, interval time.Duration) (clearanceSolution, time.Time, bool) {
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

func (m *Manager) cacheClearance(key string, solution clearanceSolution, refreshedAt time.Time, version uint64, fingerprint, bindingFingerprint string, interval time.Duration) {
	m.clearanceMu.Lock()
	now := time.Now().UTC()
	m.cleanupClearanceCacheLocked(now, interval)
	if _, exists := m.clearances[key]; !exists {
		m.ensureClearanceCacheCapacityLocked()
	}
	m.clearances[key] = clearanceState{
		cookies: solution.Cookies, userAgent: solution.UserAgent, refreshedAt: refreshedAt,
		used: true, version: version, fingerprint: fingerprint, bindingFingerprint: bindingFingerprint, lastUsedAt: now,
	}
	m.clearanceMu.Unlock()
}

func (m *Manager) cleanupClearanceCacheLocked(now time.Time, interval time.Duration) {
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

func (m *Manager) ensureClearanceCacheCapacityLocked() {
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

func (m *Manager) recordClearanceError(ctx context.Context, node domain.Node, persist bool) {
	if node.ID == 0 || !persist {
		return
	}
	if stateRepository, ok := m.repository.(egressStateRepository); ok {
		if err := stateRepository.UpdateEgressNodeLastError(ctx, node.ID, "clearance refresh failed"); err == nil {
			m.invalidateNodes()
			return
		}
	}
	latest, err := m.repository.GetEgressNode(ctx, node.ID)
	if err != nil {
		return
	}
	latest.LastError = "clearance refresh failed"
	if _, err := m.repository.UpdateEgressNode(ctx, latest); err == nil {
		m.invalidateNodes()
	}
}

func (m *Manager) RefreshClearance(ctx context.Context, nodeID uint64) error {
	if nodeID == 0 {
		_, err, _ := m.clearanceLoads.Do("direct", func() (any, error) {
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
	_, err, _ = m.clearanceLoads.Do(key, func() (any, error) {
		return m.refreshNode(ctx, node, proxyURL, key, true, true, true, refreshAfter)
	})
	return err
}

func (m *Manager) InvalidateClearance(nodeID uint64) {
	m.clearanceMu.Lock()
	m.invalidateNodeClearancesLocked(nodeID)
	m.clearanceMu.Unlock()
	m.clientMu.Lock()
	stale := m.invalidateClientLocked(nodeID)
	m.clientMu.Unlock()
	closeRequestClients(stale)
}

// invalidateNodeClearancesLocked 把节点名下所有 Clearance 缓存标记为失效,
// 覆盖节点级键(node:N)与粘性账号键(node:N:account:<digest>)。调用方
// 必须持有 m.clearanceMu。
func (m *Manager) invalidateNodeClearancesLocked(nodeID uint64) {
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
func (m *Manager) ForgetClearance(nodeID uint64) {
	m.ForgetClearances([]uint64{nodeID})
}

// ForgetClearances evicts a batch of node-scoped runtime state with one cache
// scan and one lock acquisition. Administrative bulk updates can contain
// thousands of nodes, so repeating the global snapshot invalidation per ID
// would add avoidable lock contention and CPU work.
func (m *Manager) ForgetClearances(nodeIDs []uint64) {
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
	m.nodeMu.Lock()
	m.clientMu.Lock()
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
	// Node mutations are rare administration operations. Clearing the small
	// one-second snapshots prevents a just-edited proxy from being used once
	// more before its scope cache expires.
	if m.nodeVersions == nil {
		m.nodeVersions = make(map[string]uint64)
	}
	m.nodeVersions[nodeSnapshotKey]++
	clear(m.nodes)
	clear(m.healthyNodes)
	var stale []requestClient
	for nodeID := range ids {
		m.invalidateClientVersionLocked(nodeID)
	}
	for key, cached := range m.clients {
		if _, selected := ids[key.nodeID]; !selected {
			continue
		}
		delete(m.clients, key)
		stale = append(stale, cached.client)
	}
	m.clientMu.Unlock()
	m.nodeMu.Unlock()
	m.clearanceMu.Unlock()
	closeRequestClients(stale)
}

func (m *Manager) invalidateClearanceKey(key string, client requestClient) {
	m.clearanceMu.Lock()
	state := m.clearances[key]
	state.invalid = true
	state.used = true
	state.lastUsedAt = time.Now().UTC()
	m.clearances[key] = state
	m.clearanceMu.Unlock()
	if client != nil {
		client.CloseIdleConnections()
	}
}

func (m *Manager) RefreshDueClearances(ctx context.Context, force bool) error {
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
		_, refreshErr, _ := m.clearanceLoads.Do(key, func() (any, error) {
			return m.refreshNode(ctx, node, proxyURL, key, true, refreshForce, false, refreshAfter)
		})
		if refreshErr != nil {
			refreshErrors = append(refreshErrors, refreshErr)
		}
	}
	shouldUseDirect := direct.used || force && webNodeCount == 0
	if shouldUseDirect && (force || direct.invalid || direct.userAgent == "" || direct.version != version || now.Sub(direct.refreshedAt) >= interval) {
		_, err, _ := m.clearanceLoads.Do("direct", func() (any, error) {
			return m.refreshNode(ctx, domain.Node{Name: "direct", Enabled: true}, "", "direct", false, force, false, time.Time{})
		})
		if err != nil {
			refreshErrors = append(refreshErrors, err)
		}
	}
	return errors.Join(refreshErrors...)
}

func isGrokWebScope(scope domain.Scope) bool {
	return scope == domain.ScopeWeb || scope == domain.ScopeWebAsset || scope == domain.ScopeConsole
}

func (m *Manager) isStickyProxyNode(value domain.Node) bool {
	if m == nil || m.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	return m.stickyFlagMemoized(value.ID, value.EncryptedProxyURL)
}

// stickyFlagDirect 不经记忆表直接解密判定。replaceNodeSnapshotLocked 在
// 持有 nodeMu 写锁时构建 poolFlags, 不能走 stickyFlagMemoized(会对同一把
// 锁再次加写锁, 自死锁); 快照按 TTL 重建, 每周期一次解密是预期成本。
func (m *Manager) stickyFlagDirect(value domain.Node) bool {
	if m == nil || m.cipher == nil || strings.TrimSpace(value.EncryptedProxyURL) == "" {
		return false
	}
	proxyURL, err := m.cipher.Decrypt(value.EncryptedProxyURL)
	return err == nil && domain.IsAccountTemplateProxy(proxyURL)
}

// proxyFlagMemoEntry 记忆一次粘性判定; ciphertext 参与相等性比较,
// 变更后自然 miss 重算。
type proxyFlagMemoEntry struct {
	ciphertext string
	sticky     bool
}

// proxyFlagMemoMax 是记忆表容量上限:节点 ID 会随订阅换血增长, 超限
// 丢弃任意条目(下次解密重建), 保证内存有界。
const proxyFlagMemoMax = 8192

func (m *Manager) stickyFlagMemoized(nodeID uint64, ciphertext string) bool {
	m.nodeMu.RLock()
	entry, ok := m.proxyFlagMemo[nodeID]
	m.nodeMu.RUnlock()
	if ok && entry.ciphertext == ciphertext {
		return entry.sticky
	}
	proxyURL, err := m.cipher.Decrypt(ciphertext)
	sticky := err == nil && domain.IsAccountTemplateProxy(proxyURL)
	m.nodeMu.Lock()
	if len(m.proxyFlagMemo) >= proxyFlagMemoMax {
		for id := range m.proxyFlagMemo {
			delete(m.proxyFlagMemo, id)
			break
		}
	}
	m.proxyFlagMemo[nodeID] = proxyFlagMemoEntry{ciphertext: ciphertext, sticky: sticky}
	m.nodeMu.Unlock()
	return sticky
}

// isProxyPoolNode 委托 domain 的唯一判定(Node.IsPoolModeNode 的解密版)。
func (m *Manager) isProxyPoolNode(value domain.Node) bool {
	return value.ProxyPool || m.isStickyProxyNode(value)
}

// isProxyPoolNodeDirect 是持 nodeMu 时的版本(见 stickyFlagDirect)。
func (m *Manager) isProxyPoolNodeDirect(value domain.Node) bool {
	return value.ProxyPool || m.stickyFlagDirect(value)
}

// snapshotProxyPoolFlag 是 isProxyPoolNode 的热路径版本:先查快照里预算
// 好的判定表,未命中(单节点查询路径,不在快照内)才回退到解密。
func (m *Manager) snapshotProxyPoolFlag(value domain.Node) bool {
	if value.ProxyPool {
		return true
	}
	// 单次加锁依次查快照判定表与记忆表:池路由路径每个成员都会走到这里,
	// 双重加锁在百成员池上是可测的热点。
	m.nodeMu.RLock()
	snapshot, ok := m.nodes[nodeSnapshotKey]
	if ok {
		if flag, found := snapshot.poolFlags[value.ID]; found {
			m.nodeMu.RUnlock()
			return flag
		}
	}
	entry, memoized := m.proxyFlagMemo[value.ID]
	m.nodeMu.RUnlock()
	if memoized && entry.ciphertext == value.EncryptedProxyURL {
		return entry.sticky
	}
	return m.stickyFlagMemoized(value.ID, value.EncryptedProxyURL)
}

func (m *Manager) invalidateClientLocked(nodeID uint64) []requestClient {
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

func (m *Manager) invalidateClientForScopeLocked(nodeID uint64, scope domain.Scope) []requestClient {
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

func BuildSSOCookie(token, cloudflareCookies string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(strings.ToLower(token), "sso=") {
		token = strings.TrimSpace(token[len("sso="):])
	}
	if value, _, found := strings.Cut(token, ";"); found {
		token = strings.TrimSpace(value)
	}
	token = strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(token)
	cookies := "sso=" + token + "; sso-rw=" + token
	if sanitized := cfcookies.Sanitize(cloudflareCookies); sanitized != "" {
		cookies += "; " + sanitized
	}
	return cookies
}
