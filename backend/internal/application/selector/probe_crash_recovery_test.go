package selector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	redisclient "github.com/redis/go-redis/v9"
)

func TestProbeCrashRecoversSharedCapacityBeforeTaskLease(t *testing.T) {
	ctx := context.Background()
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS requires an isolated Redis instance")
	}
	// app.New configures requestTimeout + 1 minute; the shipped default is 2h.
	lease := 2*time.Hour + time.Minute
	prefix := "maturity:" + time.Now().Format("150405.000000") + ":"
	settings := redisruntime.Config{Address: address, KeyPrefix: prefix, ConcurrencyLease: lease}
	first, err := redisruntime.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	selector := &Selector{concurrency: redisruntime.NewConcurrencyLimiter(first)}
	r, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "tasks.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	tasks := registry.NewProbeTaskStore(r)
	for id := uint64(1); id <= 2; id++ {
		if _, err := tasks.CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: id}); err != nil {
			t.Fatal(err)
		}
		// Abandon both release closures, reproducing process loss after acquisition.
		if _, err := selector.AcquireProbeResources(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	if claimed, err := tasks.ClaimPendingProbeTasks(ctx, 2); err != nil || len(claimed) != 2 {
		t.Fatalf("claims=%d err=%v", len(claimed), err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := redisruntime.Open(ctx, settings)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	peer := &Selector{concurrency: redisruntime.NewConcurrencyLimiter(second)}
	client := redisclient.NewClient(&redisclient.Options{Addr: address})
	defer client.Close()
	key := prefix + "concurrency:quality:measurements"
	remaining, err := client.ZRangeWithScores(ctx, key, 0, -1).Result()
	if err != nil || len(remaining) != 2 {
		t.Fatalf("remaining=%+v err=%v", remaining, err)
	}
	until := time.UnixMilli(int64(remaining[0].Score))
	if ttl := time.Until(until); ttl > qualityMeasurementLease || ttl < qualityMeasurementLease-5*time.Second {
		t.Fatalf("unexpected probe ttl: %v", ttl)
	}
	deadline, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if release, err := peer.AcquireProbeResources(deadline, 3); !errors.Is(err, context.DeadlineExceeded) {
		if release != nil {
			release()
		}
		t.Fatalf("live slots oversold: %v", err)
	}
	t.Logf("waiting for actual lost-process probe leases (%s), production lease remains %s", qualityMeasurementLease, lease)
	recoverCtx, recoverCancel := context.WithTimeout(ctx, qualityMeasurementLease+5*time.Second)
	defer recoverCancel()
	release, err := peer.AcquireProbeResources(recoverCtx, 3)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := r.DB().Table("q_probe_task").Where("state = ?", "running").Update("lease_until", time.Now().UTC().Add(-time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	reclaimed, err := r.ReclaimRunningProbes(ctx, "lost worker")
	if err != nil || reclaimed != 2 {
		t.Fatalf("reclaimed=%d err=%v", reclaimed, err)
	}
	t.Logf("lost process capacity recovered and %d expired tasks reclaimed", reclaimed)
	keys, err := client.Keys(ctx, prefix+"*").Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) > 0 {
		if err := client.Del(ctx, keys...).Err(); err != nil {
			t.Fatal(err)
		}
	}
}
