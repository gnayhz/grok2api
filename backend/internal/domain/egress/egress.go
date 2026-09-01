package egress

import (
	"fmt"
	"strings"
	"time"
)

type Mode string

const (
	ModeDirect Mode = "direct"
	ModeSingle Mode = "single"
	ModePool   Mode = "pool"
)

const LastErrorTransport = "transport error"

// LastErrorExitIPQuality marks a node whose exit IP is quality-degraded
// (degraded-model routing): the degraded account's RSC attribution came back
// clean, so the exit IP is the suspect.
const LastErrorExitIPQuality = "quality degraded (exit ip)"

// Scope names the request-side traffic family. It describes upstream traffic,
// never proxy resources: nodes and pools are scope-free and serve whatever the
// routing configuration sends through them.
type Scope string

const (
	ScopeBuild        Scope = "grok_build"
	ScopeWeb          Scope = "grok_web"
	ScopeConsole      Scope = "grok_console"
	ScopeWebAsset     Scope = "grok_web_asset"
	ScopeConsoleAsset Scope = "grok_console_asset"
)

// RequestScopes lists the scopes that carry upstream traffic. Asset scopes are
// auxiliary downloads of their parent family.
func RequestScopes() []Scope {
	return []Scope{ScopeBuild, ScopeWeb, ScopeConsole, ScopeWebAsset, ScopeConsoleAsset}
}

// RoutingScope maps a request scope onto its routing configuration key.
// Asset downloads follow their parent family's exit so operators configure
// three meaningful rows instead of five overlapping ones.
func RoutingScope(scope Scope) Scope {
	switch scope {
	case ScopeWebAsset:
		return ScopeWeb
	case ScopeConsoleAsset:
		return ScopeConsole
	default:
		return scope
	}
}

