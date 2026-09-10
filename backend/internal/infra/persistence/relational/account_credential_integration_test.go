package relational

import (
	"context"
	"errors"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAccountCredentialGenerationsAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			until := now.Add(time.Hour)
			initial, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "credential", SourceKey: "credential",
				EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", EncryptedCloudflareCookie: "cookie", ExpiresAt: now.Add(-time.Hour), AuthStatus: account.AuthStatusActive,
				FailureCount: 3, CooldownUntil: &until, LastError: account.LastErrorQualityIdle, RiskStatus: account.RiskStatusRSCDenied})
			if err != nil {
				t.Fatal(err)
			}
			get := func() account.Credential {
				t.Helper()
				v, err := rb.Get(ctx, initial.ID)
				if err != nil {
					t.Fatal(err)
				}
				return v
			}
			if initial.CredentialGeneration != 1 {
				t.Fatalf("new generation %d", initial.CredentialGeneration)
			}
			failure := account.CredentialEvent{Kind: account.CredentialRefreshFailed, OccurredAt: now, Failure: account.CredentialRefreshFailure{Status: 401, Code: "oauth_http_401"}}
			var wg sync.WaitGroup
			errs := make(chan error, 20)
			for i := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					_, err := repo.ApplyCredential(ctx, initial.CredentialRef(), failure)
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			failed := get()
			if failed.RefreshFailureCount != 20 || failed.RefreshUnclassifiedAuthCount != 20 || failed.AuthStatus != account.AuthStatusReauthRequired || failed.RefreshPermanent || failed.ReauthMarkedAt == nil || !failed.ReauthMarkedAt.Equal(now) {
				t.Fatalf("parallel failures: count=%d unknown=%d status=%s", failed.RefreshFailureCount, failed.RefreshUnclassifiedAuthCount, failed.AuthStatus)
			}
			if failed.FailureCount != 3 || failed.LastError != initial.LastError || failed.RiskStatus != initial.RiskStatus || failed.CredentialGeneration != initial.CredentialGeneration {
				t.Fatal("credential failure changed another state owner")
			}
			if _, err := ra.ApplyCredential(ctx, initial.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, OccurredAt: now.Add(time.Hour), Reason: strings.Repeat("拒", 600)}); err != nil {
				t.Fatal(err)
			}
			if current := get(); current.ReauthMarkedAt == nil || !current.ReauthMarkedAt.Equal(now) || len([]rune(current.AuthError)) != 512 {
				t.Fatal("rejection reset anchor or lost bounded diagnostic")
			}
			cookie, disabled := "cookie-2", false
			if _, err := rb.UpdateAdministration(ctx, initial.ID, repository.AccountAdminPatch{EncryptedCloudflareCookie: &cookie, AccountUpdates: repository.AccountUpdates{Enabled: &disabled}}); err != nil {
				t.Fatal(err)
			}
			if get().CredentialGeneration != initial.CredentialGeneration {
				t.Fatal("cookie/admin changed material generation")
			}
			// Explicit replacement clears auth and changes the generation even for the same material.
			replacement, _, err := ra.UpsertByIdentity(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			if replacement.CredentialGeneration != 2 || replacement.Enabled || replacement.AuthStatus != account.AuthStatusActive || replacement.AuthError != "" {
				t.Fatal("import changed admin intent or failed to replace auth")
			}
			replacement2, _, err := rb.UpsertByIdentity(ctx, initial)
			if err != nil {
				t.Fatal(err)
			}
			if replacement2.CredentialGeneration != 3 {
				t.Fatal("same-value import reused material generation")
			}
			var notifications atomic.Int32
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { notifications.Add(1) })
			rotate := account.CredentialEvent{Kind: account.CredentialRefreshed, OccurredAt: now, AccessToken: "rotated", RefreshToken: "rotated-refresh", ExpiresAt: now.Add(time.Hour), BuildBotFlagSource: 2}
			for _, event := range []account.CredentialEvent{failure, rotate, {Kind: account.CredentialRejected, Reason: "late"}} {
				result, err := ra.ApplyCredential(ctx, initial.CredentialRef(), event)
				if err != nil || result.Applied || result.Credential.CredentialGeneration != 3 {
					t.Fatalf("obsolete event kind=%s applied=%t err=%v", event.Kind, result.Applied, err)
				}
			}
			if notifications.Load() != 0 {
				t.Fatal("obsolete result emitted invalidation")
			}
			// Exactly one success from the captured material may install a replacement.
			var applied atomic.Int32
			errs = make(chan error, 12)
			for i := range 12 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					result, err := repo.ApplyCredential(ctx, replacement2.CredentialRef(), rotate)
					if result.Applied {
						applied.Add(1)
					}
					errs <- err
				}()
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Fatal(err)
				}
			}
			current := get()
			if applied.Load() != 1 || current.CredentialGeneration != 4 || current.EncryptedAccessToken != "rotated" || current.EncryptedRefreshToken != "rotated-refresh" || current.BuildBotFlagSource != 2 || current.AuthStatus != account.AuthStatusActive || current.RefreshFailureCount != 0 || current.LastRefreshAt == nil {
				t.Fatal("competing rotations did not install one version")
			}
			if current.Enabled || current.FailureCount != 3 || current.LastError != initial.LastError || current.RiskStatus != initial.RiskStatus {
				t.Fatal("success cleared unrelated account restrictions")
			}
			// Material hydration must carry the same generation as the encrypted tokens.
			enabled := true
			if _, err := ra.UpdateAdministration(ctx, initial.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &enabled}}); err != nil {
				t.Fatal(err)
			}
			material, err := rb.GetCredentialMaterial(ctx, current.ID, current.Provider)
			hydrated, ok := material.ApplyTo(account.Credential{ID: current.ID, Provider: current.Provider})
			if err != nil || !ok || hydrated.CredentialGeneration != 4 || hydrated.EncryptedAccessToken != "rotated" {
				t.Fatal("routing hydration detached generation from material")
			}
			wrong := current.CredentialRef()
			wrong.Provider = account.ProviderWeb
			if _, err := ra.ApplyCredential(ctx, wrong, failure); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("cross provider: %v", err)
			}
		})
	}
}

