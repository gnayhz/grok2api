package relational

import (
	"context"
	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"testing"
	"time"
)

// Source node tests exercise the same reserved commit as the application.
func commitSourceNodesForTest(t *testing.T, repo *EgressRepository, ctx context.Context, id uint64, nodes []egress.Node) (int, error) {
	t.Helper()
	source, err := repo.GetEgressSource(ctx, id)
	if err != nil {
		return 0, err
	}
	claim, err := repo.BeginEgressSourceSync(ctx, source)
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	return repo.CommitEgressSourceSync(ctx, claim, nodes, now, now.Add(time.Duration(source.RefreshIntervalSeconds)*time.Second))
}

// Historical metadata combinations (including nonzero imports with an error)
// are seeded directly; no production writer is retained for arbitrary status.
func seedLegacySourceSyncMetadata(repo *EgressRepository, ctx context.Context, id uint64, synced, next time.Time, imported int, message string) error {
	return repo.db.db.WithContext(ctx).Model(&egressSubscriptionSourceModel{}).Where("id = ?", id).Updates(map[string]any{
		"last_synced_at": synced, "next_sync_at": next, "last_sync_imported": imported, "last_sync_error": message,
	}).Error
}
