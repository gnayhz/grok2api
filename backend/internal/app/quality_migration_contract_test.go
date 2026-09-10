package app

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestLegacyQualityMigrationRespectsHealthClock(t *testing.T) {
	for _, revision := range []uint64{7, math.MaxInt64} {
		t.Run(time.Duration(revision).String(), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "migration.db")
			db, err := relational.OpenSQLite(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			reg, err := qualityregistry.Open(ctx, qualityregistry.Options{Driver: "sqlite", SQLitePath: path, AccountLinks: repo})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reg.Close() })
			row, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, Name: "migration", SourceKey: "migration", EncryptedAccessToken: "test-encrypted-token", Enabled: false, AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			if err := reg.DB().Table("provider_accounts").Where("id = ?", row.ID).Updates(map[string]any{"enabled": false, "health_revision": revision, "failure_count": 3, "last_error": accountdomain.LastErrorMissingThinkingDisabled, "cooldown_until": now.Add(time.Hour), "cooldown_marked_at": now}).Error; err != nil {
				t.Fatal(err)
			}
			err = repo.MigrateLegacyQualityHolds(ctx, now)
			after, readErr := repo.Get(ctx, row.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			var holds []journal.RestrictionRow
			if e := reg.DB().Find(&holds).Error; e != nil {
				t.Fatal(e)
			}
			var markers int64
			if e := reg.DB().Table("runtime_settings").Where("key = ?", "guard_owned_restrictions_v1").Count(&markers).Error; e != nil {
				t.Fatal(e)
			}
			if revision == math.MaxInt64 {
				if err == nil || after.HealthRevision != revision || after.LastError != accountdomain.LastErrorMissingThinkingDisabled || after.CooldownUntil == nil || len(holds) != 0 || markers != 0 {
					t.Fatalf("exhausted clock migration committed: err=%v revision=%d marker=%q holds=%d migration_markers=%d", err, after.HealthRevision, after.LastError, len(holds), markers)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if after.HealthRevision != revision+1 || after.Enabled || after.FailureCount != 3 || after.LastError != "" || after.CooldownUntil != nil || len(holds) != 1 || markers != 1 {
				t.Fatalf("unversioned migration: before=%d after=%d enabled=%v failures=%d marker=%q holds=%d migration_markers=%d", revision, after.HealthRevision, after.Enabled, after.FailureCount, after.LastError, len(holds), markers)
			}
		})
	}
}
