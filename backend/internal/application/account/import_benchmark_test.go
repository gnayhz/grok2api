package account

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type importCostAdapter struct{ seeds []provider.CredentialSeed }

func (importCostAdapter) Provider() accountdomain.Provider { return accountdomain.ProviderBuild }
func (importCostAdapter) Definition() provider.Definition {
	return provider.Definition{Provider: accountdomain.ProviderBuild, ModelNamespace: accountdomain.ProviderBuild.ModelNamespace(), Credential: provider.CredentialSurface{AuthType: accountdomain.AuthTypeOAuth, Import: true}}
}
func (a importCostAdapter) ParseImportedCredentials([]byte) ([]provider.CredentialSeed, error) {
	return append([]provider.CredentialSeed(nil), a.seeds...), nil
}
func (importCostAdapter) MarshalCredentials([]provider.CredentialSeed) ([]byte, error) {
	return nil, nil
}

func accountImportCostDatabase(b *testing.B, dialect string) *relational.Database {
	b.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if dialect == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "import-cost.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			b.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, err := sql.Open("pgx", dsn)
		if err != nil {
			b.Fatal(err)
		}
		schema := fmt.Sprintf("g20_import_cost_%d", time.Now().UnixNano())
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
		db, err = relational.OpenPostgres(ctx, parsed.String(), 8, 4)
		if err != nil {
			b.Fatal(err)
		}
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

// The same fixture runs at the prior commit and the final implementation.
// Import includes M07 preflight, encryption, SQL and link reconciliation.
// Delete includes M07 validation, SQL, deletion protection and runtime cleanup;
// repeated fixture creation/explicit clear are outside the measured interval.
func BenchmarkAccountImportDeleteCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"import", "delete"} {
			for _, count := range []int{1, 100} {
				b.Run(fmt.Sprintf("%s/%s/%d", dialect, operation, count), func(b *testing.B) {
					ctx := context.Background()
					db := accountImportCostDatabase(b, dialect)
					repo := relational.NewAccountRepository(db)
					cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
					if err != nil {
						b.Fatal(err)
					}
					seeds := make([]provider.CredentialSeed, count)
					emails := make([]string, count)
					for i := range seeds {
						emails[i] = fmt.Sprintf("cost-%d@example.test", i)
						seeds[i] = provider.CredentialSeed{Provider: accountdomain.ProviderBuild, AuthType: accountdomain.AuthTypeOAuth, Name: fmt.Sprint(i), SourceKey: fmt.Sprint(i), Email: emails[i], AccessToken: "synthetic", RefreshToken: "synthetic"}
					}
					s := NewService(repo, nil, nil, nil, provider.NewRegistry(importCostAdapter{seeds: seeds}), cipher, nil)
					s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
					if out, err := s.ImportCredentials(ctx, []byte("synthetic")); err != nil || out.Created != count {
						b.Fatalf("seed: %+v, %v", out, err)
					}
					values := make([]accountdomain.Credential, count)
					for i := range seeds {
						values[i], err = s.credentialFromSeed(seeds[i])
						if err != nil {
							b.Fatal(err)
						}
					}
					b.ReportAllocs()
					b.ResetTimer()
					for b.Loop() {
						if operation == "import" {
							if out, err := s.ImportCredentials(ctx, []byte("synthetic")); err != nil || out.Updated != count {
								b.Fatalf("import: %+v, %v", out, err)
							}
							continue
						}
						b.StopTimer()
						if _, err := repo.ClearTombstones(ctx, emails); err != nil {
							b.Fatal(err)
						}
						out, err := repo.UpsertManyByIdentity(ctx, values)
						if err != nil {
							b.Fatal(err)
						}
						ids := make([]uint64, len(out))
						for i := range out {
							ids[i] = out[i].ID
						}
						b.StartTimer()
						if n, err := s.BatchDelete(ctx, ids); err != nil || n != int64(count) {
							b.Fatalf("delete: %d, %v", n, err)
						}
					}
				})
			}
		}
	}
}
