package egress

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/cfcookies"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/pkg/proxyurl"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
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

// clearanceRoutineStaleGrace 是例行过期 serve-stale 的宽限:refreshedAt 超出
// 刷新窗口但仍在宽限内时立即交付旧解并由后台合并刷新,覆盖后台例行刷新
// 循环(默认每分钟跑一轮)的调度间隙。invalid(403 失效)与绑定指纹变化
// 不适用:那两类过期是「已知坏了」,同步强制刷新。
const clearanceRoutineStaleGrace = 90 * time.Second
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

// 会话级钉扎的容量与生命周期。会话钉扎表与会话客户端缓存是纯进程内
// 状态(单副本部署下即全量);容量上限防御长生命周期进程的状态增长,
// 空闲超时让停止对话的会话自然让出资源。
const maxSessionPinnedNodes = 8192
const maxSessionCachedClients = 2048
const sessionPinIdleTTL = 2 * time.Hour
const sessionPinSweepInterval = time.Minute

// sessionNodePin 记录一个会话当前钉住的出口节点(sessionPinMu 保护)。
type sessionNodePin struct {
	nodeID   uint64
	lastUsed time.Time
}

var errNodeSnapshotInvalidated = errors.New("egress node snapshot invalidated")
var errClientCacheInvalidated = errors.New("egress client cache invalidated")
var errAccountConnectionIsolationDisabled = errors.New("egress account connection isolation disabled")

type Lease struct {
	clientHandle          *clientHandle
	healthProxy           string
	healthBindingRevision uint64
	healthBaseline        domain.HealthState
	NodeID                uint64
	NodeName              string
	Scope                 domain.Scope
	ProxyURL              string
	UserAgent             string
	CFCookies             string
	client                requestClient
	browser               *browserClient
	sticky                bool
	proxyPool             bool
	buildEnvironmentProxy bool
	connectionPolicy      ConnectionPolicy
	clearanceKey          string
	clearanceGeneration   uint64
	clearanceManager      *Manager
	release               func()
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
	if l.connectionPolicy.Fresh {
		request = request.Clone(request.Context())
		request.Close = true
	}
	request = request.WithContext(attemptmeta.Begin(request.Context(), l.attemptPath()))
	response, err := l.do(request)
	if invalidateForbidden && err == nil && response != nil && response.StatusCode == http.StatusForbidden {
		l.InvalidateClearance()
	}
	return response, err
}

func (l *Lease) attemptPath() attemptmeta.Path {
	path := attemptmeta.Path{NodeID: l.NodeID, Rotating: l.proxyPool}
	if l.NodeID == 0 && l.ProxyURL == "" {
		path.Status = attemptmeta.PathDirect
		if l.buildEnvironmentProxy {
			path.Status = attemptmeta.PathUnknown
		}
	}
	return path
}

// InvalidateClearance invalidates the exact browser-session binding used by
// this lease after a 403 has been classified as egress-related.
func (l *Lease) InvalidateClearance() {
	if l != nil && l.clearanceManager != nil && l.clearanceKey != "" {
		l.clearanceManager.clearance.invalidateClearanceKey(l.clearanceKey, l.client, l.clearanceGeneration)
	}
}

