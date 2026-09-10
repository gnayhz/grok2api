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

func BenchmarkFixedRoutingConfiguration(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"node", "routing"} {
			b.Run(dialect+"/"+operation, func(b *testing.B) {
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
					db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "fixed-routing-cost.db"))
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
				proxy := "http://fixed-cost.example:8080"
				node, err := service.Create(ctx, Input{Name: name, Enabled: true, ProxyURL: &proxy})
				if err != nil {
					b.Fatal(err)
				}
				defer service.Delete(ctx, node.ID)
				target := RoutingTargetInput{Mode: domain.RoutingTargetNode, NodeID: node.ID}
				config := OperationsConfigInput{ProbeIntervalSeconds: 900, DefaultTarget: &target, ScopeTargets: map[domain.Scope]RoutingTargetInput{}, ClassTargets: map[domain.TrafficClass]RoutingTargetInput{}}
				if _, err := service.UpdateOperationsConfig(ctx, config); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if operation == "node" {
						if _, err := service.Update(ctx, node.ID, Input{Name: name, Enabled: true}); err != nil {
							b.Fatal(err)
						}
					} else if _, err := service.UpdateOperationsConfig(ctx, config); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				stored, err := repo.GetEgressNode(ctx, node.ID)
				if err != nil || !stored.Enabled || stored.Name != name || stored.EncryptedProxyURL == "" {
					b.Fatalf("node changed: %+v %v", stored, err)
				}
				routing, err := repo.GetEgressOperationsConfig(ctx)
				if err != nil || routing.DefaultTarget.NodeID != node.ID {
					b.Fatalf("routing changed: %+v %v", routing, err)
				}
				b.ReportMetric(1, "writes/op")
			})
		}
	}

}
