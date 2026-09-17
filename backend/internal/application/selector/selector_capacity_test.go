package selector

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Delay only the first return AFTER the actual Memory/Redis operation. This
// models an interrupted read without claiming to interrupt a Redis command.
type selectorCapacityReadGate struct {
	repository.ConcurrencyLimiter
	entered, resume chan struct{}
	reads           atomic.Int32
	failure         error
}

func (g *selectorCapacityReadGate) after(ctx context.Context, err error) error {
	if err != nil || g.reads.Add(1) != 1 {
		return err
	}
	close(g.entered)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.resume:
		return g.failure
	}
}
func (g *selectorCapacityReadGate) Current(ctx context.Context, key string) (int, error) {
	value, err := g.ConcurrencyLimiter.Current(ctx, key)
	return value, g.after(ctx, err)
}
func (g *selectorCapacityReadGate) CurrentMany(ctx context.Context, keys []string) (map[string]int, error) {
	value, err := g.ConcurrencyLimiter.(repository.ConcurrencySnapshotReader).CurrentMany(ctx, keys)
	return value, g.after(ctx, err)
}

// Hides the optional batch interface while retaining the same real store.
type singleSelectorCapacityReader struct{ repository.ConcurrencyLimiter }

func selectorCapacityLimiter(t *testing.T, backend string) repository.ConcurrencyLimiter {
	t.Helper()
	if backend == "memory" {
		return memory.NewConcurrencyLimiter()
	}
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("isolated TEST_REDIS_ADDRESS required")
	}
	store, err := redisruntime.Open(context.Background(), redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("g18-capacity-%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return redisruntime.NewConcurrencyLimiter(store)
}

func TestSelectorSharedCapacityCancellation(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			limiter := selectorCapacityLimiter(t, backend)
			for _, reader := range []string{"batch", "single"} {
				for _, scenario := range []string{"pre_canceled", "hot_canceled", "waiter_canceled", "owner_canceled", "all_canceled", "waiter_deadline", "owner_deadline", "shared_success", "storage_error", "storage_deadline"} {
					t.Run(reader+"/"+scenario, func(t *testing.T) {
						ctx := context.Background()
						key := repository.AccountConcurrencyKey(818)
						release, ok, err := limiter.Acquire(ctx, key, 1)
						if err != nil || !ok {
							t.Fatalf("seed capacity: %t %v", ok, err)
						}
						defer release()
						gate := &selectorCapacityReadGate{ConcurrencyLimiter: limiter, entered: make(chan struct{}), resume: make(chan struct{})}
						var once sync.Once
						unblock := func() { once.Do(func() { close(gate.resume) }) }
						defer unblock()
						var readerPort repository.ConcurrencyLimiter = gate
						if reader == "single" {
							readerPort = singleSelectorCapacityReader{gate}
						}
						selector := NewSelector(nil, readerPort, nil, nil, time.Hour, time.Second, time.Minute)
						storageErr := errors.New("capacity storage unavailable")
						if scenario == "storage_error" {
							gate.failure = storageErr
						}
						if scenario == "storage_deadline" {
							gate.failure = context.DeadlineExceeded
							storageErr = context.DeadlineExceeded
						}
						load := func(ctx context.Context) error {
							values, err := selector.loadConcurrencySnapshot(ctx, []string{key})
							if err == nil && values[key] != 1 {
								return fmt.Errorf("actual capacity changed to %d", values[key])
							}
							return err
						}
						if scenario == "pre_canceled" || scenario == "hot_canceled" {
							expected := int32(0)
							if scenario == "hot_canceled" {
								unblock()
								if err := load(ctx); err != nil {
									t.Fatal(err)
								}
								expected = 1
							}
							canceled, cancel := context.WithCancel(ctx)
							cancel()
							if err := load(canceled); !errors.Is(err, context.Canceled) {
								t.Fatalf("known canceled read=%v", err)
							}
							if gate.reads.Load() != expected {
								t.Fatalf("canceled request read storage: %d", gate.reads.Load())
							}
							return
						}
						ownerCtx, cancelOwner := context.WithCancel(ctx)
						if scenario == "owner_deadline" {
							cancelOwner()
							ownerCtx, cancelOwner = context.WithTimeout(ctx, 100*time.Millisecond)
						}
						defer cancelOwner()
						owner := make(chan error, 1)
						go func() { owner <- load(ownerCtx) }()
						awaitRoutingSignal(t, gate.entered)
						results := make([]chan error, 3)
						cancels := make([]context.CancelFunc, 3)
						for i := range results {
							c, cancel := context.WithCancel(ctx)
							if i == 0 && scenario == "waiter_deadline" {
								cancel()
								c, cancel = context.WithTimeout(ctx, 30*time.Millisecond)
							}
							cancels[i] = cancel
							defer cancel()
							waitCtx := &routingWaitContext{Context: c, waiting: make(chan struct{})}
							results[i] = make(chan error, 1)
							go func(done chan error) { done <- load(waitCtx) }(results[i])
							awaitRoutingSignal(t, waitCtx.waiting)
						}
						canceledWaiter := scenario == "waiter_canceled" || scenario == "waiter_deadline" || scenario == "all_canceled"
						if canceledWaiter {
							want := error(context.Canceled)
							if scenario == "waiter_deadline" {
								want = context.DeadlineExceeded
							} else {
								cancels[0]()
							}
							if err := awaitRoutingError(t, results[0]); !errors.Is(err, want) {
								t.Fatalf("canceled waiter=%v", err)
							}
							if gate.reads.Load() != 1 {
								t.Fatal("waiter cancellation interrupted owner")
							}
						}
						if scenario == "all_canceled" {
							for i := 1; i < len(cancels); i++ {
								cancels[i]()
							}
						}
						canceledOwner := scenario == "owner_canceled" || scenario == "all_canceled" || scenario == "owner_deadline"
						if canceledOwner {
							want := error(context.Canceled)
							if scenario == "owner_deadline" {
								want = context.DeadlineExceeded
							} else {
								cancelOwner()
							}
							if err := awaitRoutingError(t, owner); !errors.Is(err, want) {
								t.Fatalf("owner=%v", err)
							}
						} else {
							unblock()
							err := awaitRoutingError(t, owner)
							if gate.failure != nil {
								if !errors.Is(err, storageErr) {
									t.Fatalf("owner storage error=%v", err)
								}
							} else if err != nil {
								t.Fatal(err)
							}
						}
						for i, result := range results {
							if i == 0 && canceledWaiter {
								continue
							}
							err := awaitRoutingError(t, result)
							switch {
							case scenario == "all_canceled":
								if !errors.Is(err, context.Canceled) {
									t.Fatalf("all canceled waiter=%v", err)
								}
							case gate.failure != nil:
								if !errors.Is(err, storageErr) {
									t.Fatalf("lost shared storage cause: %v", err)
								}
							default:
								if err != nil {
									t.Fatalf("live waiter inherited owner cancellation: %v", err)
								}
							}
						}
						expected := int32(1)
						if canceledOwner && scenario != "all_canceled" {
							expected = 2
						}
						if gate.reads.Load() != expected {
							t.Fatalf("reads=%d want=%d", gate.reads.Load(), expected)
						}
						// No canceled/error result can occupy the key permanently.
						unblock()
						if err := load(ctx); err != nil {
							t.Fatalf("next read: %v", err)
						}
						// The observed value must not weaken the final atomic limit.
						extra, acquired, err := limiter.Acquire(ctx, key, 1)
						if acquired {
							extra()
						}
						if err != nil || acquired {
							t.Fatalf("capacity exceeded: %t %v", acquired, err)
						}
						release()
						current, err := limiter.Current(ctx, key)
						if err != nil || current != 0 {
							t.Fatalf("capacity leak: %d %v", current, err)
						}
					})
				}
			}
		})
	}
}