func (l *Lease) Release() {
	if l != nil && l.release != nil {
		l.release()
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
	routing    *routingRuntime
	clearance  *clearanceRuntime
	startOnce  sync.Once
	tasks      *taskRuntime
	transport  *clientRegistry
	health     *healthRuntime
	repository repository.EgressRuntimeRepository
	cipher     security.Cryptor
	logger     *slog.Logger
	// exitEligibility 出口资格谓词缝隙(D3-1);nil=无质量层,恒可调度。
	exitEligibility        atomic.Value // ExitEligibility
	buildStreamIdleTimeout atomic.Int64
	failureProbeMu         sync.Mutex
	failureProber          FailureProber
	failureProbes          map[uint64]failureProbeState
	// knownExitAddrs 是 KnownNodeExitAddrs 的短命快照(knownExitAddrsCache)。
	knownExitAddrs knownExitAddrsCache
}

// knownExitAddrsCache 缓存 KnownNodeExitAddrs 的结果:法院的同出口排除缝
// 按比对候选逐对询问,大机队下一次计划会问上百次,每次都回源读整张节点表
// 不可接受。该数据本就声明为"最后已知、可能过期"的建议性输入,沿用节点
// 快照的 TTL;节点事实失效(InvalidateNodeSnapshots)会一并丢弃它,因此
// 探活写入后立即可见。
type knownExitAddrsCache struct {
	mu       sync.Mutex
	loadedAt time.Time
	addrs    map[uint64]domain.ExitAddresses
}

// rotationPersistState 由 rotationMu 保护。
type rotationPersistState struct {
	last    uint64
	pending uint64
	writing bool
}
type clearanceState struct {
	generation         uint64
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

// operationsConfigRepository is optional so lightweight routing repositories
// retain their narrow contract. The relational implementation supplies it,
// allowing fallback policy to be read only when primary selection fails.

// egressPoolStore supplies dedicated-pool lookups. The relational repository
// implements it; lightweight test repositories keep their narrow contracts.
type egressPoolStore interface {
	GetEgressPool(ctx context.Context, id uint64) (domain.Pool, error)
	ListEgressNodesByPool(ctx context.Context, poolID uint64) ([]domain.Node, error)
}

type operationsConfigRepository interface {
	GetEgressOperationsConfig(context.Context) (domain.OperationsConfig, error)
}

type cachedClient struct {
	// policy is set on the returned copy for each acquisition, including when
	// different hint decisions resolve to the same fresh transport.
	policy   ConnectionPolicy
	handle   *clientHandle
	client   requestClient
	browser  *browserClient
	lastUsed time.Time
}

type clientCacheKey struct {
	nodeID          uint64
	scope           domain.Scope
	fingerprint     string
	accountIdentity string
	// sessionKey partitions a reusable Build pool within the account boundary.
	sessionKey string
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

// NewManagerWithLimits 是 Manager 的唯一构造器;limits 零值等于无上限。
func NewManagerWithLimits(repository repository.EgressRuntimeRepository, cipher security.Cryptor, limits netbudget.Limits) *Manager {
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

		failureProbes: make(map[uint64]failureProbeState),
	}
	manager.tasks = newTaskRuntime()
	manager.transport = newClientRegistry(manager.log, netbudget.New(limits))
	manager.transport.tasks = manager.tasks
	manager.transport.network.SetPressureHandler(func() { manager.tasks.start("idle_sweep", manager.transport.closeIdle) })
	manager.health = newHealthRuntime(manager.persistHealthReport, manager.invalidateNodes)
	manager.routing = newRoutingRuntime(manager)
	manager.clearance = newClearanceRuntime(manager)

	manager.buildStreamIdleTimeout.Store(int64(settingsdomain.DefaultBuildStreamIdleTimeout))
	return manager
}

// SetExitEligibility 安装出口资格谓词缝隙(D3-1);nil 保持未设。
// 质量状态为空时谓词恒真——现行行为零变化。
func (m *Manager) SetExitEligibility(eligibility ExitEligibility) {
	if eligibility == nil {
		return
	}
	m.exitEligibility.Store(eligibility)
}

func (m *Manager) exitEligibilityObserver() ExitEligibility {
	if value, ok := m.exitEligibility.Load().(ExitEligibility); ok {
		return value
	}
	return nil
}

// qualitySchedulable 候选阶段的出口质量资格判定(与租约收口点
// leaseForNodeWithOptions 同款语义):缝隙未注入恒真(D2 剥离态)、
// 取证通道放行(B2 生产/探针双通道)、直连(节点 0)不适用。
// 池成员与自动调度的候选构建用它把质量轴不合格的出口在**选择之前**
// 剔除(B2 查询点 3"池内过滤"):策略挑到的成员必须可用,而不是挑到
// 再失败——被押成员不应让整个池/自动调度失败。
func (m *Manager) qualitySchedulable(ctx context.Context, nodeID uint64) bool {
	if nodeID == 0 || exitEligibilityBypassed(ctx) {
		return true
	}
	if eligibility := m.exitEligibilityObserver(); eligibility != nil {
		return eligibility.ExitSchedulable(nodeID)
	}
	return true
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

// UpdateBuildResponseHeaderTimeout rebuilds only cached Build clients. Active
// requests keep their current transport and are not interrupted.
func (m *Manager) UpdateBuildResponseHeaderTimeout(value time.Duration) {
	m.transport.UpdateBuildResponseHeaderTimeout(value)
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
	m.transport.UpdateAccountIsolatedConnections(enabled)
}

// AccountIsolatedConnections reports whether upstream clients are partitioned by account.
func (m *Manager) AccountIsolatedConnections() bool {
	return m != nil && m.transport.accountIsolated.Load()
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
	m.clearance.SetClearanceLock(value)
}

func (m *Manager) UpdateClearanceConfig(value ClearanceConfig) {
	m.clearance.UpdateClearanceConfig(value)
}

// AcquireBuildEnvironmentDirect preserves environment proxy selection while
// keeping the default Build path under the runtime's resource ownership.
func (m *Manager) AcquireBuildEnvironmentDirect(ctx context.Context, affinity string) (*Lease, error) {
	selected := domain.Node{ID: 0, Name: "direct", Enabled: true, Health: 1}
	lease, _, err := m.leaseForNodeWithOptions(ctx, domain.ScopeBuild, affinity, "", false, selected, clientOptions{buildEnvironmentProxy: true})
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
// CredentialLeaser 是 Provider 适配器可见的网络能力合同:仅凭据
// 租约获取;节点管理、轮换配置等 Manager 管理面不在消费面上。
type CredentialLeaser interface {
	AcquireCredential(ctx context.Context, scope domain.Scope, credential accountdomain.Credential) (*Lease, error)
}

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

// managedClearanceMode 报告当前 Clearance 是否为托管模式(flaresolverr/
// on_demand)。池路由与固定目标路由共用这一判定,保证同一作用域的
// Clearance 行为不因出口形态不同而分叉。
func (m *Manager) managedClearanceMode() bool {
	mode := m.clearance.clearanceMode()
	return mode == "flaresolverr" || mode == "on_demand"
}

func (m *Manager) acquire(ctx context.Context, scope domain.Scope, affinity string, allowDirect bool, encryptedCredentialCookies string) (*Lease, bool, error) {
	for attempt := 0; attempt < clientCreationRetryLimit; attempt++ {
		lease, configured, err := m.routing.acquire(ctx, scope, affinity, allowDirect, encryptedCredentialCookies)
		if !errors.Is(err, errNodeSnapshotInvalidated) {
			return lease, configured, err
		}
	}
	return nil, false, errNodeSnapshotInvalidated
}

// ErrRoutingTargetUnavailable reports that a configured fixed routing
// target cannot currently serve the request. Callers fail fast: a configured
// target is a strict binding, never silently rerouted to other exits.
var ErrRoutingTargetUnavailable = errors.New("egress routing target unavailable")

func (m *Manager) loadOperationsConfig(ctx context.Context, now time.Time) (domain.OperationsConfig, bool, error) {
	return m.routing.loadOperationsConfig(ctx, now)
}

type clientOptions struct {
	buildEnvironmentProxy   bool
	requireAccountIsolation bool
	freshTunnel             bool
	// sessionKey is a soft hint; the registry resolves scope and fresh policy
	// before choosing a client, always respecting the account boundary.
	sessionKey string
	// onSessionDial 仅诊断用:会话客户端每次新建上游连接时回调。
	onSessionDial func()
}

func (m *Manager) leaseForNode(ctx context.Context, scope domain.Scope, affinity, encryptedCredentialCookies string, managedClearance bool, selected domain.Node) (*Lease, bool, error) {
	return m.leaseForNodeWithOptions(ctx, scope, affinity, encryptedCredentialCookies, managedClearance, selected, clientOptions{})
}

func (m *Manager) leaseForNodeWithOptions(ctx context.Context, scope domain.Scope, affinity, encryptedCredentialCookies string, managedClearance bool, selected domain.Node, options clientOptions) (*Lease, bool, error) {
	if !m.routing.selectionCurrent(ctx) {
		return nil, false, errNodeSnapshotInvalidated
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// 出口资格谓词缝隙(D3-1):质量轴不合格的出口不租出。全部租约路径
	// (自动调度/池成员/固定目标)都经过本函数——单一收口点。取证通道
	// (B2 生产/探针双通道)与直连(节点 0)放行;缝隙未注入恒真。
	if selected.ID != 0 && !exitEligibilityBypassed(ctx) {
		eligibility := m.exitEligibilityObserver()
		if admission, ok := eligibility.(ExitAdmission); ok {
			allowed, err := admission.CheckExitAdmission(ctx, selected.ID)
			if err != nil {
				return nil, false, fmt.Errorf("%w: exit admission unavailable: %w", ErrRoutingTargetUnavailable, err)
			}
			if !allowed {
				return nil, false, fmt.Errorf("%w: node %d quality-ineligible", ErrRoutingTargetUnavailable, selected.ID)
			}
		} else if eligibility != nil && !eligibility.ExitSchedulable(selected.ID) {
			return nil, false, fmt.Errorf("%w: node %d quality-ineligible", ErrRoutingTargetUnavailable, selected.ID)
		}
	}
	credentialCookies := ""
	if !managedClearance && isGrokWebScope(scope) && strings.TrimSpace(encryptedCredentialCookies) != "" {
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
	// 外部代理池:出口由服务商自动更换(粘性/每请求),连接阶段重试与全新
	// CONNECT 即新出口。自建 WARP 出口(换IP Webhook)是固定 IP,不在此列。
	// 池模式判定(代理池标志或账号模板)统一委托 domain 唯一策略;此处已持有
	// 明文代理 URL,故用节点判定而非自行拼标志。
	proxyPool := selected.IsPoolModeNode(proxyURL)
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
	if isGrokWebScope(scope) {
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
	var clearanceGeneration uint64
	// Manual mode may prefer account-bound cookies. Managed mode always enters
	// the FlareSolverr lifecycle so stale imported cookies cannot bypass refresh.
	if managedClearance {
		clearanceKey = clearanceCacheKey(selected.ID, proxyURL, sticky)
		cookies, userAgent, clearanceGeneration, err = m.clearance.ensureClearance(ctx, selected, proxyURL, cookies, userAgent, clearanceKey, !sticky)
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
	options.sessionKey = buildSessionFromContext(ctx)
	options.freshTunnel = freshTunnel
	if options.sessionKey != "" {
		options.onSessionDial = func() {
			m.log().Info("egress_session_conn_dial", "session", options.sessionKey, "node", selected.ID)
		}
	}
	var client cachedClient
	for attempt := 0; attempt < clientCreationRetryLimit; attempt++ {
		client, err = m.transport.clientForContext(ctx, selected.ID, scope, proxyURL, userAgent, cookies, sticky, accountIdentity, options)
		if err != nil {
			return nil, false, err
		}
		if client.handle == nil || client.handle.retainLease() {
			break
		}
		err = ErrClientRetired
	}
	if err != nil {
		return nil, false, err
	}
	if options.sessionKey != "" {
		m.log().Info("egress_session_lease", "session", options.sessionKey, "node", selected.ID, "reuse", client.policy.SessionReuse, "account_isolated", client.policy.AccountIsolated, "fresh", client.policy.Fresh)
	}
	if err := ctx.Err(); err != nil {
		if client.handle != nil {
			client.handle.releaseLease()
		}
		return nil, false, err
	}
	if !m.routing.selectionCurrent(ctx) {
		if client.handle != nil {
			client.handle.releaseLease()
		}
		return nil, false, errNodeSnapshotInvalidated
	}
	m.incrementInflight(selected.ID)
	// Pool 的语义是"外部代理池"——出口 IP 由服务商自动更换(粘性/每请求),
	// ProxyPool 标志即声明。历史上"静态出口误标池"的案例现由保存时的类型
	// 语义与互斥校验把关,不再由运行时叠加 rotation_enabled 判定兜底(该兜底
	// 曾把无 Webhook 的真代理池误判为固定出口)。
	recordSelection(ctx, Selection{NodeID: selected.ID, NodeName: selected.Name, Scope: scope, Proxied: proxyURL != "", Pool: selected.ProxyPool, Connection: client.policy})
	var once sync.Once
	release := func() {
		once.Do(func() {
			m.decrementInflight(selected.ID)
			if client.handle != nil {
				client.handle.releaseLease()
			}
		})
	}
	stopRelease := context.AfterFunc(ctx, release)
	return &Lease{buildEnvironmentProxy: options.buildEnvironmentProxy, clientHandle: client.handle, healthProxy: selected.EncryptedProxyURL, healthBindingRevision: selected.BindingRevision, healthBaseline: selected.HealthState(), NodeID: selected.ID, NodeName: selected.Name, Scope: scope, ProxyURL: proxyURL, UserAgent: userAgent, CFCookies: cookies, client: client.client, browser: client.browser, sticky: sticky, proxyPool: proxyPool, connectionPolicy: client.policy, clearanceKey: clearanceKey, clearanceGeneration: clearanceGeneration, clearanceManager: m, release: func() { stopRelease(); release() }}, true, nil
}

func (m *Manager) incrementInflight(nodeID uint64) { m.routing.incrementInflight(nodeID) }

func (m *Manager) decrementInflight(nodeID uint64) { m.routing.decrementInflight(nodeID) }

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

// proxyFlagMemoEntry 记忆一次粘性判定; ciphertext 参与相等性比较,
// 变更后自然 miss 重算。
type proxyFlagMemoEntry struct {
	ciphertext string
	sticky     bool
}

// proxyFlagMemoMax 是记忆表容量上限:节点 ID 会随订阅换血增长, 超限
// 丢弃任意条目(下次解密重建), 保证内存有界。
const proxyFlagMemoMax = 8192

// isProxyPoolNode 委托 domain 的唯一判定(Node.IsPoolModeNode 的解密版)。
func (m *Manager) isProxyPoolNode(value domain.Node) bool { return m.routing.isProxyPoolNode(value) }

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
