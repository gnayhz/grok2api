package redis

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	redisclient "github.com/redis/go-redis/v9"
)

func TestConcurrencyReleaseWorkerBoundsOutstandingRetries(t *testing.T) {
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	defer func() { perfmetrics.Default = previous }()
	client := redisclient.NewClient(&redisclient.Options{Addr: "unused:6379"})
	client.AddHook(releasePipelineHook{err: errors.New("release unavailable")})
	store := &Store{
		client:                  client,
		concurrencyReleaseQueue: make(chan concurrencyReleaseRetry, concurrencyReleaseRetryQueueCapacity),
		concurrencyReleaseStop:  make(chan struct{}), concurrencyReleaseDone: make(chan struct{}),
	}
	go store.runConcurrencyReleaseRetries()
	defer store.Close()
	fillReleaseRetries(t, store, "concurrency:test", 2*concurrencyReleaseRetryQueueCapacity)
	select {
	case store.concurrencyReleaseQueue <- concurrencyReleaseRetry{redisKey: "concurrency:test", token: "overflow", expiresAt: time.Now().Add(time.Hour)}:
		t.Error("worker accepted retries beyond both bounded buffers")
	case <-time.After(time.Second):
	}
	store.enqueueConcurrencyReleaseRetry(concurrencyReleaseRetry{redisKey: "concurrency:test", token: "overflow-metric", expiresAt: time.Now().Add(time.Hour)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	assertConcurrencyReleaseMetrics(t, registry.CollectAndReset(), map[string]int64{
		"shutdown": 2 * concurrencyReleaseRetryQueueCapacity, "dropped": 1,
	})
}

// Populate distinct, unexpired retry identities through the actual worker's
// intake. Blocking here makes the test independent of goroutine scheduling.
func fillReleaseRetries(t *testing.T, store *Store, key string, count int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for i := 0; i < count; i++ {
		value := concurrencyReleaseRetry{redisKey: key, token: fmt.Sprintf("fixture-%d", i), expiresAt: time.Now().Add(time.Hour)}
		select {
		case store.concurrencyReleaseQueue <- value:
		case <-ctx.Done():
			t.Fatalf("retry intake stalled at %d: %v", i, ctx.Err())
		}
	}
}

func TestConcurrencyReleaseCapacityRecoversSharedLeases(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("requires isolated TEST_REDIS_ADDRESS")
	}
	registry := perfmetrics.NewRegistry()
	previous := perfmetrics.Default
	perfmetrics.Default = registry
	defer func() { perfmetrics.Default = previous }()
	database, err := strconv.Atoi(os.Getenv("TEST_REDIS_DATABASE"))
	if err != nil && os.Getenv("TEST_REDIS_DATABASE") != "" {
		t.Fatal(err)
	}
	ctx := context.Background()
	cfg := Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: database, KeyPrefix: fmt.Sprintf("release-capacity-%d:", time.Now().UnixNano())}
	store, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	observer, err := Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	fault := &releaseOutageHook{}
	store.client.AddHook(fault)
	limiter, other := NewConcurrencyLimiter(store), NewConcurrencyLimiter(observer)
	release, ok, err := limiter.AcquireBounded(ctx, "account:1", 1, time.Hour)
	if err != nil || !ok {
		t.Fatalf("acquire: %t %v", ok, err)
	}
	defer release()
	fault.offline.Store(true)
	defer fault.offline.Store(false)
	release()
	release() // The public release remains idempotent while queued.
	if count, err := other.Current(ctx, "account:1"); err != nil || count != 1 {
		t.Fatalf("failed release must retain shared lease: %d %v", count, err)
	}

	// Real Redis holds these distinct lease members. Synthetic intake fills the
	// worker without requiring 32K independent account requests; the account
	// above exercises the public acquisition/release contract across two stores.
	fillerKey := store.key("concurrency", "fixture")
	defer observer.client.Del(ctx, fillerKey, store.key("concurrency", "account:1"))
	count := 2*concurrencyReleaseRetryQueueCapacity - 1
	members := make([]redisclient.Z, count)
	for i := range members {
		members[i] = redisclient.Z{Score: float64(time.Now().Add(time.Hour).UnixMilli()), Member: fmt.Sprintf("fixture-%d", i)}
	}
	if err := observer.client.ZAdd(ctx, fillerKey, members...).Err(); err != nil {
		t.Fatal(err)
	}
	fillReleaseRetries(t, store, fillerKey, count)
	failedDeadline := time.Now().Add(5 * time.Second)
	for fault.pipelineFailures.Load() == 0 {
		if time.Now().After(failedDeadline) {
			t.Fatal("test did not exercise a failed background retry")
		}
		time.Sleep(5 * time.Millisecond)
	}
	fault.offline.Store(false)
	deadline := time.Now().Add(45 * time.Second)
	for {
		remaining, err := observer.client.ZCard(ctx, fillerKey).Result()
		active, currentErr := other.Current(ctx, "account:1")
		if err != nil || currentErr != nil {
			t.Fatalf("read recovery: %v %v", err, currentErr)
		}
		if remaining == 0 && active == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("full retry buffers failed to resume: filler=%d account=%d", remaining, active)
		}
		time.Sleep(20 * time.Millisecond)
	}
	registry.CollectAndReset()

	// An expired old lease cannot remove a successor after the retry resumes.
	old, ok, err := limiter.AcquireBounded(ctx, "account:1", 1, 100*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("old owner: %t %v", ok, err)
	}
	fault.offline.Store(true)
	old()
	time.Sleep(150 * time.Millisecond)
	next, ok, err := other.AcquireBounded(ctx, "account:1", 1, time.Hour)
	if err != nil || !ok {
		t.Fatalf("successor: %t %v", ok, err)
	}
	defer next()
	fault.offline.Store(false)
	expired := false
	deadline = time.Now().Add(5 * time.Second)
	for !expired {
		for _, sample := range registry.CollectAndReset() {
			if sample.Name == "runtime_concurrency_release_total" && sample.Labels.Outcome == "expired" && sample.Total > 0 {
				expired = true
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("expired retry was not processed")
		}
		if !expired {
			time.Sleep(5 * time.Millisecond)
		}
	}
	old()
	if active, err := other.Current(ctx, "account:1"); err != nil || active != 1 {
		t.Fatalf("late retry removed successor: %d %v", active, err)
	}
	next()
	if active, err := other.Current(ctx, "account:1"); err != nil || active != 0 {
		t.Fatalf("successor release: %d %v", active, err)
	}
}

type releaseOutageHook struct {
	releasePipelineHook
	offline          atomic.Bool
	pipelineFailures atomic.Int64
}

func (h *releaseOutageHook) ProcessHook(next redisclient.ProcessHook) redisclient.ProcessHook {
	return func(ctx context.Context, cmd redisclient.Cmder) error {
		if h.offline.Load() && cmd.Name() == "evalsha" && len(cmd.Args()) > 1 && cmd.Args()[1] == releaseLeaseScript.Hash() {
			err := errors.New("injected release outage")
			cmd.SetErr(err)
			return err
		}
		return next(ctx, cmd)
	}
}

func (h *releaseOutageHook) ProcessPipelineHook(next redisclient.ProcessPipelineHook) redisclient.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redisclient.Cmder) error {
		if h.offline.Load() {
			h.pipelineFailures.Add(1)
			err := errors.New("injected background release outage")
			for _, cmd := range cmds {
				cmd.SetErr(err)
			}
			return err
		}
		return next(ctx, cmds)
	}
}
