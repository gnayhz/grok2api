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

func BenchmarkPoolMemberReplacement(b *testing.B) {
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
				db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "member-cost.db"))
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
			pool, err := service.CreatePool(ctx, PoolInput{Name: name, Strategy: domain.PoolStrategyRandom})
			if err != nil {
				b.Fatal(err)
			}
			defer service.DeletePool(ctx, pool.ID)
			var ids []uint64
			for i := 0; i < 3; i++ {
				proxy := "http://member-benchmark.example:8080"
				node, err := service.Create(ctx, Input{Name: fmt.Sprintf("%s-node-%d", name, i), Enabled: true, ProxyURL: &proxy})
				if err != nil {
					b.Fatal(err)
				}
				defer service.Delete(ctx, node.ID)
				ids = append(ids, node.ID)
			}
			if err := service.SetPoolMembers(ctx, pool.ID, ids); err != nil {
				b.Fatal(err)
			}
			if err := service.SetPoolMemberPriority(ctx, pool.ID, ids[1], 5); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := service.SetPoolMembers(ctx, pool.ID, ids); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			members, err := repo.EgressPoolMembers(ctx)
			if err != nil || len(members[pool.ID]) != len(ids) {
				b.Fatalf("members: %v %v", members, err)
			}
			preferred, err := repo.EgressPoolPreferredNodes(ctx)
			if err != nil || preferred[pool.ID] != ids[1] {
				b.Fatalf("preference: %v %v", preferred, err)
			}
			b.ReportMetric(1, "writes/op")
		})
	}
}
