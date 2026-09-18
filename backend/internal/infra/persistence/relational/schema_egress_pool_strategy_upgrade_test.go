package relational

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestSchemaUpgradesEgressPoolStrategiesPreservingMembers(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			var database *Database
			var err error
			if dialect == "postgres" {
				dsn := os.Getenv("TEST_POSTGRES_DSN")
				if dsn == "" {
					t.Skip("requires isolated TEST_POSTGRES_DSN")
				}
				database, err = OpenPostgres(ctx, dsn, 4, 2)
			} else {
				database, err = OpenSQLite(ctx, filepath.Join(t.TempDir(), "pool-strategies.db"))
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = database.Close() })
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			repo := NewEgressRepository(database)
			pool, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "synthetic-upgrade", Strategy: domain.PoolStrategyRandom, FallbackMode: domain.PoolFallbackNone})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = repo.DeleteEgressPool(ctx, pool.ID) })
			node, err := repo.CreateEgressNode(ctx, domain.Node{Name: "synthetic-upgrade-member", Enabled: true, EncryptedProxyURL: "synthetic-encrypted-proxy"})
			if err != nil {
				t.Fatal(err)
			}
			if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{node.ID}); err != nil {
				t.Fatal(err)
			}
			installLegacyPoolStrategyConstraint(t, ctx, database)
			if err := database.db.Exec("UPDATE egress_pools SET strategy = 'session-reuse' WHERE id = ?", pool.ID).Error; err == nil {
				t.Fatal("legacy constraint unexpectedly accepted session reuse")
			}
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := repo.GetEgressPool(ctx, pool.ID)
			if err != nil || stored.Name != pool.Name || stored.Enabled || stored.Strategy != domain.PoolStrategyRandom {
				t.Fatalf("upgrade changed old pool: %+v %v", stored, err)
			}
			for _, strategy := range []domain.PoolStrategy{domain.PoolStrategyLeastUsed, domain.PoolStrategySessionReuse} {
				stored.Strategy = strategy
				if _, err := repo.UpdateEgressPool(ctx, stored); err != nil {
					t.Fatalf("upgraded constraint rejected %s: %v", strategy, err)
				}
			}
			if err := database.InitializeSchema(ctx); err != nil {
				t.Fatalf("repeat startup: %v", err)
			}
			stored, err = repo.GetEgressPool(ctx, pool.ID)
			if err != nil || stored.Strategy != domain.PoolStrategySessionReuse || stored.Enabled {
				t.Fatalf("restart changed pool: %+v %v", stored, err)
			}
			members, err := repo.ListEgressNodesByPool(ctx, pool.ID)
			if err != nil || len(members) != 1 || members[0].ID != node.ID {
				t.Fatalf("upgrade lost membership: %+v %v", members, err)
			}
			if err := database.db.Exec("UPDATE egress_pools SET strategy = 'unsupported' WHERE id = ?", pool.ID).Error; err == nil {
				t.Fatal("upgraded constraint allowed an unknown strategy")
			}
		})
	}
}

func installLegacyPoolStrategyConstraint(t *testing.T, ctx context.Context, database *Database) {
	t.Helper()
	const legacy = "strategy IN ('affinity','random','sticky','rotation')"
	install := func() error {
		db := database.db.WithContext(ctx)
		if database.dialect == "postgres" {
			if err := db.Exec("ALTER TABLE egress_pools DROP CONSTRAINT chk_egress_pools_strategy").Error; err != nil {
				return err
			}
			return db.Exec("ALTER TABLE egress_pools ADD CONSTRAINT chk_egress_pools_strategy CHECK (" + legacy + ")").Error
		}
		var sql string
		if err := db.Raw("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'egress_pools'").Scan(&sql).Error; err != nil {
			return err
		}
		oldSQL := strings.Replace(sql, "strategy IN ('affinity','random','sticky','rotation','least-used','session-reuse')", legacy, 1)
		if oldSQL == sql {
			t.Fatal("fixture did not replace current constraint")
		}
		oldSQL = strings.Replace(oldSQL, "egress_pools", "egress_pools_legacy", 1)
		for _, statement := range []string{oldSQL, "INSERT INTO egress_pools_legacy SELECT * FROM egress_pools", "DROP TABLE egress_pools", "ALTER TABLE egress_pools_legacy RENAME TO egress_pools"} {
			if err := db.Exec(statement).Error; err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if database.dialect == "sqlite" {
		err = database.withSQLiteForeignKeysDisabled(ctx, install)
	} else {
		err = install()
	}
	if err != nil {
		t.Fatal(err)
	}
}
