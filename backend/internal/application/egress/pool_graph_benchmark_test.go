package egress

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func BenchmarkPoolFallbackUpdate(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		b.Run(dialect, func(b *testing.B) {
			ctx := context.Background()
			var db *relational.Database
			var err error
			if dialect == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					b.Skip("requires isolated TEST_POSTGRES_DSN")
				}
				db, err = relational.OpenPostgres(ctx, dsn, 4, 2)
			} else {
				db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "pool-cost.db"))
			}
			if err != nil {
				b.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				b.Fatal(err)
			}
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				b.Fatal(err)
			}
			repo := relational.NewEgressRepository(db)
			service := NewService(repo, cipher)
			defer service.Close(ctx)
			name := fmt.Sprintf("pool-bench-%d", time.Now().UnixNano())
			second, err := service.CreatePool(ctx, PoolInput{Name: name + "-target", Strategy: domain.PoolStrategyRandom})
			if err != nil {
				b.Fatal(err)
			}
			defer service.DeletePool(ctx, second.ID)
			input := PoolInput{Name: name + "-first", Strategy: domain.PoolStrategyRandom, FallbackMode: domain.PoolFallbackPool, FallbackPoolID: second.ID}
			first, err := service.CreatePool(ctx, input)
			if err != nil {
				b.Fatal(err)
			}
			defer service.DeletePool(ctx, first.ID)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				updated, err := service.UpdatePool(ctx, first.ID, input)
				if err != nil || updated.FallbackPoolID != second.ID || updated.Strategy != domain.PoolStrategyRandom {
					b.Fatalf("valid update = %+v %v", updated, err)
				}
			}
			b.StopTimer()
			stored, err := repo.GetEgressPool(ctx, first.ID)
			if err != nil || stored.FallbackPoolID != second.ID {
				b.Fatalf("valid fallback lost: %+v %v", stored, err)
			}
			b.ReportMetric(1, "writes/op")
		})
	}
}
