package account

import (
	"context"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// This fixture is identical at G21 and G22. It includes M07 current-state reads,
// in-flight ownership and real SQL commits. Provider facts have no network or
// cipher cost. Each refresh installs a new generation, rather than measuring a
// cooldown or already-complete shortcut.
func BenchmarkAccountMaintenanceOperationCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"credential", "billing"} {
			b.Run(fmt.Sprintf("%s/%s", dialect, operation), func(b *testing.B) {
				ctx := context.Background()
				db := accountImportCostDatabase(b, dialect)
				repo := relational.NewAccountRepository(db)
				value, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: "operation-cost", SourceKey: "operation-cost", EncryptedAccessToken: "access", EncryptedRefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour), AuthStatus: accountdomain.AuthStatusActive})
				if err != nil {
					b.Fatal(err)
				}
				adapter := &credentialRefreshAdapter{billing: accountdomain.Billing{MonthlyLimit: 100, Used: 12}}
				s := NewService(repo, nil, nil, nil, providerimpl.NewRegistry(adapter), nil, security.RandomTokenSource{}, nil, nil, nil)
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if operation == "credential" {
						value, err = s.ensureCredential(ctx, value, ensureCredentialOptions{force: true, bypassCooldown: true})
					} else {
						_, err = s.RefreshBilling(ctx, value.ID)
					}
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if operation == "credential" && adapter.refreshCount.Load() != int64(b.N) {
					b.Fatal("benchmark skipped actual rotation")
				}
				if operation == "billing" && adapter.billingCount.Load() != int64(b.N) {
					b.Fatal("benchmark skipped actual Billing observation")
				}
			})
		}
	}
}
