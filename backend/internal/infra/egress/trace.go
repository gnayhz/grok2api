package egress

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

type (
	Selection = physical.Selection
	Trace     = physical.Trace
)

// buildSessionContextKey carries a soft Build reuse hint. Pool strategy,
// eligibility, account isolation and fresh connections remain network policy.
type buildSessionContextKey struct{}

type accountContextKey struct{}
type pinnedNodeContextKey struct{}
type trafficClassContextKey struct{}

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
// but still honoring cooldowns, degrade-guard exclusions and probe waits. It
// is a live-test/debug pinning entry (not on the main request path);
// exit-IP quality verification instead uses WithQualityVerificationNode,
// which bypasses those guards.
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

func qualityVerificationNodeFromContext(ctx context.Context) uint64 {
	return physical.QualityVerificationNode(ctx)
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

// WithBuildSession supplies a soft network reuse hint, separate from history
// identity and the upstream cache key. Only a domain-separated digest is kept
// in process-local pins and diagnostics; it is never sent in request headers.
func WithBuildSession(ctx context.Context, sessionKey string) context.Context {
	if ctx == nil {
		return ctx
	}
	trimmed := strings.TrimSpace(sessionKey)
	if trimmed == "" {
		return ctx
	}
	digest := sha256.Sum256([]byte("build-session:" + trimmed))
	return context.WithValue(ctx, buildSessionContextKey{}, hex.EncodeToString(digest[:12]))
}

// buildSessionFromContext 返回会话摘要;空串表示该调用不参与会话钉扎
// (模型同步/计费/探针等无会话语义的调用保持原行为)。
func buildSessionFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(buildSessionContextKey{}).(string)
	return value
}

func TraceFromContext(ctx context.Context) *Trace {
	return physical.TraceFromContext(ctx)
}

func nodeExcluded(ctx context.Context, nodeID uint64) bool {
	return physical.NodeExcluded(ctx, nodeID)
}

func recordSelection(ctx context.Context, value Selection) {
	if trace := TraceFromContext(ctx); trace != nil {
		trace.Record(value)
	}
}