func TestSelectorCapacityCancellationAcrossPlans(t *testing.T) {
	db := openRoutingCancellationDB(t, "sqlite")
	repo := relational.NewAccountRepository(db)
	values, excluded := selectorEligibilityAccounts(t, repo, 100)
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			limiter := selectorCapacityLimiter(t, backend)
			for _, path := range []string{"ordinary", "session", "segmented", "session_segmented"} {
				t.Run(path, func(t *testing.T) {
					ctx := context.Background()
					gate := &selectorCapacityReadGate{ConcurrencyLimiter: limiter, entered: make(chan struct{}), resume: make(chan struct{})}
					defer close(gate.resume)
					selector := NewSelector(repo, gate, memory.NewStickyStore(), nil, time.Hour, time.Second, time.Minute)
					selector.UpdateConfig(time.Hour, time.Second, time.Minute, 0)
					if path == "segmented" || path == "session_segmented" {
						selector.UpdateSegmentedSelector(true, 100, 8)
					}
					// Finish SQL before measuring which request owns the capacity read.
					if _, err := selector.loadCandidates(ctx, account.ProviderBuild, 0, "grok-test", "", time.Now()); err != nil {
						t.Fatal(err)
					}
					acquire := func(ctx context.Context) error {
						var lease *accountLease
						var err error
						if path == "session" || path == "session_segmented" {
							var session *selectionSession
							session, err = selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false)
							if err == nil {
								lease, err = session.Acquire(ctx, excluded, false)
							}
						} else {
							if s, serr := selector.beginSelectionSession(ctx, account.ProviderBuild, 0, "grok-test", "", "", excluded, false); serr != nil {
								lease, err = nil, serr
							} else {
								lease, err = s.Acquire(ctx, excluded, false)
							}
						}
						if lease != nil {
							if (path == "segmented" || path == "session_segmented") && lease.selectorObservation == nil {
								err = errors.New("segmented plan not exercised")
							}
							lease.Release()
						}
						return err
					}
					ownerCtx, cancel := context.WithCancel(ctx)
					defer cancel()
					owner := make(chan error, 1)
					go func() { owner <- acquire(ownerCtx) }()
					awaitRoutingSignal(t, gate.entered)
					liveCtx := &routingWaitContext{Context: ctx, waiting: make(chan struct{})}
					live := make(chan error, 1)
					go func() { live <- acquire(liveCtx) }()
					awaitRoutingSignal(t, liveCtx.waiting)
					cancel()
					if err := awaitRoutingError(t, owner); !errors.Is(err, context.Canceled) {
						t.Fatalf("owner cancellation: %v", err)
					}
					if err := awaitRoutingError(t, live); err != nil {
						t.Fatalf("live plan failed: %v", err)
					}
					if gate.reads.Load() != 2 {
						t.Fatalf("capacity reads=%d want one takeover", gate.reads.Load())
					}
					assertSelectorCapacityReleased(t, limiter, values)
					if err := acquire(ctx); err != nil {
						t.Fatalf("next acquire: %v", err)
					}
					assertSelectorCapacityReleased(t, limiter, values)
				})
			}
		})
	}
}

