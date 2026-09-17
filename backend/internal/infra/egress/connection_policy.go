package egress

import (
	"context"
	"strings"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

type SessionReuseDecision = physical.SessionReuseDecision
type ConnectionPolicy = physical.ConnectionPolicy

const (
	SessionReuseNotRequested = physical.SessionReuseNotRequested
	SessionReuseAccepted     = physical.SessionReuseAccepted
	SessionReuseFresh        = physical.SessionReuseFresh
	SessionReuseUnsupported  = physical.SessionReuseUnsupported
)

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
