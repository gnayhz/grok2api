package selector

import (
	"context"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

// Identical fixture is executable before/after the current-facts change. It
// measures an actual warm-pool selection, material read and slot release; the
// isolated PostgreSQL database is supplied by the iteration test wrapper.
func BenchmarkSelectorClaimCost(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, count := range []int{32, 3000} {
			b.Run(fmt.Sprintf("%s/%d", dialect, count), func(b *testing.B) {
				ctx := context.Background()
				var db *relational.Database
				var err error
				if dialect == "postgres" {
					dsn := os.Getenv("TEST_POSTGRES_DSN")
					if dsn == "" {
						b.Skip("isolated TEST_POSTGRES_DSN required")
					}
					db, err = relational.OpenPostgres(ctx, dsn, 8, 4)
				} else {
					db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "claim-cost.db"))
				}
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				if err := db.InitializeSchema(ctx); err != nil {
					b.Fatal(err)
				}
				repo := relational.NewAccountRepository(db)
				existing, err := repo.ListRoutingCandidates(ctx, account.ProviderBuild, 0, "grok-test", "")
				if err != nil {
					b.Fatal(err)
				}
				excluded := make(map[uint64]bool)
				for _, v := range existing {
					excluded[v.Credential.ID] = true
				}
				values := make([]account.Credential, count)
				for i := range values {
					name := fmt.Sprintf("g19-claim-cost-%d-%d", count, i)
					values[i] = account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: "synthetic", Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 64, Priority: count - i}
				}
				results, err := repo.ImportAccounts(ctx, testsupport.AccountImports(values))
				if err != nil {
					b.Fatal(err)
				}
				for _, r := range results {
					delete(excluded, r.ID)
				}
				selector := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
				acquireLease := func() (*accountLease, error) {
					s, serr := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
					if serr != nil {
						return nil, serr
					}
					return s.Acquire(ctx, excluded, false)
				}
				lease, err := acquireLease()
				if err != nil {
					b.Fatal(err)
				}
				lease.Release()
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					lease, err := acquireLease()
					if err != nil {
						b.Fatal(err)
					}
					lease.Release()
				}
			})
		}
	}
}
