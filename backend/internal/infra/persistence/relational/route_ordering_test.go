package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestFindModelRoutesOrderingContract 锁定 round 33 查询改写的排序契约
// （与原 ORDER BY 逐项等价）：候选名命中 < 别名命中；同级内 Provider
// 优先级 grok_build < grok_web < grok_console；再按 id 升序。别名命中
// 路径经 model_route_aliases 表（改写后走独立分支）。
func TestFindModelRoutesOrderingContract(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, t.TempDir()+"/order.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewModelRepository(database)
	// 三 Provider 同名（候选命中）+ 一个纯别名路由（别名命中）。
	for _, p := range []account.Provider{account.ProviderConsole, account.ProviderWeb, account.ProviderBuild} {
		if err := repo.UpsertDiscovered(ctx, p, []string{"grok-order-x"}); err != nil {
			t.Fatal(err)
		}
	}
	// 手工插一条只有别名的路由。
	if err := database.db.WithContext(ctx).Exec(
		"INSERT INTO model_routes (public_id, provider, upstream_model, capability, origin, enabled, created_at, updated_at) VALUES ('Console/grok-alias-only', 'grok_console', 'grok-alias-only', 'responses', 'manual', 1, ?, ?)",
		time.Now().UTC(), time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}
	var routeID uint64
	if err := database.db.WithContext(ctx).Raw("SELECT id FROM model_routes WHERE upstream_model = 'grok-alias-only'").Scan(&routeID).Error; err != nil {
		t.Fatal(err)
	}
	if err := database.db.WithContext(ctx).Exec(
		"INSERT INTO model_route_aliases (model_route_id, alias, created_at) VALUES (?, 'grok-order-x', ?)", routeID, time.Now().UTC()).Error; err != nil {
		t.Fatal(err)
	}

	rows, err := findModelRoutesByPublicID(database.db.WithContext(ctx), "grok-order-x")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 {
		t.Fatalf("rows = %d, want 4（3 候选 + 1 别名）", len(rows))
	}
	// 前三：候选命中按 Provider 优先级 build→web→console。
	wantProviders := []string{"grok_build", "grok_web", "grok_console"}
	for i, want := range wantProviders {
		if rows[i].Provider != want {
			t.Fatalf("rows[%d].provider = %s, want %s", i, rows[i].Provider, want)
		}
	}
	// 末位：别名命中的 console 路由（同为 console 但别名命中排后）。
	if rows[3].Provider != "grok_console" || rows[3].UpstreamModel != "grok-alias-only" {
		t.Fatalf("rows[3] = %s/%s, want 别名命中的 grok-alias-only", rows[3].Provider, rows[3].UpstreamModel)
	}
	// 未知名（无候选无别名）→ ErrRecordNotFound。
	if _, err := findModelRoutesByPublicID(database.db.WithContext(ctx), "grok-never-registered"); err == nil {
		t.Fatal("未知名应返回 not found")
	}
}
