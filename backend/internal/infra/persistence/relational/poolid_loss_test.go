package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// TestPoolIDDropPreservesSourceRows guards the subscription_sources rebuild:
// dropping the pool_id column must carry every source row over.
func TestPoolIDDropPreservesSourceRows(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "poolid-drop.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, statement := range legacyDDLStatements() {
		if err := database.db.WithContext(ctx).Exec(statement).Error; err != nil {
			t.Fatalf("legacy schema: %v", err)
		}
	}
	now := time.Now().UTC()
	if err := database.db.WithContext(ctx).Exec("INSERT INTO egress_subscription_sources (name, scope, enabled, encrypted_url, refresh_interval_seconds, pool_id, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)", "src-a", "grok_web", true, "enc-url", 900, 7, now, now).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var after int
	if err := database.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM egress_subscription_sources").Scan(&after).Error; err != nil {
		t.Fatal(err)
	}
	if after != 1 {
		t.Fatalf("subscription rows lost: after=%d", after)
	}
	var poolCol int
	database.db.WithContext(ctx).Raw("SELECT COUNT(*) FROM pragma_table_info('egress_subscription_sources') WHERE name='pool_id'").Scan(&poolCol)
	if poolCol != 0 {
		t.Fatal("pool_id column survived")
	}
}
