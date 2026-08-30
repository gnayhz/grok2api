package egress

import (
	"context"
	"fmt"
	"strings"
	"sync"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Selection is the egress snapshot actually selected for an upstream request. It contains only metadata safe for audit
// and excludes proxy URLs, credentials, User-Agent, and Cookies.
type Selection struct {
	NodeID   uint64
	NodeName string
	Scope    domain.Scope
	Proxied  bool
	// Pool marks a proxy-pool (rotating-endpoint) selection: consecutive
	// requests through the same node leave through DIFFERENT exit IPs, which
	// the Build risk probe relies on for its differential second attempt.
	Pool bool
}

// Trace retains the most recent actual egress selection per scope. When a request retries egress, audit records the final attempt.
// Web asset archival uses an independent scope and does not overwrite the primary Grok Web inference egress.
type Trace struct {
	mu         sync.RWMutex
	selections map[domain.Scope]Selection
}

type traceContextKey struct{}
type accountContextKey struct{}
type pinnedNodeContextKey struct{}
type qualityVerificationContextKey struct{}
type trafficClassContextKey struct{}
type nodeExclusionsContextKey struct{}

// WithTrafficClass labels one upstream call with its operational purpose so
// egress route rules can select a dedicated exit without matching URLs. Calls
// that carry no class default to inference semantics (account binding wins).
func WithTrafficClass(ctx context.Context, class domain.TrafficClass) context.Context {
	if ctx == nil || !class.IsValid() {
		return ctx
	}
	return context.WithValue(ctx, trafficClassContextKey{}, class)
}

// TrafficClassFromContext returns the call's traffic class, defaulting to
// inference for unannotated requests.
func TrafficClassFromContext(ctx context.Context) domain.TrafficClass {
	if ctx == nil {
		return domain.TrafficClassInference
	}
	if class, ok := ctx.Value(trafficClassContextKey{}).(domain.TrafficClass); ok && class.IsValid() {
		return class
	}
	return domain.TrafficClassInference
}

// WithAccount passes a stable Provider account identity to the egress layer. It is used only to render
// authentication usernames for sticky proxies such as Resin and is never written to upstream headers or audit.
func WithAccount(ctx context.Context, provider string, accountID uint64) context.Context {
	if ctx == nil || strings.TrimSpace(provider) == "" || accountID == 0 {
		return ctx
	}
	return WithAccountIdentity(ctx, strings.TrimSpace(provider)+fmt.Sprintf("%d", accountID))
}

// WithCredential passes the stable egress identity of a weakly linked account to Build transport;
// unlinked accounts retain the existing Provider+ID identity.
func WithCredential(ctx context.Context, credential accountdomain.Credential) context.Context {
	identity := strings.TrimSpace(credential.EgressIdentity)
	if identity == "" {
		provider := credential.Provider
		if provider == "" {
			provider = accountdomain.ProviderBuild
		}
		return WithAccount(ctx, string(provider), credential.ID)
	}
	return WithAccountIdentity(ctx, identity)
}

// WithPinnedNode pins one upstream call to a specific node, bypassing routing
// but still honoring cooldowns, degrade-guard exclusions and probe waits.
// It is set by degraded same-account retries (the retry re-enters the same exit
// unless it is cooling) and by tests; exit-IP quality verification instead uses
// WithQualityVerificationNode, which bypasses those guards.
func WithPinnedNode(ctx context.Context, nodeID uint64) context.Context {
	if ctx == nil || nodeID == 0 {
		return ctx
	}
	return context.WithValue(ctx, pinnedNodeContextKey{}, nodeID)
}

func pinnedNodeFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	value, _ := ctx.Value(pinnedNodeContextKey{}).(uint64)
	return value
}

// WithQualityVerificationNode 把一次调用钉到受检节点并绕过冷却/排除守卫。
// 出口质量 canary 验证的对象必然处于质量隔离冷却(L2 软冷却也可能仍在生效,
// 它们在 canary 判定通过/暂定放行时才被清除); 若钉住路径同样拒绝冷却节点,
// canary 永远无法执行, "验证通过→解除隔离"的回池链路整体失效。与
// WithPinnedNode(降智同号重试, 仍受冷却与探活等待约束)语义不同。
func WithQualityVerificationNode(ctx context.Context, nodeID uint64) context.Context {
	if ctx == nil || nodeID == 0 {
		return ctx
	}
	return context.WithValue(ctx, qualityVerificationContextKey{}, nodeID)
}

func qualityVerificationNodeFromContext(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	value, _ := ctx.Value(qualityVerificationContextKey{}).(uint64)
	return value
}

// account-bound proxy templates such as Resin. Providers that represent the
// same upstream login (for example Web and Console sharing one SSO token) can
// deliberately pass the same identity so their proxy and clearance lease is
// not split by the internal provider name.
func WithAccountIdentity(ctx context.Context, identity string) context.Context {
	if ctx == nil || strings.TrimSpace(identity) == "" {
		return ctx
	}
	return context.WithValue(ctx, accountContextKey{}, strings.TrimSpace(identity))
}

func accountFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(accountContextKey{}).(string)
	return strings.TrimSpace(value)
}

// AccountFromContext exposes the non-sensitive sticky account identity to
// provider transports while keeping the context key private.
func AccountFromContext(ctx context.Context) string { return accountFromContext(ctx) }

// WithTrace creates or reuses a concurrency-safe egress selection trace for one gateway request.
func WithTrace(ctx context.Context) (context.Context, *Trace) {
	if existing := TraceFromContext(ctx); existing != nil {
		return ctx, existing
	}
	trace := &Trace{selections: make(map[domain.Scope]Selection)}
	return context.WithValue(ctx, traceContextKey{}, trace), trace
}

// TraceFromContext returns the egress trace from context, or nil when none is configured.
func TraceFromContext(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(traceContextKey{}).(*Trace)
	return trace
}

// Selection returns a safe snapshot of the most recent actual egress selection for a scope.
func (t *Trace) Selection(scope domain.Scope) (Selection, bool) {
	if t == nil {
		return Selection{}, false
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	value, ok := t.selections[scope]
	return value, ok
}

// Record appends an actual egress selection for a scope. It is the exported
// counterpart of the manager-internal recordSelection: provider test doubles
// use it to seed the trace (e.g. rotating-pool selections) so request-path
// policies that depend on the egress shape can be exercised end to end.
func (t *Trace) Record(value Selection) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.selections[value.Scope] = value
	t.mu.Unlock()
}

// WithNodeExclusions attaches the request-scoped set of egress node IDs that
// must not serve this request. The real-time quality guard populates it after
// a degraded attempt so the in-request retry lands on a different fixed exit
// IP instead of re-entering the same degraded node (account bindings included).
func WithNodeExclusions(ctx context.Context, nodeIDs map[uint64]struct{}) context.Context {
	if ctx == nil || len(nodeIDs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, nodeExclusionsContextKey{}, nodeIDs)
}

func nodeExcluded(ctx context.Context, nodeID uint64) bool {
	if ctx == nil || nodeID == 0 {
		return false
	}
	excluded, ok := ctx.Value(nodeExclusionsContextKey{}).(map[uint64]struct{})
	if !ok {
		return false
	}
	_, hit := excluded[nodeID]
	return hit
}

func recordSelection(ctx context.Context, value Selection) {
	trace := TraceFromContext(ctx)
	if trace == nil {
		return
	}
	trace.mu.Lock()
	trace.selections[value.Scope] = value
	trace.mu.Unlock()
}