func TestAccountCredentialAtomicFailureAndOverflow(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			now := time.Now().UTC().Truncate(time.Microsecond)
			v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "atomic", SourceKey: "atomic", EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: now.Add(-time.Hour), AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			var events atomic.Int32
			ra.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events.Add(1) })
			failure := account.CredentialEvent{Kind: account.CredentialRefreshFailed, OccurredAt: now, Failure: account.CredentialRefreshFailure{Status: 400, Code: "invalid_grant", Permanent: true}}
			injected := errors.New("credential SQL unavailable")
			callback := "test:credential-failure"
			if err := a.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "account_credentials" {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, writeErr := ra.ApplyCredential(ctx, v.CredentialRef(), failure)
			if err := a.db.Callback().Update().Remove(callback); err != nil {
				t.Fatal(err)
			}
			stored, err := rb.Get(ctx, v.ID)
			if !errors.Is(writeErr, injected) || err != nil || stored.AuthStatus != account.AuthStatusActive || stored.ReauthMarkedAt != nil || stored.RefreshPermanent || stored.RefreshFailureCount != 0 || events.Load() != 0 {
				t.Fatal("failure partially committed authentication")
			}
			result, err := ra.ApplyCredential(ctx, v.CredentialRef(), failure)
			if err != nil || !result.Applied || result.Credential.AuthStatus != account.AuthStatusReauthRequired || !result.Credential.RefreshPermanent || events.Load() != 1 {
				t.Fatal("failure/auth did not commit together")
			}
			rotate := account.CredentialEvent{Kind: account.CredentialRefreshed, AccessToken: "new", ExpiresAt: now.Add(time.Hour)}
			if err := a.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "account_credentials" {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, writeErr = ra.ApplyCredential(ctx, v.CredentialRef(), rotate)
			if err := a.db.Callback().Update().Remove(callback); err != nil {
				t.Fatal(err)
			}
			stored, err = rb.Get(ctx, v.ID)
			if !errors.Is(writeErr, injected) || err != nil || stored.AuthStatus != account.AuthStatusReauthRequired || stored.CredentialGeneration != 1 || stored.EncryptedAccessToken != "access" || events.Load() != 1 {
				t.Fatal("rotation partially activated old credential")
			}
			if err := a.db.Model(&accountCredentialModel{}).Where("account_id = ?", v.ID).Update("generation", int64(math.MaxInt64)).Error; err != nil {
				t.Fatal(err)
			}
			stored, err = rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ra.ApplyCredential(ctx, stored.CredentialRef(), rotate); !errors.Is(err, account.ErrCredentialGenerationExhausted) {
				t.Fatalf("rotation overflow: %v", err)
			}
			if _, _, err := ra.UpsertByIdentity(ctx, v); !errors.Is(err, account.ErrCredentialGenerationExhausted) {
				t.Fatalf("import overflow: %v", err)
			}
			if err := a.db.Model(&accountCredentialModel{}).Where("account_id = ?", v.ID).Update("generation", -1).Error; err == nil {
				t.Fatal("SQL accepted negative generation")
			}
			after, err := rb.Get(ctx, v.ID)
			if err != nil || after.CredentialGeneration != math.MaxInt64 || after.AuthStatus != stored.AuthStatus || after.EncryptedAccessToken != stored.EncryptedAccessToken {
				t.Fatal("failed overflow changed state")
			}
		})
	}
}

