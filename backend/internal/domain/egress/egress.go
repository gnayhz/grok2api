package egress

import (
	"fmt"
	"time"
)

type Mode string

const (
	ModeDirect Mode = "direct"
	ModeSingle Mode = "single"
	ModePool   Mode = "pool"
)

const LastErrorTransport = "transport error"

type Scope string

const (
	ScopeBuild        Scope = "grok_build"
	ScopeWeb          Scope = "grok_web"
	ScopeConsole      Scope = "grok_console"
	ScopeWebAsset     Scope = "grok_web_asset"
	ScopeConsoleAsset Scope = "grok_console_asset"
)

type Node struct {
	ID                          uint64
	Name                        string
	Scope                       Scope
	Enabled                     bool
	ProxyPool                   bool
	SourceID                    uint64
	SourceKey                   string
	AccountCapacity             int
	ProxyProfileID              uint64
	ProxyProfileName            string
	EncryptedProxyURL           string
	UserAgent                   string
	EncryptedCloudflareCookie   string
	ClearanceRefreshedAt        *time.Time
	ClearanceFingerprint        string
	ClearanceBindingFingerprint string
	Health                      float64
	FailureCount                int
	CooldownUntil               *time.Time
	LastError                   string
	ProbeStatus                 ProbeStatus
	LastProbedAt                *time.Time
	ProbeLatencyMS              int
	ExitIP                      string
	ProbeError                  string
	ProbeProvider               ProbeProvider
	IPv4Probe                   ProbeFamilyResult
	IPv6Probe                   ProbeFamilyResult
	AssignedAccountCount        int
	CreatedAt                   time.Time
	UpdatedAt                   time.Time
}

type PublicNode struct {
	ID                   uint64
	Name                 string
	Scope                Scope
	Enabled              bool
	ProxyConfigured      bool
	ProxyDisplay         string
	ProxyFingerprint     string
	ProxyPool            bool
	SourceID             uint64
	AccountCapacity      int
	ProxyProfileID       uint64
	ProxyProfileName     string
	UserAgent            string
	CookieConfigured     bool
	AccountBoundProxy    bool
	Health               float64
	FailureCount         int
	CooldownUntil        *time.Time
	LastError            string
	ProbeStatus          ProbeStatus
	LastProbedAt         *time.Time
	ProbeLatencyMS       int
	ExitIP               string
	ProbeError           string
	ProbeProvider        ProbeProvider
	IPv4Probe            ProbeFamilyResult
	IPv6Probe            ProbeFamilyResult
	AssignedAccountCount int
	CreatedAt            time.Time
	UpdatedAt            time.Time
}

