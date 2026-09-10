package repository

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// ClientKeyRepository 定义下游 API Key 持久化能力。
type ClientKeyRepository interface {
	List(ctx context.Context, query ClientKeyListQuery) ([]clientkey.Key, int64, error)
	Create(ctx context.Context, value clientkey.Key) (clientkey.Key, error)
	Get(ctx context.Context, id uint64) (clientkey.Key, error)
	GetByPrefix(ctx context.Context, prefix string) (clientkey.Key, error)
	Patch(ctx context.Context, id uint64, patch clientkey.ManagementPatch) (clientkey.Key, error)
	UpdateManyEnabled(ctx context.Context, ids []uint64, enabled bool) (int64, error)
	// Deletion is atomic with permissions, reservations, and associated media
	// jobs/tickets. M17's deletion policy must accept every associated job or the
	// whole command returns ErrConflict without deleting any rows. Internal keys
	// are excluded. Missing IDs are ignored in DeleteMany; Delete returns ErrNotFound.
	Delete(ctx context.Context, id uint64) error
	DeleteMany(ctx context.Context, ids []uint64) (int64, error)
	Touch(ctx context.Context, id uint64) error
}

// BillingReservationScope identifies the durable instance responsible for expiry
// recovery. ProtectedEventIDs adds its live/recovered events; it cannot authorize
// reclaiming reservations owned by another instance or with unknown ownership.
type BillingReservationScope struct {
	OwnerID           string
	ProtectedEventIDs []string
}
