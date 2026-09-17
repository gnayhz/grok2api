package physical

import (
	"context"
	"sync"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

type traceContextKey struct{}
type qualityVerificationContextKey struct{}
type nodeExclusionsContextKey struct{}

// Selection is the egress snapshot actually selected for an upstream request.
// It contains only metadata safe for audit.
//
// Pool and Connection are deliberate test-observable projections, not
// accidental write-only state. The egress tests assert them together with
// Lease.ConnectionPolicy() and the lease's own pool flag, so divergence between
// the tracing predicate, the scheduling predicate and the connection policy
// fails loudly. They are written by the egress manager and read by those
// assertions; delete them only if the assertions move to an equally direct
// production path.
type Selection struct {
	NodeID     uint64
	NodeName   string
	Scope      domain.Scope
	Proxied    bool
	Pool       bool
	Connection ConnectionPolicy
}

// SessionReuseDecision describes how networking used a soft connection hint.
type SessionReuseDecision string

const (
	SessionReuseNotRequested SessionReuseDecision = "not_requested"
	SessionReuseAccepted     SessionReuseDecision = "accepted"
	SessionReuseFresh        SessionReuseDecision = "rejected_fresh_connection"
	SessionReuseUnsupported  SessionReuseDecision = "unsupported_scope"
)

// ConnectionPolicy is the immutable policy of an acquired lease.
type ConnectionPolicy struct {
	AccountIsolated bool
	Fresh           bool
	SessionReuse    SessionReuseDecision
}

// Trace retains the most recent actual egress selection per scope.
type Trace struct {
	mu         sync.RWMutex
	selections map[domain.Scope]Selection
}

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

// Record appends an actual egress selection for a scope.
func (t *Trace) Record(value Selection) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.selections[value.Scope] = value
	t.mu.Unlock()
}

// WithNodeExclusions attaches request-scoped egress node IDs that must not serve this request.
func WithNodeExclusions(ctx context.Context, nodeIDs map[uint64]struct{}) context.Context {
	if ctx == nil || len(nodeIDs) == 0 {
		return ctx
	}
	return context.WithValue(ctx, nodeExclusionsContextKey{}, nodeIDs)
}

// NodeExcluded reports whether the node is excluded from this request.
func NodeExcluded(ctx context.Context, nodeID uint64) bool {
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

// WithQualityVerificationNode pins a call to a node and bypasses cooldown/exclusion guards.
// 出口质量 canary 验证的对象必然处于质量隔离冷却(L2 软冷却也可能仍在生效,
// 它们在 canary 判定通过/暂定放行时才被清除); 若钉住路径同样拒绝冷却节点,
// canary 永远无法执行, "验证通过→解除隔离"的回池链路整体失效。与受冷却与
// 探活等待约束的固定目标钉扎语义不同。
func WithQualityVerificationNode(ctx context.Context, nodeID uint64) context.Context {
	if ctx == nil || nodeID == 0 {
		return ctx
	}
	return context.WithValue(ctx, qualityVerificationContextKey{}, nodeID)
}

// QualityVerificationNode returns the pinned verification node ID, or zero.
func QualityVerificationNode(ctx context.Context) uint64 {
	if ctx == nil {
		return 0
	}
	value, _ := ctx.Value(qualityVerificationContextKey{}).(uint64)
	return value
}
