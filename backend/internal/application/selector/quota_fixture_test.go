package selector

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Quota fixture installation has no in-flight Provider query. Production
// callers must capture the revision before fetching and use SaveQuotaSnapshot.
func saveQuotaWindowsFixture(repo repository.AccountRepository, ctx context.Context, id uint64, tier account.WebTier, now time.Time, windows []account.QuotaWindow) error {
	revision, err := repo.GetQuotaRevision(ctx, id)
	if err != nil {
		return err
	}
	return repo.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: id, Revision: revision, Tier: tier, SyncedAt: now, Windows: windows})
}
