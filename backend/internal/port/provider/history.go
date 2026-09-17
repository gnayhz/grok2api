package provider

import (
	"context"
	"net/http"
	"strings"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
)

// HistoryExchange exposes interpreted upstream facts, retaining the unread
// response envelope for the adapter's ordinary conversion/delivery path.
type HistoryExchange struct {
	Response    *http.Response
	URL         string
	Prepared    historydomain.Prepared
	Rejection   historydomain.Rejection
	Accepted    bool
	RateLimited bool
}

// HistoryRecoveryRequest binds a rejection to its exact prepared history and
// same-account/same-plane sender. The provider supplies facts, never policy.
type HistoryRecoveryRequest struct {
	Input      historydomain.RecoveryInput
	Model, Key string
	History    historydomain.Service
	Original   HistoryExchange
	Retry      func(context.Context, historydomain.RecoveryStep) (HistoryExchange, error)
}

type HistoryRecoveryResult struct {
	Exchange HistoryExchange
	Outcome  historydomain.RecoveryOutcome
}

// HistoryController is supplied by the logical request owner. Both preparation
// and rejection recovery consume the same frozen policy. An absent controller
// gives no permission to discard prior history or retry a rejection.
type HistoryController interface {
	historydomain.InputAuthorizer
	Recover(context.Context, HistoryRecoveryRequest) HistoryRecoveryResult
}

// ApplyHistoryRecoveryWarnings encodes recovery facts without changing policy.
// The logical owner also calls it after account failover so a prior history loss
// cannot disappear merely because another account produced the final response.
func ApplyHistoryRecoveryWarnings(header http.Header, outcome historydomain.RecoveryOutcome) {
	if outcome.IdentityContextUnavailable {
		appendCompatibilityWarning(header, "history_identity_context_unavailable")
	}
	if outcome.OmittedCompactions > 0 {
		appendCompatibilityWarning(header, "foreign_compaction_omitted")
	}
	if outcome.RemovedOpaque > 0 {
		appendCompatibilityWarning(header, "reasoning_encrypted_content_downgraded")
	}
	if outcome.SessionHintCleared {
		appendCompatibilityWarning(header, "reasoning_session_reset")
	}
	if outcome.Failed {
		appendCompatibilityWarning(header, "reasoning_recovery_failed")
	}
}

func appendCompatibilityWarning(header http.Header, warning string) {
	if header == nil || strings.TrimSpace(warning) == "" {
		return
	}
	existing := strings.TrimSpace(header.Get("X-Grok2API-Compatibility-Warnings"))
	if existing == "" {
		header.Set("X-Grok2API-Compatibility-Warnings", warning)
		return
	}
	for _, value := range strings.Split(existing, ",") {
		if strings.TrimSpace(value) == warning {
			return
		}
	}
	header.Set("X-Grok2API-Compatibility-Warnings", existing+","+warning)
}
