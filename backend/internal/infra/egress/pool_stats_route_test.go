package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// 真实路径集成:AcquirePoolRouted 选中节点后统计必须记录。
func TestAcquirePoolRoutedRecordsStats(t *testing.T) {
	manager, repo := newPoolTestManager(t)
	repo.pool[1] = domain.Pool{ID: 1, Enabled: true, Strategy: domain.PoolStrategyRotation, FallbackMode: domain.PoolFallbackNone}
	for i := uint64(1); i <= 3; i++ {
		repo.member[1] = append(repo.member[1], domain.Node{ID: i, Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy(t, manager.cipher, "http://127.0.0.1:900"+string(rune('0'+i)))})
	}
	ResetPoolStats(1)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		_, _, err := manager.AcquirePoolRouted(ctx, domain.ScopeBuild, "acct-1", 1, true, "")
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
	}
	items, _ := PoolStatsSnapshot(1)
	if len(items) != 1 {
		t.Fatalf("tracked nodes = %d, want 1 (rotation pins one member)", len(items))
	}
	if items[0].Selections != 5 {
		t.Fatalf("selections = %d, want 5", items[0].Selections)
	}
	if items[0].LastSelectedAt.IsZero() {
		t.Fatal("LastSelectedAt not set")
	}
	_ = time.Now()
}
