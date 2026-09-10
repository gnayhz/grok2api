package relational

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAccountAdministrationAcrossConnections(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "original", SourceKey: "admin", Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "old-access", EncryptedRefreshToken: "old-refresh"})
			if err != nil {
				t.Fatal(err)
			}
			// Twenty writers from two independent pools own disjoint field groups.
			// Every health failure must count, and no name/priority command may
			// revert current credential, auth, admin, or risk fields.
			name, priority, enabled := "renamed", 9, false
			errs := make(chan error, 20)
			var wg sync.WaitGroup
			for i := range 20 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var err error
					switch i % 5 {
					case 0:
						_, err = ra.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{Name: &name})
					case 1:
						_, err = rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Priority: &priority, Enabled: &enabled}})
					case 2:
						_, err = ra.ApplyHealth(ctx, v.ID, v.Provider, account.HealthEvent{Kind: account.HealthFailure, Status: 429, CooldownBase: time.Second, CooldownMax: time.Minute})
					case 3:
						_, err = rotateOAuthFixture(rb, ctx, v.ID, "new-access", "new-refresh", time.Now().Add(time.Hour), 0)
					case 4:
						err = rb.UpdateRiskAttribution(ctx, v.ID, repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: account.RiskTriggerDegrade, Detail: "concurrent risk"})
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
			current, err := ra.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Name != name || current.Priority != priority || current.Enabled || current.FailureCount != 4 || current.HealthRevision != 4 || current.EncryptedAccessToken != "new-access" || current.EncryptedRefreshToken != "new-refresh" || current.RiskDetail != "concurrent risk" || current.RiskTrigger != account.RiskTriggerDegrade {
				t.Fatal("independent admin/health/token/risk writes did not all survive")
			}
			// Explicit false/zero and Build fields retain their existing meaning.
			zero, remaining, one, entitled, route := 0, 0.0, 1, true, account.BuildRouteXAI
			out, err := rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Enabled: &enabled, Priority: &zero, MaxConcurrent: &one, MinimumRemaining: &remaining}, BuildSuperEntitled: &entitled, BuildRouteMode: &route})
			if err != nil {
				t.Fatal(err)
			}
			if out.EnabledChanged || out.Credential.Priority != 0 || out.Credential.MaxConcurrent != 1 || !out.Credential.BuildSuperEntitled || out.Credential.BuildRouteMode != route {
				t.Fatal("explicit admin values lost")
			}
			// Updating an empty patch is a read and emits no invalidation.
			var events int
			rb.SetInvalidationObserver(func(context.Context, repository.InvalidationEvent) { events++ })
			if _, err := rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{}); err != nil {
				t.Fatal(err)
			}
			if events != 0 {
				t.Fatal("empty patch invalidated routing")
			}
		})
	}
}

func TestAccountAdministrationAtomicCookieAndRisk(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewAccountRepository(a), NewAccountRepository(b)
			ctx := context.Background()
			v, _, err := ra.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "web", SourceKey: "web", Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: "sso", EncryptedCloudflareCookie: "old-cookie", WebTier: account.WebTierSuper})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := rb.ApplyWebProfile(ctx, v.CredentialRef(), account.WebProfileObservation{Kind: account.WebProfileNSFWEnabled, OccurredAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			name, cookie, enabled := "renamed", "new-cookie", false
			patch := repository.AccountAdminPatch{Name: &name, EncryptedCloudflareCookie: &cookie, AccountUpdates: repository.AccountUpdates{Enabled: &enabled}, Risk: &repository.RiskAttribution{Status: account.RiskStatusRSCDenied, Trigger: account.RiskTriggerManual}}
			notifications := 0
			ra.SetInvalidationObserver(func(_ context.Context, event repository.InvalidationEvent) {
				notifications++
				stored, err := rb.Get(ctx, v.ID)
				if err != nil || stored.Name != name || stored.EncryptedCloudflareCookie != cookie || stored.RiskStatus != patch.Risk.Status {
					t.Error("notification preceded atomic commit")
				}
				if event.Kind != repository.InvalidationAccountStateChanged || event.AccountID != v.ID {
					t.Error("invalid routing notification")
				}
			})
			injected := errors.New("cookie write unavailable")
			callback := "test:admin-cookie-fault"
			if err := a.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "account_credentials" {
					tx.AddError(injected)
				}
			}); err != nil {
				t.Fatal(err)
			}
			_, writeErr := ra.UpdateAdministration(ctx, v.ID, patch)
			if err := a.db.Callback().Update().Remove(callback); err != nil {
				t.Fatal(err)
			}
			if !errors.Is(writeErr, injected) {
				t.Fatalf("fault missing: %v", writeErr)
			}
			stored, err := rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != v.Name || stored.Enabled != v.Enabled || stored.EncryptedCloudflareCookie != "old-cookie" || stored.RiskStatus != "" || notifications != 0 {
				t.Fatal("partial admin command committed or notified")
			}
			if _, err := ra.UpdateAdministration(ctx, v.ID, patch); err != nil {
				t.Fatal(err)
			}
			if notifications != 1 {
				t.Fatalf("notifications=%d", notifications)
			}
			stored, err = rb.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.WebNSFWEnabledAt == nil || stored.WebTier != account.WebTierSuper || stored.EncryptedAccessToken != "sso" || stored.AuthStatus != account.AuthStatusActive {
				t.Fatal("admin command changed independent identity/profile state")
			}
			// Invalid provider-specific patch cannot partially rename a Web account.
			entitled, wrongName := true, "invalid"
			if _, err := rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{Name: &wrongName, BuildSuperEntitled: &entitled}); err == nil {
				t.Fatal("cross-provider Build command accepted")
			}
			ra.SetInvalidationObserver(nil)
			cookie = ""
			if _, err := rb.UpdateAdministration(ctx, v.ID, repository.AccountAdminPatch{EncryptedCloudflareCookie: &cookie, Risk: &repository.RiskAttribution{}}); err != nil {
				t.Fatal(err)
			}
			stored, err = ra.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != name || stored.EncryptedCloudflareCookie != "" || stored.RiskStatus != "" || stored.RiskTrigger != "" || stored.RiskOriginAccountID != 0 || stored.RiskCheckedAt != nil || stored.RiskDetail != "" {
				t.Fatal("explicit clearing did not preserve independent fields")
			}
			for _, id := range []uint64{0, v.ID + 100} {
				if _, err := ra.UpdateAdministration(ctx, id, patch); !errors.Is(err, repository.ErrNotFound) {
					t.Fatal(fmt.Errorf("missing account %d: %w", id, err))
				}
			}
		})
	}
}
