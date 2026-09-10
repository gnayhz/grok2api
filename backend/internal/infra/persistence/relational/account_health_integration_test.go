package relational

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestAccountHealthAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			initial, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "health", SourceKey: "health", EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			id := initial.ID
			get := func() account.Credential {
				t.Helper()
				v, err := rb.Get(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			apply := func(repo *AccountRepository, event account.HealthEvent) account.HealthResult {
				t.Helper()
				v, err := repo.ApplyHealth(ctx, id, initial.Provider, event)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			failure := account.HealthEvent{Kind: account.HealthFailure, Status: 429, CooldownBase: time.Second, CooldownMax: time.Minute}
			// Both connections complete requests selected from the same old snapshot.
			var wg sync.WaitGroup
			results := make(chan error, 12)
			for i := range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					_, err := repo.ApplyHealth(ctx, id, initial.Provider, failure)
					results <- err
				}()
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			current := get()
			if current.FailureCount != 12 || current.HealthRevision != 12 {
				t.Fatalf("concurrent state: failures=%d revision=%d", current.FailureCount, current.HealthRevision)
			}
			// A result delivered late cannot clear a newer write, including after
			// a successful header but before the stream's failure is persisted.
			if got := apply(ra, account.HealthEvent{Kind: account.HealthSuccess, ObservedRevision: 0}); got.Applied || got.State.FailureCount != 12 {
				t.Fatalf("late success: %+v", got)
			}
			success := apply(rb, account.HealthEvent{Kind: account.HealthSuccess, ObservedRevision: current.HealthRevision})
			if !success.Applied || success.State.FailureCount != 0 {
				t.Fatalf("current success: %+v", success)
			}
			apply(ra, failure)
			stream := failure
			stream.AfterSuccess = true
			stream.ObservedRevision = success.State.Revision
			if got := apply(rb, stream); got.State.FailureCount != 2 {
				t.Fatalf("stream erased newer failure: %+v", got)
			}
			// Admin intent, auth, risk and token material survive health writes.
			admin := get()
			admin.Enabled = false
			admin.AuthStatus = account.AuthStatusReauthRequired
			admin.RiskStatus = account.RiskStatusRSCDenied
			if _, err := ra.UpdateAdministration(ctx, admin.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &admin.Enabled}, Risk: &repository.RiskAttribution{Status: admin.RiskStatus}}); err != nil {
				t.Fatal(err)
			}
			if _, err := ra.ApplyCredential(ctx, admin.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, Reason: "fixture rejected"}); err != nil {
				t.Fatal(err)
			}
			beforeIdle := apply(rb, failure).State
			idle := apply(ra, account.HealthEvent{Kind: account.HealthQualityIdle, RetryAfter: time.Hour})
			if idle.State.FailureCount != beforeIdle.FailureCount {
				t.Fatalf("idle lost count: %+v", idle)
			}
			if got := get(); got.Enabled || got.AuthStatus != account.AuthStatusReauthRequired || got.RiskStatus != account.RiskStatusRSCDenied || got.EncryptedAccessToken != initial.EncryptedAccessToken {
				t.Fatal("health modified independent state")
			}
			// The administrator patch cannot roll back health or its SQL revision.
			admin.Name = "renamed"
			if _, err := rb.UpdateAdministration(ctx, admin.ID, repository.AccountAdminPatch{Name: &admin.Name}); err != nil {
				t.Fatal(err)
			}
			if got := get(); got.HealthRevision != idle.State.Revision || got.LastError != account.LastErrorQualityIdle || got.Name != "renamed" {
				t.Fatalf("edit overwrote health: revision=%d marker=%s", got.HealthRevision, got.LastError)
			}
			if got := apply(rb, account.HealthEvent{Kind: account.HealthClearQuality, MinHold: time.Hour}); got.Applied {
				t.Fatal("fresh quality cooldown cleared before hold")
			}
			// Token rotation does not erase the quality marker or health version.
			if _, err := rotateOAuthFixture(ra, ctx, id, "rotated-access", "rotated-refresh", time.Now().Add(time.Hour), 0); err != nil {
				t.Fatal(err)
			}
			if got := get(); got.HealthRevision != idle.State.Revision || got.LastError != account.LastErrorQualityIdle {
				t.Fatal("credential refresh erased health")
			}
			if got := apply(rb, account.HealthEvent{Kind: account.HealthClearQuality}); !got.Applied || got.State.LastError != "" || got.State.CooldownUntil != nil {
				t.Fatalf("quality clear: %+v", got)
			}
			apply(ra, failure)
			if got := apply(rb, account.HealthEvent{Kind: account.HealthClearQuality}); got.Applied {
				t.Fatal("quality clear erased generic failure")
			}
			// Reimport preserves the current health row, clock and hold timestamp.
			before := get()
			if _, _, err := ra.UpsertByIdentity(ctx, initial); err != nil {
				t.Fatal(err)
			}
			if got := get(); got.HealthRevision != before.HealthRevision || got.FailureCount != before.FailureCount || got.CooldownMarkedAt == nil || !got.CooldownMarkedAt.Equal(*before.CooldownMarkedAt) {
				t.Fatal("reimport changed health")
			}
			// An invalid event and an exhausted persistent clock roll back fully.
			if _, err := rb.ApplyHealth(ctx, id, initial.Provider, account.HealthEvent{Kind: "unknown"}); err == nil {
				t.Fatal("invalid event accepted")
			}
			if got := get(); got.HealthRevision != before.HealthRevision {
				t.Fatal("invalid event advanced clock")
			}
			if err := a.db.Model(&accountModel{}).Where("id = ?", id).Update("health_revision", int64(math.MaxInt64)).Error; err != nil {
				t.Fatal(err)
			}
			if _, err := ra.ApplyHealth(ctx, id, initial.Provider, failure); err == nil {
				t.Fatal("exhausted clock accepted")
			}
			if got := get(); got.HealthRevision != math.MaxInt64 || got.FailureCount != before.FailureCount {
				t.Fatal("exhausted clock changed state")
			}
			if err := ra.ResetQuotaState(ctx, initial.Provider, []uint64{id}); err != nil {
				t.Fatalf("quota reset depends on unrelated health clock: %v", err)
			}
			if got := get(); got.HealthRevision != math.MaxInt64 || got.FailureCount != before.FailureCount {
				t.Fatal("quota reset changed health")
			}
			if _, err := ra.ApplyHealth(ctx, id, account.ProviderWeb, failure); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("cross-provider write: %v", err)
			}
		})
	}
}

