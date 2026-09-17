package enforcement

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// StateStore exposes the atomic operations this use case requires.
// Implementations must validate current coordination ownership and generations.
type StateStore interface {
	ObserveExitIdentity(ctx context.Context, nodeID uint64, identity model.ExitIdentity, revision uint64) (oldEpoch, newEpoch uint64, released []model.EpochKey, err error)
	ListBannedExits() []model.EpochKey
	ReleaseCurrentExitAfterReview(ctx context.Context, nodeID uint64, reason string) error
	ExitIPAt(ctx context.Context, nodeID, epoch uint64) (model.ExitIPRecord, bool, error)
	AppendDegrade(ctx context.Context, nodeID, epoch uint64, ip string, at time.Time) error
}
