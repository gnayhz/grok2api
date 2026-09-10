package relational

import (
	"context"
	"errors"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestLegacyQualityMigrationAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			if err := a.db.AutoMigrate(journal.Models()...); err != nil {
				t.Fatal(err)
			}
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Millisecond)
			var events atomic.Int32
			observe := func(_ context.Context, e repository.InvalidationEvent) {
				if e.Kind == repository.InvalidationAccountHealthChanged {
					if e.HealthRevision != 8 || e.FailureCount != 3 || e.CooldownUntil != nil {
						t.Errorf("incorrect notification: %+v", e)
					}
					events.Add(1)
				}
			}
			ra.SetInvalidationObserver(observe)
			rb.SetInvalidationObserver(observe)
			var ids []uint64
			markers := []string{account.LastErrorMissingThinking, account.LastErrorMissingThinkingDisabled, "upstream status 429", account.LastErrorQualityIdle}
			for i, marker := range markers {
				row, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: marker, SourceKey: marker, EncryptedAccessToken: testEncryptedToken, AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				ids = append(ids, row.ID)
				revision := uint64(7)
				if i == 1 {
					revision = math.MaxInt64
				}
				if err := a.db.Model(&accountModel{}).Where("id = ?", row.ID).Updates(map[string]any{"enabled": false, "health_revision": revision, "failure_count": 3, "last_error": marker, "cooldown_until": now.Add(time.Minute * time.Duration(i+1)), "cooldown_marked_at": now}).Error; err != nil {
					t.Fatal(err)
				}
			}
			assertRollback := func() {
				t.Helper()
				for i, id := range ids {
					row, err := rb.Get(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					if row.LastError != markers[i] || row.CooldownUntil == nil || row.FailureCount != 3 || row.Enabled {
						t.Fatalf("rollback changed account %d: %+v", id, row.HealthState())
					}
				}
				var holds, markers int64
				if err := b.db.Model(&journal.RestrictionRow{}).Count(&holds).Error; err != nil {
					t.Fatal(err)
				}
				if err := b.db.Model(&runtimeSettingsModel{}).Where("key = ?", "guard_owned_restrictions_v1").Count(&markers).Error; err != nil {
					t.Fatal(err)
				}
				if holds != 0 || markers != 0 || events.Load() != 0 {
					t.Fatalf("partial migration: holds=%d markers=%d events=%d", holds, markers, events.Load())
				}
			}
			if _, err := ra.ApplyHealth(ctx, ids[0], account.ProviderBuild, account.HealthEvent{Kind: account.HealthMigrateLegacyQuality}); err == nil {
				t.Fatal("ordinary health endpoint bypassed restriction transfer")
			}
			if err := ra.MigrateLegacyQualityHolds(ctx, now); err == nil {
				t.Fatal("overflow accepted")
			}
			assertRollback()
			if err := a.db.Model(&accountModel{}).Where("id = ?", ids[1]).Update("health_revision", 7).Error; err != nil {
				t.Fatal(err)
			}
			// Fail the restriction insert after the health transition has run.
			if err := a.db.Callback().Create().Before("gorm:create").Register("migration_fault", func(tx *gorm.DB) {
				if tx.Statement.Table == "q_guard_restriction" {
					tx.AddError(errors.New("injected restriction write failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			if err := ra.MigrateLegacyQualityHolds(ctx, now); err == nil {
				t.Fatal("restriction failure committed")
			}
			if err := a.db.Callback().Create().Remove("migration_fault"); err != nil {
				t.Fatal(err)
			}
			assertRollback()
			var wg sync.WaitGroup
			errs := make(chan error, 8)
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					errs <- repo.MigrateLegacyQualityHolds(ctx, now)
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := NewAccountRepository(b).MigrateLegacyQualityHolds(ctx, now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			for i, id := range ids {
				row, err := rb.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if row.Enabled || row.FailureCount != 3 {
					t.Fatalf("independent state changed: %+v", row.HealthState())
				}
				if i < 2 {
					if row.HealthRevision != 8 || row.LastError != "" || row.CooldownUntil != nil || row.CooldownMarkedAt != nil {
						t.Fatalf("migration failed: %+v", row.HealthState())
					}
				} else if row.HealthRevision != 7 || row.LastError != markers[i] || row.CooldownUntil == nil {
					t.Fatalf("unowned health changed: %+v", row.HealthState())
				}
			}
			var holds []journal.RestrictionRow
			if err := b.db.Order("account_id").Find(&holds).Error; err != nil {
				t.Fatal(err)
			}
			if len(holds) != 2 || events.Load() != 2 {
				t.Fatalf("holds=%d events=%d", len(holds), events.Load())
			}
			for i, hold := range holds {
				if hold.AccountID != ids[i] || !hold.ExpiresAt.Equal(now.Add(time.Minute*time.Duration(i+1))) {
					t.Fatalf("replay changed hold: %+v", hold)
				}
			}
			if holds[1].Reason != "legacy_disabled_review" {
				t.Fatal("lost manual review marker")
			}
		})
	}
}
