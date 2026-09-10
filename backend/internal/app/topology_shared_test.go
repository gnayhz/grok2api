package app

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/jackc/pgx/v5"
)

func TestSharedMediaMarkerConcurrentApplicationConstruction(t *testing.T) {
	dsn, redisAddress := os.Getenv("TEST_POSTGRES_DSN"), os.Getenv("TEST_REDIS_ADDRESS")
	if dsn == "" || redisAddress == "" {
		t.Skip("requires isolated PostgreSQL and Redis")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	schema := fmt.Sprintf("topology_marker_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	dir := t.TempDir()
	configure := func(instance string, replicas int) func(*config.Config) {
		return func(cfg *config.Config) {
			cfg.Database.Driver, cfg.Database.Postgres.DSN = "postgres", parsed.String()
			cfg.RuntimeStore.Driver = "redis"
			cfg.RuntimeStore.Redis.Address, cfg.RuntimeStore.Redis.KeyPrefix = redisAddress, schema+":"
			cfg.Deployment.Replicas, cfg.Deployment.ClusterID, cfg.Deployment.InstanceID, cfg.Deployment.SharedMedia = replicas, "topology-marker-cluster", instance, true
			cfg.Media.Local.Path = dir
			if err := cfg.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Initialize SQL once, without a multi-replica marker. The concurrent phase
	// exercises Application's real shared-media and resource construction path.
	seed := newLifecycleApplication(t, configure("seed", 1))
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, sharedMediaMarkerName)); !os.IsNotExist(err) {
		t.Fatalf("single replica created marker: %v", err)
	}
	t.Run("replicas", func(t *testing.T) {
		for _, name := range []string{"replica-a", "replica-b"} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				a := newLifecycleApplication(t, configure(name, 2))
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}
				if open := a.database.Stats().OpenConnections; open != 0 {
					t.Fatalf("SQL remains open: %d", open)
				}
			})
		}
	})
	marker, err := os.ReadFile(filepath.Join(dir, sharedMediaMarkerName))
	if err != nil || string(marker) != "topology-marker-cluster\n" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
}
