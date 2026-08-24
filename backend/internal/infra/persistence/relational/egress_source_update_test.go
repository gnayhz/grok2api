package relational

import (
	"context"
	"testing"
	"time"

	egress "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 管理端编辑订阅源不得回滚同步 worker 的运行态列——与 UpdateEgressNode
// 修复的同类缺陷(读-改-写窗口内后台窄列写被陈旧快照整体覆盖)。
// 场景:handler 读快照 → 维护循环完成一次同步(写 last_synced_at/
// next_sync_at/last_sync_imported/last_sync_error) → 管理端改名落库。
// 全行 Save 会把四个运行态列滚回旧值;正确行为是配置面列写入、运行态列
// 归窄方法所有(仅在服务层显式重置时清空 next_sync_at/last_sync_error)。
func TestUpdateEgressSourcePreservesConcurrentSyncState(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)

	created, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "feed", Enabled: true, EncryptedURL: "enc-url", RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}

	// 管理端 handler 读取的陈旧快照(同步发生在此之前)。
	stale, err := repo.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}

	// 维护循环并发完成一次同步。
	syncedAt := time.Now().UTC().Add(-time.Minute)
	nextSync := time.Now().UTC().Add(14 * time.Minute)
	if err := repo.UpdateEgressSourceSync(ctx, created.ID, syncedAt, nextSync, 7, "fetch ok"); err != nil {
		// last_sync_error 传非空以证明覆盖回滚;真实成功同步为空串,错误同步非空。
		t.Fatal(err)
	}

	// 管理端改名落库(仅配置面变更,不触碰 URL/代理 → 不应重置调度)。
	stale.Name = "feed-renamed"
	stale.Enabled = false
	if _, err := repo.UpdateEgressSource(ctx, stale); err != nil {
		t.Fatal(err)
	}

	final, err := repo.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Name != "feed-renamed" || final.Enabled {
		t.Fatalf("config columns not applied: %+v", final)
	}
	if final.LastSyncedAt == nil || !final.LastSyncedAt.Equal(syncedAt) {
		t.Fatalf("last_synced_at rolled back by admin edit: %+v (want %v)", final.LastSyncedAt, syncedAt)
	}
	if final.NextSyncAt == nil || !final.NextSyncAt.Equal(nextSync) {
		t.Fatalf("next_sync_at rolled back by admin edit: %+v (want %v)", final.NextSyncAt, nextSync)
	}
	if final.LastSyncImported != 7 {
		t.Fatalf("last_sync_imported rolled back: %d", final.LastSyncImported)
	}
	if final.LastSyncError != "fetch ok" {
		t.Fatalf("last_sync_error rolled back: %q", final.LastSyncError)
	}
}

// 配置变更(URL/代理)必须仍能显式重置调度:服务层 applySourceInput 把
// NextSyncAt 置 nil、LastSyncError 清空——仓储层按零值签名写入。
func TestUpdateEgressSourceConfigChangeReArmsSchedule(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewEgressRepository(database)

	created, err := repo.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "feed", Enabled: true, EncryptedURL: "enc-url", RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	syncedAt := time.Now().UTC().Add(-time.Minute)
	nextSync := time.Now().UTC().Add(14 * time.Minute)
	if err := repo.UpdateEgressSourceSync(ctx, created.ID, syncedAt, nextSync, 3, "old error"); err != nil {
		t.Fatal(err)
	}

	// 服务层配置变更后的域对象:NextSyncAt=nil、LastSyncError=""。
	current, err := repo.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.EncryptedURL = "enc-url-v2"
	current.NextSyncAt = nil
	current.LastSyncError = ""
	if _, err := repo.UpdateEgressSource(ctx, current); err != nil {
		t.Fatal(err)
	}

	final, err := repo.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.EncryptedURL != "enc-url-v2" {
		t.Fatalf("url not updated: %q", final.EncryptedURL)
	}
	if final.NextSyncAt != nil {
		t.Fatalf("next_sync_at must be reset on config change, got %+v", final.NextSyncAt)
	}
	if final.LastSyncError != "" {
		t.Fatalf("last_sync_error must be cleared on config change, got %q", final.LastSyncError)
	}
	// last_synced_at/last_sync_imported 是纯运行态,配置变更也不得清空。
	if final.LastSyncedAt == nil || !final.LastSyncedAt.Equal(syncedAt) {
		t.Fatalf("last_synced_at must survive config change: %+v", final.LastSyncedAt)
	}
	if final.LastSyncImported != 3 {
		t.Fatalf("last_sync_imported must survive config change: %d", final.LastSyncImported)
	}
}
