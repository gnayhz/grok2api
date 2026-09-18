package history

import (
	"context"
	"errors"
	"fmt"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Commit diagnostics contain only locally defined categories. Decoder and SQL
// errors may contain response data or connection details, so their text must not
// be written to logs. Unwrap retains the cause for cancellation and CAS checks.
type journalCommitError struct {
	stage, reason string
	cause         error
}

func (e *journalCommitError) Error() string {
	return fmt.Sprintf("%s: %s: %s", historydomain.ErrHistoryCommit, e.stage, e.reason)
}

func (e *journalCommitError) Unwrap() []error {
	if e.cause == nil {
		return []error{historydomain.ErrHistoryCommit}
	}
	return []error{historydomain.ErrHistoryCommit, e.cause}
}

func (e *journalCommitError) HistoryFailureDiagnostic() (string, string) {
	return e.stage, e.reason
}

func journalCommitFailure(stage, reason string, cause error) error {
	return &journalCommitError{stage: stage, reason: reason, cause: cause}
}

func journalFailureReason(err error) string {
	if kind, ok := repository.StoreFaultKindOf(err); ok {
		return "store_" + string(kind)
	}
	switch {
	case errors.Is(err, responsebuffer.ErrLimit):
		return "size_limit"
	case errors.Is(err, responsebuffer.ErrExhausted):
		return "resource_exhausted"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, repository.ErrConflict):
		return "conflict"
	default:
		return historydomain.HistoryFailureReason(err)
	}
}

func (p *PreparedHistory) observeCommitFailure(err error) {
	if err == nil {
		return
	}
	stage, reason := "commit", journalFailureReason(err)
	var failure *journalCommitError
	if errors.As(err, &failure) {
		stage, reason = failure.stage, failure.reason
	}
	p.replay.logger.Warn("conversation_history_commit_failed", "scope_hash", p.ScopeHash(),
		"generation", p.Generation(), "stage", stage, "reason", reason,
		"normalizer", historydomain.JournalNormalizerVersion)
}
