package account

import (
	"context"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestCredentialCompletionDoesNotOverwriteReimport(t *testing.T) {
	for _, scenario := range []string{"refresh_success", "refresh_failure", "sso_rejection", "concurrent_failures"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			s, original, adapter := newCredentialRefreshTestService(t, now)
			s.now = func() time.Time { return now }
			reimport := func() {
				replacement := original
				replacement.EncryptedAccessToken, replacement.EncryptedRefreshToken = "replacement-access", "replacement-refresh"
				if scenario == "sso_rejection" {
					replacement.AuthType = accountdomain.AuthTypeSSO
					replacement.EncryptedRefreshToken = ""
				}
				if _, _, err := s.accounts.UpsertByIdentity(ctx, replacement); err != nil {
					t.Fatal(err)
				}
			}
			switch scenario {
			case "refresh_success":
				adapter.afterRefresh = reimport
				result, err := s.EnsureCredential(ctx, original, true)
				if err != nil {
					t.Fatal(err)
				}
				if result.EncryptedAccessToken != "replacement-access" {
					t.Errorf("refresh returned obsolete material")
				}
			case "refresh_failure":
				reimport()
				s.recordCredentialRefreshFailure(ctx, original, &provider.CredentialRefreshError{Status: 400, Code: "invalid_grant", Permanent: true}, true, false)
			case "sso_rejection":
				original.AuthType = accountdomain.AuthTypeSSO
				reimport()
				if err := s.markSSOCredentialRejected(ctx, original, "old SSO rejected"); err != nil {
					t.Fatal(err)
				}
			case "concurrent_failures":
				var wg sync.WaitGroup
				for range 12 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						s.recordCredentialRefreshFailure(ctx, original, &provider.CredentialRefreshError{Status: 503, Code: "server_error"}, true, false)
					}()
				}
				wg.Wait()
			}
			stored, err := s.accounts.Get(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "concurrent_failures" {
				if stored.RefreshFailureCount != 12 {
					t.Errorf("failure count = %d, want 12", stored.RefreshFailureCount)
				}
				return
			}
			if stored.EncryptedAccessToken != "replacement-access" {
				t.Errorf("reimported material overwritten")
			}
			if stored.AuthStatus != accountdomain.AuthStatusActive || stored.RefreshPermanent || stored.RefreshFailureCount != 0 {
				t.Errorf("new credential poisoned: auth=%s permanent=%t failures=%d", stored.AuthStatus, stored.RefreshPermanent, stored.RefreshFailureCount)
			}
		})
	}
}
