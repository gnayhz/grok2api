package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestLegacyQualityMigrationPreservesManualDisableAndIndependentHealth(t *testing.T) {
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
	r, err := qualityregistry.Open(ctx, qualityregistry.Options{Driver: "sqlite", SQLitePath: path, AccountLinks: repo})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	now := time.Now().UTC()
	for id, marker := range []string{accountdomain.LastErrorMissingThinking, accountdomain.LastErrorMissingThinkingDisabled, "upstream status 429", accountdomain.LastErrorQualityIdle} {
		row, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, Name: marker, SourceKey: marker, EncryptedAccessToken: "fixture-token", AuthStatus: accountdomain.AuthStatusActive})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.DB().Table("provider_accounts").Where("id = ?", row.ID).Updates(map[string]any{"enabled": id != 1, "last_error": marker, "cooldown_until": now.Add(12 * time.Hour), "cooldown_marked_at": now, "failure_count": 3}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.MigrateLegacyQualityHolds(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := repo.MigrateLegacyQualityHolds(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	var accounts []struct {
		ID            uint64
		Enabled       bool
		LastError     string
		CooldownUntil *time.Time
		FailureCount  int
	}
	if err := r.DB().Table("provider_accounts").Order("id").Find(&accounts).Error; err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.FailureCount != 3 {
			t.Fatal("migration changed independent failure count")
		}
		if account.ID <= 2 && (account.LastError != "" || account.CooldownUntil != nil) {
			t.Fatalf("legacy health not migrated: %+v", account)
		}
		if account.ID > 2 && (account.LastError == "" || account.CooldownUntil == nil) {
			t.Fatalf("independent health lost: %+v", account)
		}
		if account.ID == 2 && account.Enabled {
			t.Fatal("migration re-enabled an account with ambiguous manual ownership")
		}
	}
	var holds []journal.RestrictionRow
	if err := r.DB().Find(&holds).Error; err != nil {
		t.Fatal(err)
	}
	if len(holds) != 2 {
		t.Fatalf("holds=%d", len(holds))
	}
	for _, hold := range holds {
		if !hold.ExpiresAt.Equal(now.Add(2 * time.Minute)) {
			t.Fatalf("replay extended TTL: %+v", hold)
		}
	}
}
