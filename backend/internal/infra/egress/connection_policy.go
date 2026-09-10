package egress

import (
	"context"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// SessionReuseDecision describes how networking used a soft connection hint.
// It says nothing about historical continuity or upstream cache hits.
type SessionReuseDecision string

const (
	SessionReuseNotRequested SessionReuseDecision = "not_requested"
	SessionReuseAccepted     SessionReuseDecision = "accepted"
	SessionReuseFresh        SessionReuseDecision = "rejected_fresh_connection"
	SessionReuseUnsupported  SessionReuseDecision = "unsupported_scope"
)

// ConnectionPolicy is the immutable policy of an acquired lease. Isolation
// partitions all reusable clients, including session clients. A policy update
// retires old clients for new acquisitions; existing leases drain normally.
type ConnectionPolicy struct {
	AccountIsolated bool
	Fresh           bool
	SessionReuse    SessionReuseDecision
}

func (l *Lease) ConnectionPolicy() ConnectionPolicy {
	if l == nil {
		return ConnectionPolicy{}
	}
	return l.connectionPolicy
}

func resolveConnectionOptions(scope domain.Scope, options clientOptions) (clientOptions, SessionReuseDecision) {
	options.sessionKey = strings.TrimSpace(options.sessionKey)
	decision := SessionReuseNotRequested
	if options.sessionKey != "" {
		decision = SessionReuseAccepted
	}
	if scope != domain.ScopeBuild {
		if options.sessionKey != "" {
			decision = SessionReuseUnsupported
		}
		options.sessionKey, options.onSessionDial, options.freshTunnel = "", nil, false
	} else if options.freshTunnel && options.sessionKey != "" {
		decision = SessionReuseFresh
		options.sessionKey, options.onSessionDial = "", nil
	}
	return options, decision
}

func buildSessionForScope(ctx context.Context, scope domain.Scope) string {
	if scope == domain.ScopeBuild {
		return buildSessionFromContext(ctx)
	}
	return ""
}