func TestAccountHealthLegacyMigration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewAccountRepository(a)
			old := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
			until := old.Add(3 * time.Hour)
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "old-health", SourceKey: "old-health", EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive, FailureCount: 3, CooldownUntil: &until, CooldownMarkedAt: &old, LastError: account.LastErrorQualityIdle})
			if err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().DropConstraint(&accountModel{}, "chk_accounts_health_revision"); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().DropColumn(&accountModel{}, "health_revision"); err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			rb := NewAccountRepository(b)
			got, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.HealthRevision != 0 || got.FailureCount != 3 || got.CooldownMarkedAt == nil || !got.CooldownMarkedAt.Equal(old) {
				t.Fatal("migration changed legacy health")
			}
			result, err := rb.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthClearQuality, MinHold: 30 * time.Minute})
			if err != nil || !result.Applied || result.State.Revision != 1 {
				t.Fatalf("adopt legacy: %+v %v", result, err)
			}
			base, err := rb.ListRoutingAccountBases(ctx, v.Provider, "")
			if err != nil || len(base) != 1 || base[0].Credential.HealthRevision != 1 {
				t.Fatal(fmt.Sprintf("routing lost durable version: %v %v", base, err))
			}
		})
	}
}

func TestAccountHealthConcurrentMaintenance(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "maintenance", SourceKey: "maintenance", EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			results := make(chan error, 18)
			var wg sync.WaitGroup
			for i := range 18 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var err error
					switch i % 3 {
					case 0:
						_, err = ra.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthFailure, Status: 429, CooldownBase: time.Second, CooldownMax: time.Minute})
					case 1:
						_, err = rotateOAuthFixture(rb, ctx, v.ID, "rotated-access", "rotated-refresh", time.Now().Add(time.Hour), 0)
					case 2:
						_, err = rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{Name: &v.Name})
					}
					results <- err
				}()
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			got, err := ra.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.FailureCount != 6 || got.HealthRevision != 6 {
				t.Fatalf("maintenance lost health: failures=%d revision=%d", got.FailureCount, got.HealthRevision)
			}
		})
	}
}