func TestAccountCredentialLegacyGenerationMigration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra := NewAccountRepository(a)
			now := time.Now().UTC().Truncate(time.Microsecond)
			due := now.Add(time.Hour)
			v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "legacy", SourceKey: "legacy", EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: due, RefreshDueAt: &now, RefreshFailureCount: 3, RefreshPermanent: true, LastRefreshErrorCode: "invalid_grant", AuthStatus: account.AuthStatusReauthRequired, ReauthMarkedAt: &now, LastError: account.LastErrorMissingThinking})
			if err != nil {
				t.Fatal(err)
			}
			dropColumns := func() error {
				for _, column := range []struct {
					model            any
					constraint, name string
				}{{&accountCredentialModel{}, "chk_account_credentials_generation", "generation"}, {&accountModel{}, "chk_provider_accounts_auth_error", "auth_error"}} {
					if err := a.db.Migrator().DropConstraint(column.model, column.constraint); err != nil {
						return err
					}
					if err := a.db.Migrator().DropColumn(column.model, column.name); err != nil {
						return err
					}
				}
				return nil
			}
			if dialect == "sqlite" {
				err = a.withSQLiteForeignKeysDisabled(ctx, dropColumns)
			} else {
				err = dropColumns()
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			rb := NewAccountRepository(b)
			stored, err := rb.Get(ctx, v.ID)
			if err != nil || stored.CredentialGeneration != 0 || stored.EncryptedAccessToken != "access" || stored.EncryptedRefreshToken != "refresh" || stored.RefreshFailureCount != 3 || !stored.RefreshPermanent || stored.ReauthMarkedAt == nil || !stored.ReauthMarkedAt.Equal(now) || stored.LastError != v.LastError || stored.RefreshDueAt == nil || !stored.RefreshDueAt.Equal(now) {
				t.Fatalf("migration state generation=%d failures=%d permanent=%t auth=%s anchor=%v due=%v expected=%v health=%s err=%v", stored.CredentialGeneration, stored.RefreshFailureCount, stored.RefreshPermanent, stored.AuthStatus, stored.ReauthMarkedAt, stored.RefreshDueAt, now, stored.LastError, err)
			}
			result, err := rb.ApplyCredential(ctx, stored.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRefreshed, AccessToken: "new", RefreshToken: "new-refresh", ExpiresAt: due})
			if err != nil || !result.Applied || result.Credential.CredentialGeneration != 1 || result.Credential.AuthStatus != account.AuthStatusActive {
				t.Fatal("legacy zero generation did not rotate")
			}
			result, err = rb.ApplyCredential(ctx, stored.CredentialRef(), account.CredentialEvent{Kind: account.CredentialRejected, Reason: "legacy late"})
			if err != nil || result.Applied {
				t.Fatalf("legacy replay applied=%t current_generation=%d observed=%d err=%v", result.Applied, result.Credential.CredentialGeneration, stored.CredentialGeneration, err)
			}
		})
	}
}
