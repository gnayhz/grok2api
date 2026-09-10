package account

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Interleave a real committed write after the service reads its validation
// snapshot, before it persists the administrator's single-field command.
type adminSnapshotRepository struct {
	repository.AccountRepository
	afterRead func()
}

func (r *adminSnapshotRepository) Get(ctx context.Context, id uint64) (accountdomain.Credential, error) {
	v, err := r.AccountRepository.Get(ctx, id)
	if err == nil && r.afterRead != nil {
		hook := r.afterRead
		r.afterRead = nil
		hook()
	}
	return v, err
}

func TestAdminEditPreservesConcurrentState(t *testing.T) {
	for _, scenario := range []string{"token_rotation", "administrator_disable", "risk_attribution", "enabled_changed"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "admin.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			service := NewService(repo, relational.NewAuditRepository(db), nil, nil, nil, nil, nil)
			original := mustUpsert(t, repo, accountdomain.Credential{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth,
				Name: "original", SourceKey: "admin-concurrency", EncryptedAccessToken: "old-access", EncryptedRefreshToken: "old-refresh", ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: accountdomain.AuthStatusActive})
			rotatedExpiry := now.Add(4 * time.Hour)
			service.accounts = &adminSnapshotRepository{AccountRepository: repo, afterRead: func() {
				var err error
				switch scenario {
				case "token_rotation":
					_, err = rotateOAuthFixture(repo, ctx, original.ID, "new-access", "new-refresh", rotatedExpiry, 0)
				case "administrator_disable", "enabled_changed":
					disabled := false
					_, err = repo.UpdateMany(ctx, original.Provider, []uint64{original.ID}, repository.AccountUpdates{Enabled: &disabled})
				case "risk_attribution":
					err = repo.UpdateRiskAttribution(ctx, original.ID, repository.RiskAttribution{Status: accountdomain.RiskStatusRSCDenied, Trigger: accountdomain.RiskTriggerDegrade, OriginAccountID: original.ID, CheckedAt: &now, Detail: "current attribution"})
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			name, enabled := "renamed", false
			input := UpdateInput{Name: &name}
			if scenario == "enabled_changed" {
				input.Enabled = &enabled
			}
			view, err := service.Update(ctx, original.ID, input)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := repo.Get(ctx, original.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Name != name {
				t.Fatal("administrator name edit was lost")
			}
			switch scenario {
			case "token_rotation":
				if stored.EncryptedAccessToken != "new-access" || stored.EncryptedRefreshToken != "new-refresh" || !stored.ExpiresAt.Equal(rotatedExpiry) {
					t.Fatal("name edit rolled back concurrent token rotation or expiry")
				}
			case "administrator_disable":
				if stored.Enabled {
					t.Fatal("name edit undid concurrent administrator disable")
				}
			case "risk_attribution":
				if stored.RiskStatus != accountdomain.RiskStatusRSCDenied || stored.RiskTrigger != accountdomain.RiskTriggerDegrade || stored.RiskDetail != "current attribution" || stored.RiskCheckedAt == nil || !stored.RiskCheckedAt.Equal(now) {
					t.Fatal("name edit erased concurrent risk attribution")
				}
			case "enabled_changed":
				if stored.Enabled || view.EnabledChanged {
					t.Fatal("enabledChanged described the stale validation snapshot instead of the committed transition")
				}
			}
		})
	}
}
