package relational

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestModelLiteralNamePrioritySurvivesAvailabilityChanges(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			db, other := settingsDatabasePair(t, driver)
			ctx := context.Background()
			accounts, writer, reader := NewAccountRepository(db), NewModelRepository(db), NewModelRepository(other)
			for _, provider := range account.Providers() {
				for _, alias := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/alias-%t", provider, alias), func(t *testing.T) {
						name := fmt.Sprintf("priority-%t", alias)
						publicName := provider.ModelNamespace() + "/" + name
						createAccount := func(suffix string) account.Credential {
							value, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: provider, Name: name + suffix, SourceKey: name + suffix, EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive, Enabled: true})
							if err != nil {
								t.Fatal(err)
							}
							return value
						}
						fallbackAccount, literalAccount := createAccount("-fallback"), createAccount("-literal")
						capability, fallbackUpstream, literalUpstream := model.CapabilityResponses, "grok-4.3", "grok-4.5"
						if provider == account.ProviderWeb {
							capability, fallbackUpstream, literalUpstream = model.CapabilityChat, "grok-chat-fast", "grok-chat-expert"
						}
						fallback, err := writer.Create(ctx, model.Route{PublicID: publicName, Provider: provider, UpstreamModel: fallbackUpstream, Capability: capability, Enabled: true}, []uint64{fallbackAccount.ID})
						if err != nil {
							t.Fatal(err)
						}
						literal, err := writer.Create(ctx, model.Route{PublicID: provider.ModelNamespace() + "/" + publicName, Provider: provider, UpstreamModel: literalUpstream, Capability: capability, Enabled: true}, []uint64{literalAccount.ID})
						if err != nil {
							t.Fatal(err)
						}
						if alias {
							next := provider.ModelNamespace() + "/current-" + name
							if _, err := writer.Patch(ctx, literal.ID, model.RoutePatch{PublicID: &next}); err != nil {
								t.Fatal(err)
							}
						}
						assert := func(t *testing.T, available bool, enabled bool, expectedID uint64) {
							t.Helper()
							rows, err := reader.GetByPublicIDCandidates(ctx, publicName)
							if available {
								if err != nil || len(rows) != 1 || rows[0].ID != expectedID {
									t.Errorf("resolved another model: %+v %v; want %d", rows, err, expectedID)
								}
							} else {
								var unavailable *repository.ModelRouteUnavailableError
								if !errors.Is(err, repository.ErrNotFound) || !errors.As(err, &unavailable) || unavailable.Enabled != enabled {
									t.Errorf("unavailable name lost identity/status: %+v %v", rows, err)
								}
							}
							exists, err := reader.HasEnabledRouteByPublicID(ctx, publicName)
							if err != nil || exists != enabled {
								t.Errorf("enabled meaning=%t want=%t err=%v", exists, enabled, err)
							}
							configured, err := reader.GetByPublicIDIncludingDisabled(ctx, publicName)
							if err != nil || configured.ID != expectedID {
								t.Errorf("configured meaning=%+v want=%d err=%v", configured, expectedID, err)
							}
						}
						t.Run("active", func(t *testing.T) { assert(t, true, true, literal.ID) })
						if _, err := writer.UpdateManyEnabled(ctx, []uint64{literal.ID}, false); err != nil {
							t.Fatal(err)
						}
						t.Run("disabled", func(t *testing.T) { assert(t, false, false, literal.ID) })
						if _, err := writer.UpdateManyEnabled(ctx, []uint64{literal.ID}, true); err != nil {
							t.Fatal(err)
						}
						no := false
						patch := repository.AccountAdminPatch{}
						patch.Enabled = &no
						if _, err := accounts.UpdateAdministration(ctx, literalAccount.ID, patch); err != nil {
							t.Fatal(err)
						}
						t.Run("no-account", func(t *testing.T) { assert(t, false, true, literal.ID) })
						if err := writer.Delete(ctx, literal.ID); err != nil {
							t.Fatal(err)
						}
						t.Run("deleted", func(t *testing.T) { assert(t, true, true, fallback.ID) })
					})
				}
			}
		})
	}
}
