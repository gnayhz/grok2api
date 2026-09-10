package account

import (
	"context"
	"errors"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestWebProfileCompletionMustBelongToObservedMaterial(t *testing.T) {
	for _, action := range []string{"acceptTerms", "setBirthDate", "enableNSFW"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			s, repo, adapter := newWebAccountSettingsTestService(t)
			v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: action, SourceKey: action, UserID: "old-user", EncryptedAccessToken: "old-sso", AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			var replacement accountdomain.Credential
			adapter.afterCall = func(completed string) {
				if completed != action {
					return
				}
				fresh := v
				fresh.UserID, fresh.EncryptedAccessToken = "new-user", "new-sso"
				var err error
				replacement, _, err = repo.UpsertByIdentity(ctx, fresh)
				if err != nil {
					t.Fatal(err)
				}
				if replacement.ID != v.ID || replacement.CredentialGeneration != v.CredentialGeneration+1 {
					t.Fatal("fixture did not replace material before completion")
				}
			}
			switch action {
			case "acceptTerms":
				err = s.AcceptWebTerms(ctx, v.ID)
			case "setBirthDate":
				err = s.SetWebBirthDate(ctx, v.ID)
			case "enableNSFW":
				err = s.EnableWebNSFW(ctx, v.ID)
			}
			if !errors.Is(err, ErrConflict) {
				t.Fatalf("stale completion must be distinguishable from success: %v", err)
			}
			current, readErr := repo.Get(ctx, v.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if replacement.ID == 0 || current.CredentialGeneration != replacement.CredentialGeneration || current.EncryptedAccessToken != "new-sso" {
				t.Fatal("new material missing")
			}
			marked := current.WebTermsAcceptedAt != nil
			if action == "setBirthDate" {
				marked = current.WebBirthDateSetAt != nil
			}
			if action == "enableNSFW" {
				marked = current.WebNSFWEnabledAt != nil
			}
			if marked {
				t.Errorf("old material's %s completion marked new identity as completed (service error=%v)", action, err)
			}
		})
	}
}
