package account

import (
	"context"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestWebProfileImportMustKeepCompletedIdentitySeparate(t *testing.T) {
	for _, scenario := range []string{"same", "unknown", "different"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			s, repo, _ := newWebAccountSettingsTestService(t)
			user := "known-user"
			if scenario == "unknown" {
				user = ""
			}
			v, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: scenario, SourceKey: scenario, UserID: user, EncryptedAccessToken: "old-sso", AuthStatus: accountdomain.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.AcceptWebTerms(ctx, v.ID); err != nil {
				t.Fatal(err)
			}
			if err := s.EnableWebNSFW(ctx, v.ID); err != nil {
				t.Fatal(err)
			}
			replacement := v
			replacement.EncryptedAccessToken = "new-sso"
			if scenario == "different" {
				replacement.UserID = "other-known-user"
			}
			if _, _, err := repo.UpsertByIdentity(ctx, replacement); err != nil {
				t.Fatal(err)
			}
			current, err := repo.Get(ctx, v.ID)
			if err != nil {
				t.Fatal(err)
			}
			marked := current.WebTermsAcceptedAt != nil && current.WebNSFWEnabledAt != nil && current.WebBirthDateSetAt != nil
			if scenario == "different" {
				if current.WebTermsAcceptedAt != nil || current.WebNSFWEnabledAt != nil || current.WebBirthDateSetAt != nil {
					t.Error("explicit different identity inherited the previous user's completed Web profile")
				}
			} else if !marked {
				t.Error("same or legacy unknown identity lost compatible profile markers")
			}
		})
	}
}
