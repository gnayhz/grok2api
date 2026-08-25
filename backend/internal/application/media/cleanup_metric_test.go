package media

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// TestRunCleanupEmitsHeartbeat：周期清理必须每周期发射 media_cleanup_pass_total
// 心跳（证明循环存活）。注：Add 对 0 增量不落样本（零删除周期不制造噪声），
// 因此 pruned 指标只在确有删除时出现——心跳用 Inc 计数每周期必达。
func TestRunCleanupEmitsHeartbeat(t *testing.T) {
	ctxRoot := context.Background()
	database, err := relational.OpenSQLite(ctxRoot, filepath.Join(t.TempDir(), "cleanup-metric.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctxRoot); err != nil {
		t.Fatal(err)
	}
	service := NewService(
		relational.NewMediaAssetRepository(database),
		relational.NewMediaJobRepository(database),
		stubObjectStorage{},
		nil,
		Config{MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: 50 * time.Millisecond},
	)
	ctx, cancel := context.WithCancel(ctxRoot)
	go service.RunCleanup(ctx, func(error) {})
	defer cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, sample := range perfmetrics.Default.CollectAndReset() {
			if sample.Name == "media_cleanup_pass_total" && sample.Labels.Operation == "cleanup" {
				if sample.Labels.Outcome != "success" {
					t.Fatalf("outcome = %s, want success", sample.Labels.Outcome)
				}
				if sample.Count < 1 {
					t.Fatalf("heartbeat count = %d, want >= 1", sample.Count)
				}
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("media_cleanup_pass_total never emitted within 3s")
}
