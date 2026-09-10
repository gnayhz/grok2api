package account

import (
	"errors"
	"strings"
	"time"
)

// QuotaConsumption identifies one confirmed upstream generation. SnapshotVersion
// is the local snapshot selected before generation, not an upstream timestamp.
// Zero means that the snapshot identity is unknown (including legacy jobs).
type QuotaConsumption struct {
	EventID         string
	AccountID       uint64
	Mode            string
	SnapshotVersion uint64
	Units           int
}

var ErrInvalidQuotaConsumption = errors.New("invalid quota consumption")

func (v QuotaConsumption) Validate() error {
	if len(v.EventID) == 0 || len(v.EventID) > 160 || strings.TrimSpace(v.EventID) != v.EventID ||
		v.AccountID == 0 || len(v.Mode) == 0 || len(v.Mode) > 64 || strings.TrimSpace(v.Mode) != v.Mode ||
		v.Units <= 0 || v.SnapshotVersion > 1<<63-1 {
		return ErrInvalidQuotaConsumption
	}
	return nil
}

// CanApply defines M07's local estimate policy. Weekly credits have no known
// unit conversion; unknown, replaced, or expired snapshots require a query.
func (v QuotaConsumption) CanApply(window QuotaWindow, now time.Time) bool {
	return v.Mode != "weekly" && v.SnapshotVersion != 0 && window.AccountID == v.AccountID && window.Mode == v.Mode &&
		window.SnapshotVersion == v.SnapshotVersion && (window.ResetAt == nil || window.ResetAt.After(now))
}

type QuotaConsumptionState string

const (
	QuotaConsumptionApplied        QuotaConsumptionState = "applied"
	QuotaConsumptionPendingRefresh QuotaConsumptionState = "pending_refresh"
	QuotaConsumptionRefreshed      QuotaConsumptionState = "refreshed"
	QuotaConsumptionAccountDeleted QuotaConsumptionState = "account_deleted"
)

// A pending receipt is a durable handoff to the account owner. Refreshed means
// that a later Provider query supplied the authoritative quota; it does not
// assert upstream per-event accounting or exactly-once execution.
type QuotaConsumptionReceipt struct {
	QuotaConsumption
	State      QuotaConsumptionState
	Projection *QuotaProjection
}

// QuotaProjection carries an absolute stored value. Consumers may compare its
// revision and read it; they do not calculate consumption or window transitions.
type QuotaProjection struct {
	Mode            string `json:"mode"`
	SnapshotVersion uint64 `json:"snapshotVersion"`
	Revision        uint64 `json:"revision"`
	Remaining       int    `json:"remaining"`
}

// PendingQuotaRefresh is the bounded projection used by the account worker.
type PendingQuotaRefresh struct {
	AccountID uint64
	Mode      string
}
