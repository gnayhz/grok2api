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

func BenchmarkDeclaredHygieneCommit(b *testing.B) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"read_only", "strip"} {
			b.Run(dialect+"/"+operation, func(b *testing.B) {
				ctx := context.Background()
				var db *relational.Database
				var err error
				if dialect == "postgres" {
					dsn := os.Getenv("TEST_POSTGRES_DSN")
					if dsn == "" {
						b.Skip("requires isolated TEST_POSTGRES_DSN")
					}
					db, err = relational.OpenPostgres(ctx, dsn, 8, 2)
				} else {
					db, err = relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "hygiene.db"))
				}
				if err != nil {
					b.Fatal(err)
				}
				defer db.Close()
				if err := db.InitializeSchema(ctx); err != nil {
					b.Fatal(err)
				}
				repo := relational.NewEgressRepository(db)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					b.Fatal(err)
				}
				service := NewService(repo, cipher)
				defer service.Close(ctx)
				proxy := "http://fixed.example:8080"
				if operation == "strip" {
					proxy = "http://user-{account}:pass@fixed.example:8080"
				}
				node, err := service.Create(ctx, Input{Name: fmt.Sprintf("bench-%d", time.Now().UnixNano()), Enabled: true, ProxyURL: &proxy})
				if err != nil {
					b.Fatal(err)
				}
				target := domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: node.ID}
				config := domain.DefaultOperationsConfig()
				config.DefaultTarget = target
				config.ScopeTargets = map[domain.Scope]domain.RoutingTarget{domain.ScopeBuild: target}
				config.ClassTargets = map[domain.TrafficClass]domain.RoutingTarget{domain.TrafficClassInference: target}
				_, err = repo.SaveEgressOperationsConfig(ctx, config, func(domain.Node) error {
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
				before, err := repo.GetEgressOperationsConfig(ctx)
				if err != nil {
					b.Fatal(err)
				}
				port := declaredOperationsPort{OperationsRepository: repo}
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if operation == "strip" {
						if _, err := repo.SaveEgressOperationsConfig(ctx, config, func(domain.Node) error { return nil }); err != nil {
							b.Fatal(err)
						}
					}
					if err := service.enforceRoutingHygieneAfterSync(ctx, port); err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				after, err := repo.GetEgressOperationsConfig(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if operation == "read_only" && (after.DefaultTarget != target || after.ScopeTargets[domain.ScopeBuild] != target || after.ClassTargets[domain.TrafficClassInference] != target || !after.UpdatedAt.Equal(before.UpdatedAt)) {
					b.Fatalf("healthy targets changed: %+v", after)
				}
				if operation == "strip" {
					if after.DefaultTarget.Configured() || len(after.ScopeTargets) != 0 || len(after.ClassTargets) != 0 {
						b.Fatal("hygiene failed to strip template")
					}
					b.ReportMetric(2, "writes/op")
				} else {
					b.ReportMetric(0, "writes/op")
				}
			})
		}
	}
}