func TestSelectorCapacityCachePreservesFreshnessAndFinalAuthority(t *testing.T) {
	for _, backend := range []string{"memory", "redis"} {
		t.Run(backend, func(t *testing.T) {
			limiter := selectorCapacityLimiter(t, backend)
			gate := &selectorCapacityReadGate{ConcurrencyLimiter: limiter, entered: make(chan struct{}), resume: make(chan struct{})}
			close(gate.resume)
			selector := NewSelector(nil, gate, nil, nil, time.Hour, time.Second, time.Minute)
			key := repository.AccountConcurrencyKey(919)
			values, err := selector.loadConcurrencySnapshot(context.Background(), []string{key})
			if err != nil || values[key] != 0 {
				t.Fatalf("cold: %v %v", values, err)
			}
			release, ok, err := limiter.Acquire(context.Background(), key, 1)
			if err != nil || !ok {
				t.Fatal(err)
			}
			defer release()
			values, err = selector.loadConcurrencySnapshot(context.Background(), []string{key})
			if err != nil || values[key] != 0 || gate.reads.Load() != 1 {
				t.Fatalf("hot: %v %v reads=%d", values, err, gate.reads.Load())
			}
			extra, ok, err := limiter.Acquire(context.Background(), key, 1)
			if ok {
				extra()
			}
			if err != nil || ok {
				t.Fatalf("stale zero bypassed atomic capacity: %t %v", ok, err)
			}
			time.Sleep(concurrencySnapshotTTL + 5*time.Millisecond)
			values, err = selector.loadConcurrencySnapshot(context.Background(), []string{key})
			if err != nil || values[key] != 1 || gate.reads.Load() != 2 {
				t.Fatalf("expired: %v %v reads=%d", values, err, gate.reads.Load())
			}
			selector.concurrencySnapshots = nil
			values, err = selector.loadConcurrencySnapshot(context.Background(), []string{key})
			if err != nil || values[key] != 1 {
				t.Fatalf("manual selector without cache: %v %v", values, err)
			}
		})
	}
}
