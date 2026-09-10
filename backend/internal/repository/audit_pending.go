package repository

import (
	"context"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

var (
	ErrAuditPendingFull   = errors.New("audit pending storage is full")
	ErrAuditPendingOwner  = errors.New("audit pending storage is already owned")
	ErrAuditPendingFormat = errors.New("audit pending storage format is unsupported")
)

type AuditPendingEntry struct {
	ID          uint64
	Record      audit.Record
	Rejected    bool
	DecodeError error
}

// AuditPendingSnapshot counts all retained payloads, including rejected ones.
// Limits never authorize evicting a previously accepted entry.
type AuditPendingSnapshot struct {
	Revision   uint64
	Records    int
	Rejected   int
	Bytes      int64
	MaxRecords int
	MaxBytes   int64
}

// AuditPendingStore is the durable handoff owned by M19. Append completes only
// after persistence, and retains the first pending payload for an event/Key.
// Acknowledge is called only after the authoritative audit transaction commits.
// Storage ownership/Close belongs to the composition root, after M19 stops.
type AuditPendingStore interface {
	Append(ctx context.Context, value audit.Record) (AuditPendingEntry, error)
	ReadPending(ctx context.Context, after uint64, limit int) ([]AuditPendingEntry, error)
	PendingEventIDs(ctx context.Context, after uint64, limit int) ([]AuditPendingEntry, error)
	Acknowledge(ctx context.Context, ids []uint64) error
	Reject(ctx context.Context, id uint64, reason string) error
	RetryRejected(ctx context.Context) error
	Snapshot() AuditPendingSnapshot
}
