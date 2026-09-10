package relational

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type webProfileCostAdapter struct{ calls int }

func (*webProfileCostAdapter) Provider() account.Provider { return account.ProviderWeb }
func (a *webProfileCostAdapter) AcceptTerms(context.Context, account.Credential) error {
	a.calls++
	return nil
}
func (a *webProfileCostAdapter) SetBirthDate(context.Context, account.Credential, time.Time) error {
	a.calls++
	return nil
}
func (a *webProfileCostAdapter) EnableNSFW(context.Context, account.Credential) error {
	a.calls++
	return nil
}

// Identical source is used before and after G23. Script mode runs all three
// actual M07 actions with SQL; fixture marker reset is outside the timer. The
// other mode measures explicit identity replacement, including its final read.
// These local costs exclude upstream transport latency and encryption.
func BenchmarkAccountWebProfileCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, mode := range []string{"script", "import_changed_identity"} {
			b.Run(dialect+"/"+mode, func(b *testing.B) {
				ctx := context.Background()
				db := webProfileCostDatabase(b, dialect)
				repo := NewAccountRepository(db)
				value := account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, Name: "cost", SourceKey: "cost", UserID: "user-a", EncryptedAccessToken: "synthetic", AuthStatus: account.AuthStatusActive}
				stored, _, err := repo.UpsertByIdentity(ctx, value)
				if err != nil {
					b.Fatal(err)
				}
				adapter := &webProfileCostAdapter{}
				service := accountapp.NewService(repo, nil, nil, nil, provider.NewRegistry(adapter), nil, nil)
				var operations int
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					if mode == "script" {
						b.StopTimer()
						if err := db.db.Exec("UPDATE web_account_profiles SET terms_accepted_at = NULL, terms_accepted_version = 0, birth_date_set_at = NULL, nsfw_enabled_at = NULL WHERE account_id = ?", stored.ID).Error; err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
						if err := service.AcceptWebTerms(ctx, stored.ID); err != nil {
							b.Fatal(err)
						}
						if err := service.EnableWebNSFW(ctx, stored.ID); err != nil {
							b.Fatal(err)
						}
					} else {
						if value.UserID == "user-a" {
							value.UserID = "user-b"
						} else {
							value.UserID = "user-a"
						}
						if _, _, err := repo.UpsertByIdentity(ctx, value); err != nil {
							b.Fatal(err)
						}
					}
					operations++
				}
				b.StopTimer()
				if mode == "script" && adapter.calls != 3*operations {
					b.Fatalf("benchmark skipped real steps: calls=%d operations=%d", adapter.calls, operations)
				}
			})
		}
	}
}

func webProfileCostDatabase(b *testing.B, dialect string) *Database {
	b.Helper()
	ctx := context.Background()
	var db *Database
	var err error
	if dialect == "sqlite" {
		db, err = OpenSQLite(ctx, filepath.Join(b.TempDir(), "web-profile-cost.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			b.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			b.Fatal(err)
		}
		schema := fmt.Sprintf("g23_profile_cost_%d", time.Now().UnixNano())
		if _, err := admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			_ = admin.Close()
			b.Fatal(err)
		}
		b.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				b.Error(err)
			}
		})
		parsed, err := url.Parse(dsn)
		if err != nil {
			b.Fatal(err)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		db, err = OpenPostgres(ctx, parsed.String(), 8, 4)
	}
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		b.Fatal(err)
	}
	return db
}
