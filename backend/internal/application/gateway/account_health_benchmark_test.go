package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func BenchmarkAccountHealthWrite(b *testing.B) {
	for _, kind := range []string{"healthy_success", "soft_failure"} {
		b.Run(kind, func(b *testing.B) {
			ctx := context.Background()
			db, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "health.db"))
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			v, _, err := repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "bench", SourceKey: "bench", EncryptedAccessToken: "encrypted", Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				b.Fatal(err)
			}
			s := NewSelector(repo, memory.NewConcurrencyLimiter(), memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
			s.markSuccess(ctx, v, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if kind == "healthy_success" {
					s.markSuccess(ctx, v, nil)
				} else {
					s.MarkFailure(ctx, v, 0, 0)
				}
			}
		})
	}
}
