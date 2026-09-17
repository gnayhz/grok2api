package account

import (
	"context"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type identityCostAdapter struct{}

func (identityCostAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderWeb }
func (identityCostAdapter) SyncAccountIdentity(context.Context, accountdomain.Credential) (provider.AccountIdentity, error) {
	return provider.AccountIdentity{Email: "cost@example.test", UserID: "11111111-1111-4111-8111-111111111111"}, nil
}

// Identical Service/SQL benchmark at the parent commit and final code. Provider
// transport latency is excluded; incomplete fixture reset is outside the timer.
func BenchmarkAccountIdentitySyncCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, complete := range []bool{false, true} {
			b.Run(fmt.Sprintf("%s/complete=%t", dialect, complete), func(b *testing.B) {
				ctx := context.Background()
				db := accountImportCostDatabase(b, dialect)
				repo := relational.NewAccountRepository(db)
				value := accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "cost", SourceKey: "cost", EncryptedAccessToken: "synthetic", AuthStatus: accountdomain.AuthStatusActive}
				if complete {
					value.UserID = "11111111-1111-4111-8111-111111111111"
				}
				stored, _, err := repo.UpsertByIdentity(ctx, value)
				if err != nil {
					b.Fatal(err)
				}
				s := NewService(repo, nil, nil, nil, providerimpl.NewRegistry(identityCostAdapter{}), nil, security.RandomTokenSource{}, nil, nil, nil)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if !complete {
						b.StopTimer()
						if _, _, err := repo.UpsertByIdentity(ctx, value); err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
					}
					if err := s.SyncAccountIdentity(ctx, stored.ID); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