// ProxyProfile is a reusable physical proxy configuration. Provider-specific
// health, capacity, browser identity, and Clearance remain on Node.
type ProxyProfile struct {
	ID                uint64
	Name              string
	EncryptedProxyURL string
	BoundNodeCount    int
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

type PublicProxyProfile struct {
	ID               uint64
	Name             string
	ProxyDisplay     string
	ProxyFingerprint string
	BoundNodeCount   int
	CreatedAt        time.Time
	UpdatedAt        time.Time
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
	ID                     uint64
	Name                   string
	Scope                  Scope
	Enabled                bool
	EncryptedURL           string
	EncryptedProxyURL      string
	RefreshIntervalSeconds int
	DefaultAccountCapacity int
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
	Scope                  Scope
	Enabled                bool
	URLConfigured          bool
	ProxyConfigured        bool
	RefreshIntervalSeconds int
	DefaultAccountCapacity int
	LastSyncedAt           *time.Time
	NextSyncAt             *time.Time
	LastSyncImported       int
	LastSyncError          string
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// FallbackMode controls what happens when no primary egress node can be
// acquired for a request scope. The default is deliberately none so upgrades
// preserve the existing fail-closed behavior.
type FallbackMode string

const (
	FallbackModeNone   FallbackMode = "none"
	FallbackModeDirect FallbackMode = "direct"
	FallbackModeFixed  FallbackMode = "fixed"
)

func (value FallbackMode) IsValid() bool {
	switch value {
	case FallbackModeNone, FallbackModeDirect, FallbackModeFixed:
		return true
	default:
		return false
	}
}

// Normalized maps the zero value left by pre-fallback database rows to the
// conservative disabled mode.
func (value FallbackMode) Normalized() FallbackMode {
	if value == "" {
		return FallbackModeNone
	}
	return value
}

type FallbackConfig struct {
	Mode   FallbackMode
	NodeID uint64
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

// OperationsConfig controls background probe, account assignment, and egress
// fallback work. It defaults to a conservative disabled state for mutations
// and fallback routing.
type OperationsConfig struct {
	ProbeProvider             ProbeProvider
	ProbeIntervalSeconds      int
	AutoAssignEnabled         bool
	AutoBalanceEnabled        bool
	AssignmentIntervalSeconds int
	Fallbacks                 map[Scope]FallbackConfig
	// RouteRules routes provider traffic classes to dedicated egress targets.
	// An empty list keeps the historical scope-pool behavior unchanged.
	RouteRules []RouteRule
	UpdatedAt  time.Time
}

// TrafficClass names the operational purpose of one upstream call. Classes are
// declared by provider code at the call site; configuration only maps them to
// egress targets, so upstream host or path changes never invalidate rules.
type TrafficClass string

const (
	// TrafficClassInference covers account-facing generation requests such as
	// responses, chat, messages, compaction, and reasoning recovery. It is the
	// default for unannotated calls and always respects account node bindings.
	TrafficClassInference TrafficClass = "inference"
	// TrafficClassCredential covers OAuth token refresh and device
	// authorization exchanges.
	TrafficClassCredential TrafficClass = "credential"
	// TrafficClassBilling covers quota, billing, and subscription lookups.
	TrafficClassBilling TrafficClass = "billing"
	// TrafficClassModelSync covers account model catalog discovery.
	TrafficClassModelSync TrafficClass = "model_sync"
	// TrafficClassVideo covers video job submission, polling, and asset
	// downloads. Like inference it is account-tied generation traffic.
	TrafficClassVideo TrafficClass = "video"
)

// IsValid reports whether the value is a known traffic class.
func (value TrafficClass) IsValid() bool {
	switch value {
	case TrafficClassInference, TrafficClassCredential, TrafficClassBilling, TrafficClassModelSync, TrafficClassVideo:
		return true
	default:
		return false
	}
}

// RespectsAccountBinding reports whether an explicitly bound egress node wins
// over a configured route rule. Inference-style generation traffic keeps the
// account's stable exit IP; auxiliary background calls may follow the rule
// even for bound accounts, which is exactly where cheaper egress saves cost.
func (value TrafficClass) RespectsAccountBinding() bool {
	switch value {
	case TrafficClassInference, TrafficClassVideo:
		return true
	default:
		return false
	}
}

// RouteRuleTargetMode selects what a traffic class routes through.
type RouteRuleTargetMode string

const (
	RouteRuleTargetFixed  RouteRuleTargetMode = "fixed"
	RouteRuleTargetDirect RouteRuleTargetMode = "direct"
)

// IsValid reports whether the value is a known route rule target mode.
func (value RouteRuleTargetMode) IsValid() bool {
	return value == RouteRuleTargetFixed || value == RouteRuleTargetDirect
}

// Normalized maps the zero value left by pre-rule database rows to fixed so a
// stray rule with an empty mode cannot degrade into undefined behavior.
func (value RouteRuleTargetMode) Normalized() RouteRuleTargetMode {
	if value == "" {
		return RouteRuleTargetFixed
	}
	return value
}

// RouteRule pins one provider traffic class to a dedicated egress target:
// either a fixed egress node or a direct (no proxy) connection.
type RouteRule struct {
	Scope        Scope
	Class        TrafficClass
	TargetMode   RouteRuleTargetMode
	TargetNodeID uint64
	Enabled      bool
}

// RouteRuleFor returns the enabled rule for one scope and traffic class. At
// most one rule per (scope, class) is expected; when duplicates survive in
// stored data the first enabled match wins deterministically.
func (value OperationsConfig) RouteRuleFor(scope Scope, class TrafficClass) (RouteRule, bool) {
	for _, rule := range value.RouteRules {
		if !rule.Enabled || rule.Scope != scope || rule.Class != class {
			continue
		}
		// Normalize before comparing: the persistence layer normalizes both
		// ends, but a hand-edited row could still carry an empty mode. This
		// keeps the zero-node-id skip working for that defensive case too.
		if rule.TargetMode.Normalized() == RouteRuleTargetFixed && rule.TargetNodeID == 0 {
			continue
		}
		return rule, true
	}
	return RouteRule{}, false
}

// RouteRuleClasses lists traffic classes that may carry rules for the given
// scope. Only Grok Build consumes classes today; other scopes are rejected so
// configuration cannot silently point at unwired providers.
func RouteRuleClasses(scope Scope) []TrafficClass {
	if scope != ScopeBuild {
		return nil
	}
	return []TrafficClass{TrafficClassInference, TrafficClassCredential, TrafficClassBilling, TrafficClassModelSync, TrafficClassVideo}
}

// CanNodeServeFixedRouteTarget reports whether a node is schedulable as a
// fixed route-rule target for the supplied scope. This is the single
// authoritative predicate for the runtime, save-time, edit-guard, and
// subscription-hygiene paths; all of them must agree or a rule accepted by
// one path silently degrades at another. Sticky per-account templates are
// rejected separately at the application layer (they need decryption).
func CanNodeServeFixedRouteTarget(node Node, scope Scope) bool {
	return node.Enabled && node.EncryptedProxyURL != "" && SupportsScope(node.Scope, scope)
}

// ValidateRouteRules checks that the rule list is internally consistent:
// unique (scope, class) keys, valid enums, populated fixed targets, and only
// scopes whose providers actually consume classes.
// MaxRouteRules caps the persisted rule list. The (scope, class) uniqueness
// check below already bounds valid lists to five entries; the explicit cap is
// defense in depth so a degenerate payload fails fast before any per-rule
// node lookups run.
const MaxRouteRules = 16

func ValidateRouteRules(rules []RouteRule) error {
	if len(rules) > MaxRouteRules {
		return fmt.Errorf("出口路由规则最多 %d 条，收到 %d 条", MaxRouteRules, len(rules))
	}
	seen := make(map[Scope]map[TrafficClass]struct{}, len(rules))
	for _, rule := range rules {
		if rule.Scope != ScopeBuild {
			return fmt.Errorf("出口路由规则仅支持 %s 作用域，收到 %s", ScopeBuild, rule.Scope)
		}
		if !rule.Class.IsValid() {
			return fmt.Errorf("出口路由规则的流量类别无效: %q", rule.Class)
		}
		switch rule.TargetMode.Normalized() {
		case RouteRuleTargetFixed:
			if rule.TargetNodeID == 0 {
				return fmt.Errorf("出口路由规则 %s/%s 的固定出口节点不能为空", rule.Scope, rule.Class)
			}
		case RouteRuleTargetDirect:
			if rule.TargetNodeID != 0 {
				return fmt.Errorf("出口路由规则 %s/%s 的直连目标不能携带节点", rule.Scope, rule.Class)
			}
		default:
			return fmt.Errorf("出口路由规则 %s/%s 的目标类型无效: %q", rule.Scope, rule.Class, rule.TargetMode)
		}
		if _, exists := seen[rule.Scope]; !exists {
			seen[rule.Scope] = make(map[TrafficClass]struct{}, len(rules))
		}
		if _, duplicate := seen[rule.Scope][rule.Class]; duplicate {
			return fmt.Errorf("出口路由规则 %s/%s 重复", rule.Scope, rule.Class)
		}
		seen[rule.Scope][rule.Class] = struct{}{}
	}
	return nil
}

func DefaultOperationsConfig() OperationsConfig {
	return OperationsConfig{
		ProbeProvider:             ProbeProviderCloudflare,
		ProbeIntervalSeconds:      900,
		AssignmentIntervalSeconds: 300,
		Fallbacks: map[Scope]FallbackConfig{
			ScopeBuild:        {Mode: FallbackModeNone},
			ScopeWeb:          {Mode: FallbackModeNone},
			ScopeConsole:      {Mode: FallbackModeNone},
			ScopeWebAsset:     {Mode: FallbackModeNone},
			ScopeConsoleAsset: {Mode: FallbackModeNone},
		},
	}
}

// FallbackFor always returns a canonical, safe fallback value. It accepts
// sparse maps so older callers and historical records remain compatible.
func (value OperationsConfig) FallbackFor(scope Scope) FallbackConfig {
	fallback := value.Fallbacks[scope]
	fallback.Mode = fallback.Mode.Normalized()
	if fallback.Mode != FallbackModeFixed {
		fallback.NodeID = 0
	}
	return fallback
}

// SupportsScope reports whether a node can serve requests for the supplied
// scope. Console may intentionally reuse a Web browser proxy. Resource scopes
// may reuse their provider's primary node so explicit account bindings remain
// authoritative when no independently bound resource identity exists.
func SupportsScope(nodeScope, requestScope Scope) bool {
	if nodeScope == requestScope {
		return true
	}
	switch requestScope {
	case ScopeWebAsset, ScopeConsole:
		return nodeScope == ScopeWeb
	case ScopeConsoleAsset:
		return nodeScope == ScopeConsole || nodeScope == ScopeWeb
	default:
		return false
	}
}