// Node is one proxy exit resource. It carries no scope: whether it serves
// Build, Web, or Console traffic is decided exclusively by routing.
type Node struct {
	ID      uint64
	Name    string
	Enabled bool
	// ProxyPool 是节点级"代理池模式"标志。真正的旋转出口还要
	// RotationEnabled：只亮标志的固定 IP 不得当池（冷却豁免、同号重试）。
	// 与 Pool 资源(PoolIDs)正交。唯一判定见 IsPoolModeNode。
	ProxyPool bool
	SourceID  uint64
	// SourceName 是产生该节点的订阅名；空表示手动创建。
	SourceName string
	SourceKey  string
	// PoolIDs 是节点所属的全部代理池（多对多）；空表示未入池（参与自动调度）。
	PoolIDs []uint64
	// PoolPriority 是节点在某个池上下文里的首选顺序（ListEgressNodesByPool
	// 返回时填充；小者先）。首选优先/节点轮询的"首"由此决定。
	PoolPriority      int64
	EncryptedProxyURL string
	// UserAgent/EncryptedCloudflareCookie/Clearance* 是 FlareSolverr 托管
	// 模式写入的系统状态（Cloudflare 凭证与出口 IP + UA 绑定，必须随节点存）。
	// 管理员不可编辑：手动模式的浏览器凭证在账号侧维护，节点资源保持纯净。
	UserAgent                   string
	EncryptedCloudflareCookie   string
	ClearanceRefreshedAt        *time.Time
	ClearanceFingerprint        string
	ClearanceBindingFingerprint string
	// RotationEnabled gates the stored rotation webhook without wiping it:
	// operators can pause rotation and resume later without re-entering the URL.
	RotationEnabled bool
	// EncryptedRotationURL is the optional per-node rotation webhook (full URL
	// including its own token) encrypted at rest. Calling it must rotate the
	// node's exit IP (e.g. restart the MicroWARP instance behind the node).
	EncryptedRotationURL string
	LastRotatedAt        *time.Time
	// RotationAttempts counts rotation attempts inside the current quarantine
	// cycle; it resets when the node is re-admitted.
	RotationAttempts  int
	LastRotationError string
	// DegradeCount/LastDegradedAt track quality-degraded attributions against
	// this node's exit IP (RSC-clean account degradations).
	DegradeCount   int
	LastDegradedAt *time.Time
	Health         float64
	FailureCount   int
	CooldownUntil  *time.Time
	LastError      string
	ProbeStatus    ProbeStatus
	LastProbedAt   *time.Time
	ProbeLatencyMS int
	ExitIP         string
	ProbeError     string
	ProbeProvider  ProbeProvider
	IPv4Probe      ProbeFamilyResult
	IPv6Probe      ProbeFamilyResult
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// NodePoolRef is a lightweight pool reference on public nodes.
type NodePoolRef struct {
	ID   uint64
	Name string
}

type PublicNode struct {
	ID               uint64
	Name             string
	Enabled          bool
	ProxyConfigured  bool
	ProxyDisplay     string
	ProxyFingerprint string
	ProxyPool        bool
	SourceID         uint64
	SourceName       string
	// Pools 列出节点所属的全部代理池（多对多）。
	Pools              []NodePoolRef
	AccountBoundProxy  bool
	RotationConfigured bool
	RotationEnabled    bool
	LastRotatedAt      *time.Time
	RotationAttempts   int
	LastRotationError  string
	DegradeCount       int
	LastDegradedAt     *time.Time
	Health             float64
	FailureCount       int
	CooldownUntil      *time.Time
	LastError          string
	ProbeStatus        ProbeStatus
	LastProbedAt       *time.Time
	ProbeLatencyMS     int
	ExitIP             string
	ProbeError         string
	ProbeProvider      ProbeProvider
	IPv4Probe          ProbeFamilyResult
	IPv6Probe          ProbeFamilyResult
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// PoolStrategy selects how a pool schedules one member per request.
type PoolStrategy string

const (
	// PoolStrategyAffinity keeps every caller identity on a stable exit IP
	// (rendezvous hashing). Node failures reshuffle only the affected callers.
	PoolStrategyAffinity PoolStrategy = "affinity"
	// PoolStrategyRandom spreads every request over a random member.
	PoolStrategyRandom PoolStrategy = "random"
	// PoolStrategySticky always uses the first schedulable member in stable
	// order and only moves on when that member breaks.
	PoolStrategySticky PoolStrategy = "sticky"
	// PoolStrategyRotation keeps a persistent per-pool cursor: stay on the
	// current member until it breaks, then advance to the next member in
	// stable order (wrapping) and never regress on recovery.
	PoolStrategyRotation PoolStrategy = "rotation"
)

func (value PoolStrategy) IsValid() bool {
	return value == PoolStrategyAffinity || value == PoolStrategyRandom || value == PoolStrategySticky || value == PoolStrategyRotation
}

// Normalized maps the zero value (pre-strategy rows) onto the historical
// rendezvous behavior so existing pools keep serving identically.
func (value PoolStrategy) Normalized() PoolStrategy {
	if !value.IsValid() {
		return PoolStrategyAffinity
	}
	return value
}

// PoolFallbackMode controls what an exhausted dedicated pool falls back to.
type PoolFallbackMode string

const (
	// PoolFallbackNone keeps quality-first semantics: an exhausted pool does
	// not silently serve traffic from elsewhere; selection continues down the
	// automatic schedule instead.
	PoolFallbackNone PoolFallbackMode = "none"
	// PoolFallbackPool chains to another pool (availability-first).
	PoolFallbackPool PoolFallbackMode = "pool"
	// PoolFallbackDirect falls back to a direct (no proxy) connection.
	PoolFallbackDirect PoolFallbackMode = "direct"
)

func (value PoolFallbackMode) IsValid() bool {
	return value == PoolFallbackNone || value == PoolFallbackPool || value == PoolFallbackDirect
}

// Normalized maps the zero value (pre-pool rows) to none.
func (value PoolFallbackMode) Normalized() PoolFallbackMode {
	if value == "" {
		return PoolFallbackNone
	}
	return value
}

// Pool is a named group of nodes scheduled as one resource. It carries no
// scope: routing decides which traffic enters it, the pool decides the member.
type Pool struct {
	ID             uint64
	Name           string
	Enabled        bool
	Strategy       PoolStrategy
	FallbackMode   PoolFallbackMode
	FallbackPoolID uint64
	// RotationCursorNodeID 持久化的节点轮询游标（当前钉住的节点）。
	RotationCursorNodeID uint64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// PublicPool is the management projection of a pool with member health
// summary. Node proxy material is never included.
type PublicPool struct {
	ID               uint64
	Name             string
	Enabled          bool
	Strategy         PoolStrategy
	FallbackMode     PoolFallbackMode
	FallbackPoolID   uint64
	FallbackPoolName string
	// MemberCount/HealthyCount/QuarantinedCount summarize member nodes at
	// list time (quarantined = hard quality quarantine or cooling down).
	MemberCount      int
	HealthyCount     int
	QuarantinedCount int
	// MemberIDs lists the member node ids (pool-side membership editing).
	MemberIDs []uint64
	// PreferredNodeID 是池内设为首选的节点（priority 最小）；0 = 未设置。
	PreferredNodeID uint64
	// RotationCursorNodeID 是节点轮询当前钉住的节点；0 = 未开始。
	RotationCursorNodeID uint64
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

type ProbeStatus string

const (
	ProbeStatusUnknown   ProbeStatus = "unknown"
	ProbeStatusHealthy   ProbeStatus = "healthy"
	ProbeStatusUnhealthy ProbeStatus = "unhealthy"
)

func (value ProbeStatus) IsValid() bool {
	switch value {
	case ProbeStatusUnknown, ProbeStatusHealthy, ProbeStatusUnhealthy:
		return true
	default:
		return false
	}
}

// ProbeResult contains only operational metadata. It never stores or exposes
// proxy credentials.
type ProbeResult struct {
	Status    ProbeStatus
	TestedAt  time.Time
	LatencyMS int
	ExitIP    string
	Error     string
	Provider  ProbeProvider
	IPv4      ProbeFamilyResult
	IPv6      ProbeFamilyResult
}

// ProbeFamilyResult stores one address family's independent connectivity
// result. A zero TestedAt represents a family that has not been tested yet.
type ProbeFamilyResult struct {
	Status    ProbeStatus
	TestedAt  time.Time
	LatencyMS int
	ExitIP    string
	Error     string
}

// SubscriptionSource stores a write-only remote proxy subscription. The URL
// remains encrypted at rest and must never be returned by management APIs.
type SubscriptionSource struct {
	ID      uint64
	Name    string
	Enabled bool
	// 订阅只是节点的一种来源：同步产出的节点与手动节点完全同权，
	// 是否入池在池那边管理，与订阅无关。
	EncryptedURL           string
	EncryptedProxyURL      string
	RefreshIntervalSeconds int
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int
	LastSyncError          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type PublicSubscriptionSource struct {
	ID                     uint64
	Name                   string
	Enabled                bool
	URLConfigured          bool
	ProxyConfigured        bool
	RefreshIntervalSeconds int
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int
	LastSyncError          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

type ProbeProvider string

const (
	ProbeProviderIPInfo     ProbeProvider = "ipinfo"
	ProbeProviderCloudflare ProbeProvider = "cloudflare"
)

func (value ProbeProvider) IsValid() bool {
	return value == ProbeProviderIPInfo || value == ProbeProviderCloudflare
}

func (value ProbeProvider) Normalized() ProbeProvider {
	if !value.IsValid() {
		return ProbeProviderCloudflare
	}
	return value
}

// RoutingTargetMode selects what one routing level sends traffic through.
type RoutingTargetMode string

const (
	// RoutingTargetAuto schedules across every enabled node that has not been
	// placed in a pool: health-aware, caller-sticky selection. It is the
	// default whenever no more specific rule exists.
	RoutingTargetAuto RoutingTargetMode = "auto"
	// RoutingTargetDirect leaves through the machine's own network.
	RoutingTargetDirect RoutingTargetMode = "direct"
	// RoutingTargetNode pins one fixed node.
	RoutingTargetNode RoutingTargetMode = "node"
	// RoutingTargetPool enters a dedicated pool; the pool's strategy picks
	// the member.
	RoutingTargetPool RoutingTargetMode = "pool"
)

func (value RoutingTargetMode) IsValid() bool {
	switch value {
	case RoutingTargetAuto, RoutingTargetDirect, RoutingTargetNode, RoutingTargetPool:
		return true
	default:
		return false
	}
}

// Normalized maps the zero value onto the automatic schedule.
func (value RoutingTargetMode) Normalized() RoutingTargetMode {
	if !value.IsValid() {
		return RoutingTargetAuto
	}
	return value
}

// RoutingTarget is one routing decision: the mode plus the node/pool it
// references. NodeID/PoolID must be zero unless the mode requires them.
type RoutingTarget struct {
	Mode   RoutingTargetMode
	NodeID uint64
	PoolID uint64
}

func (value RoutingTarget) Valid() bool {
	switch value.Mode.Normalized() {
	case RoutingTargetAuto, RoutingTargetDirect:
		return value.NodeID == 0 && value.PoolID == 0
	case RoutingTargetNode:
		return value.NodeID != 0 && value.PoolID == 0
	case RoutingTargetPool:
		return value.PoolID != 0 && value.NodeID == 0
	default:
		return false
	}
}

// Configured reports whether the target was explicitly set (auto counts as an
// explicit choice; the zero value means "not configured" and also resolves to
// auto).
func (value RoutingTarget) Configured() bool {
	return value.Mode != ""
}

// Resolved returns the canonical target with a concrete mode.
func (value RoutingTarget) Resolved() RoutingTarget {
	value.Mode = value.Mode.Normalized()
	return value
}

// OperationsConfig controls probe scheduling and egress routing. Routing is
// resolved per request as: traffic class target → scope target → default
// target → automatic schedule.
type OperationsConfig struct {
	ProbeProvider        ProbeProvider
	ProbeIntervalSeconds int
	// DefaultTarget is the 总出口: it applies to any scope/class without a
	// more specific rule.
	DefaultTarget RoutingTarget
	// ScopeTargets routes one request family (build/web/console). Asset
	// scopes inherit their parent family's target.
	ScopeTargets map[Scope]RoutingTarget
	// ClassTargets routes one semantic traffic class regardless of scope;
	// classes are declared by provider call sites that annotate them.
	ClassTargets map[TrafficClass]RoutingTarget
	UpdatedAt    time.Time
}

// resolveTarget is the single routing ladder: class rule → scope rule →
// default target → automatic schedule. TargetFor and DecidingLevel both
// derive from it so the decision and its stats attribution can never diverge.
func (value OperationsConfig) resolveTarget(scope Scope, class TrafficClass) (RoutingTarget, string, bool) {
	if class != "" {
		if target, ok := value.ClassTargets[class]; ok && target.Configured() {
			return target.Resolved(), "class:" + string(class), true
		}
	}
	if target, ok := value.ScopeTargets[RoutingScope(scope)]; ok && target.Configured() {
		return target.Resolved(), "scope:" + string(RoutingScope(scope)), true
	}
	if value.DefaultTarget.Configured() {
		return value.DefaultTarget.Resolved(), "default", true
	}
	return RoutingTarget{Mode: RoutingTargetAuto}, "default", false
}

// TargetFor resolves the routing decision for one request scope and traffic
// class in one place: the most specific configured level wins and anything
// unconfigured ends on the automatic schedule.
func (value OperationsConfig) TargetFor(scope Scope, class TrafficClass) RoutingTarget {
	target, _, _ := value.resolveTarget(scope, class)
	return target
}

// DecidingLevel names the routing level that actually decided one request,
// derived from the same ladder as TargetFor. The bool reports that any rule
// was configured at all (unconfigured traffic follows the automatic schedule
// and produces no counters).
func (value OperationsConfig) DecidingLevel(scope Scope, class TrafficClass) (string, bool) {
	_, level, configured := value.resolveTarget(scope, class)
	return level, configured
}

// TrafficClass names the operational purpose of one upstream call. Classes are
// declared by provider code at the call site; configuration only maps them to
// egress targets, so upstream host or path changes never invalidate rules.
type TrafficClass string

const (
	// TrafficClassInference covers account-facing generation requests such as
	// responses, chat, messages, compaction, and reasoning recovery. It is the
	// default for unannotated calls.
	TrafficClassInference TrafficClass = "inference"
	// TrafficClassCredential covers OAuth token refresh and device
	// authorization exchanges.
	TrafficClassCredential TrafficClass = "credential"
	// TrafficClassBilling covers quota, billing, and subscription lookups.
	TrafficClassBilling TrafficClass = "billing"
	// TrafficClassModelSync covers account model catalog discovery.
	TrafficClassModelSync TrafficClass = "model_sync"
	// TrafficClassVideo covers video job submission, polling, and asset
	// downloads.
	TrafficClassVideo TrafficClass = "video"
	// TrafficClassProbe covers risk-attribution probes (SSO thinking probe,
	// Build differential probe) that verify account/exit health. These are
	// low-volume but must exit through a clean IP for reliable verdicts.
	TrafficClassProbe TrafficClass = "probe"
)

// IsValid reports whether the value is a known traffic class.
func (value TrafficClass) IsValid() bool {
	switch value {
	case TrafficClassInference, TrafficClassCredential, TrafficClassBilling, TrafficClassModelSync, TrafficClassVideo, TrafficClassProbe:
		return true
	default:
		return false
	}
}

// TrafficClasses lists every schedulable class in stable order.
func TrafficClasses() []TrafficClass {
	return []TrafficClass{TrafficClassInference, TrafficClassCredential, TrafficClassBilling, TrafficClassModelSync, TrafficClassVideo, TrafficClassProbe}
}

// MaxRoutingTargets caps each routing level so a degenerate payload fails
// fast before any per-target lookups run.
const MaxRoutingTargets = 16

// ValidateRoutingTargets checks enum validity, target shape, scope/class keys,
// and per-level size bounds. Existence of referenced nodes/pools is validated
// by the application layer, which can query the stores.
func ValidateRoutingTargets(defaultTarget RoutingTarget, scopes map[Scope]RoutingTarget, classes map[TrafficClass]RoutingTarget) error {
	if defaultTarget.Configured() && !defaultTarget.Valid() {
		return fmt.Errorf("总出口路由目标无效: %q", defaultTarget.Mode)
	}
	if len(scopes) > MaxRoutingTargets || len(classes) > MaxRoutingTargets {
		return fmt.Errorf("单层路由目标最多 %d 条", MaxRoutingTargets)
	}
	for scope, target := range scopes {
		if scope != ScopeBuild && scope != ScopeWeb && scope != ScopeConsole {
			return fmt.Errorf("作用域路由仅支持 %s、%s、%s，收到 %s", ScopeBuild, ScopeWeb, ScopeConsole, scope)
		}
		if !target.Configured() || !target.Valid() {
			return fmt.Errorf("作用域 %s 的路由目标无效", scope)
		}
	}
	for class, target := range classes {
		if !class.IsValid() {
			return fmt.Errorf("流量类别路由的类别无效: %q", class)
		}
		if !target.Configured() || !target.Valid() {
			return fmt.Errorf("流量类别 %s 的路由目标无效", class)
		}
	}
	return nil
}

func DefaultOperationsConfig() OperationsConfig {
	return OperationsConfig{
		ProbeProvider:        ProbeProviderCloudflare,
		ProbeIntervalSeconds: 900,
		DefaultTarget:        RoutingTarget{Mode: RoutingTargetAuto},
	}
}

// CanNodeServeFixedTarget reports whether a node is schedulable as a fixed
// routing target. This is the single authoritative predicate for the runtime,
// save-time, and edit-guard paths; all of them must agree or a target accepted
// by one path silently degrades at another. Sticky per-account templates are
// rejected separately at the application layer (they need decryption).
//
// 旋转出口(节点级代理池模式)可以被固定目标引用:固定的是"这条隧道",
// 不是它的瞬时出口 IP。运行时对池模式节点豁免硬/软冷却(单个坏 IP 不
// 代表端点坏),所以旋转目标几乎不会被冷却卡死——这正是它的用法。
func CanNodeServeFixedTarget(node Node) bool {
	return node.Enabled && node.EncryptedProxyURL != ""
}

// ProxyAccountPlaceholder 是粘性代理模板占位符:代理 URL 含该占位符时,
// 每个账号按自身身份渲染出独立子账号(粘性出口)。该常量与判定函数是
// "账号模板代理"的唯一权威定义,应用层与基础设施层都必须委托于此,
// 不得各自复制 strings.Contains 判定。
const ProxyAccountPlaceholder = "{account}"

// IsAccountTemplateProxy 判定一个(已解密的)代理 URL 是否为账号模板。
func IsAccountTemplateProxy(proxyURL string) bool {
	return strings.Contains(proxyURL, ProxyAccountPlaceholder)
}

// IsPoolModeNode 是"代理池模式节点"的唯一判定:节点级代理池标志且已开
// 轮换（同节点连续请求出口 IP 不同），或代理 URL 是账号模板（粘性出口
// 每请求独立，共享健康惩罚无意义）。只亮 ProxyPool、未开轮换的固定 IP
// 不是池。
func (value Node) IsPoolModeNode(decryptedProxyURL string) bool {
	return (value.ProxyPool && value.RotationEnabled) || IsAccountTemplateProxy(decryptedProxyURL)
}
