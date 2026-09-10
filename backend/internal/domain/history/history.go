// Package history defines conversation continuity contracts. Implementations own
// lineage and state transitions; callers supply upstream facts and execution policy.
package history

import (
	"context"
	"errors"
	"io"
)

const JournalNormalizerVersion = 1

var ErrHistoryCommit = errors.New("history_commit_failed")
var ErrHistoryPrepare = errors.New("history_prepare_failed")

// Prepared owns one reserved turn. Discard releases its writer without deleting
// the parent chain. Reset advances only its still-current generation.
type Prepared interface {
	Outcome() string
	Generation() int64
	RestoredItems() int
	ScopeHash() string
	Capture(io.ReadCloser, bool) (io.ReadCloser, func() error, func())
	Discard()
	Reset() error
}

// Service is the history port consumed by upstream adapters. It contains no
// network, retry-count, account-selection or client-delivery policy.
type Service interface {
	Enabled() bool
	Persistent() bool
	Apply(context.Context, string, string, []byte) []byte
	Prepare(context.Context, string, string, []byte, ...ReplayPreparation) ([]byte, Prepared, error)
	CaptureBody(io.ReadCloser, string, string, bool, bool) io.ReadCloser
	CapturePendingBody(io.ReadCloser, string, string, bool, bool) (io.ReadCloser, func())
	ApplyRecovery(context.Context, Prepared, string, string, RecoveryStep) error
}

func Discard(p Prepared) {
	if p != nil {
		p.Discard()
	}
}

var (
	ErrHistoryAmbiguous = errors.New("ambiguous_lineage")
	ErrHistoryStale     = errors.New("stale_generation")
	ErrHistoryMissing   = errors.New("durable_history_missing")
	ErrHistoryQuota     = errors.New("history_quota_denied")
)

func HistoryFailureReason(err error) string {
	switch {
	case errors.Is(err, ErrHistoryStale):
		return "stale_generation"
	case errors.Is(err, ErrHistoryQuota):
		return "quota_denied"
	case errors.Is(err, ErrHistoryAmbiguous):
		return "ambiguous_lineage"
	case errors.Is(err, ErrHistoryMissing):
		return "durable_history_missing"
	default:
		return "store_error"
	}
}

// ReplayPreparation separates authenticated compatibility keys from ambiguous
// prior keys. PriorKeys may be inspected for loss, never used as replay sources.
type ReplayPreparation struct {
	LegacyKeys []string
	PriorKeys  []string
	Authorizer InputAuthorizer
}
type InputAuthorizer interface {
	PrepareInput(context.Context, InputPreparation) error
}
type InputPreparation struct {
	UnavailableCompactions     int
	IdentityContextUnavailable bool
}

var ErrIdentityLossNotAuthorized = errors.New("history identity changed and prior opaque context is unavailable")
