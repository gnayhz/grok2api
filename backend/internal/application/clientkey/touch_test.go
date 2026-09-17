package clientkey

import (
	"context"
	"errors"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func closeKeyService(t *testing.T, service *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := service.Close(ctx); err != nil {
		t.Errorf("close client key service before SQL: %v", err)
	}
}

type touchRepository struct {
	*relational.ClientKeyRepository
	touch func(context.Context, uint64) error
}

func (r touchRepository) Touch(ctx context.Context, id uint64) error { return r.touch(ctx, id) }

func newTouchService(t *testing.T, touch func(context.Context, uint64) error) (*Service, Created) {
	t.Helper()
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "touch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	service := NewService("touch-owner", touchRepository{ClientKeyRepository: relational.NewClientKeyRepository(database), touch: touch}, nil, nil, 0, 0, testCipher(t), security.RandomTokenSource{})
	t.Cleanup(func() { closeKeyService(t, service) })
	created, err := service.Create(ctx, CreateInput{Name: "touch", Enabled: true, RPMUnlimited: true, ConcurrencyUnlimited: true, BillingLimitUSDTicks: 100})
	if err != nil {
		t.Fatal(err)
	}
	return service, created
}

func authenticateTouch(t *testing.T, ctx context.Context, service *Service, secret string) {
	t.Helper()
	_, release, err := service.Authenticate(ctx, secret)
	if err != nil {
		t.Fatal(err)
	}
	release()
}

func receiveTouch(t *testing.T, entered <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-entered:
		return ctx
	case <-time.After(5 * time.Second):
		t.Fatal("Touch did not start")
		return nil
	}
}

func TestTouchOutlivesCallerAndCloseJoinsAllAcceptedWrites(t *testing.T) {
	entered := make(chan context.Context, 2)
	resume := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(resume) })
	var calls, returned atomic.Int32
	service, created := newTouchService(t, func(ctx context.Context, _ uint64) error {
		calls.Add(1)
		entered <- ctx
		<-resume
		returned.Add(1)
		return ctx.Err()
	})
	type contextKey struct{}
	ctx, cancel := context.WithTimeout(context.WithValue(context.Background(), contextKey{}, "trace"), time.Second)
	defer cancel()
	authenticateTouch(t, ctx, service, created.Secret)
	first := receiveTouch(t, entered)
	cancel()
	if first.Err() != nil || first.Value(contextKey{}) != "trace" {
		t.Fatal("caller cancellation or context values crossed the wrong boundary")
	}
	deadline, ok := first.Deadline()
	if !ok || time.Until(deadline) <= time.Second || time.Until(deadline) > 3*time.Second {
		t.Fatalf("independent touch deadline: %v, %v", deadline, ok)
	}
	for range 5 {
		authenticateTouch(t, context.Background(), service, created.Secret)
	}
	if calls.Load() != 1 {
		t.Fatalf("minute throttle allowed %d writes", calls.Load())
	}
	// Changing enabled state clears its throttle. Both writes for the same ID still
	// belong to the service and must be joined independently.
	if _, err := service.BatchSetEnabled(context.Background(), []uint64{created.Key.ID}, true); err != nil {
		t.Fatal(err)
	}
	authenticateTouch(t, context.Background(), service, created.Secret)
	second := receiveTouch(t, entered)
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer closeCancel()
	if err := service.Close(closeCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("unfinished calls must retain ownership: %v", err)
	}
	if first.Err() != context.Canceled || second.Err() != context.Canceled {
		t.Fatal("Close did not cancel every accepted write")
	}
	once.Do(func() { close(resume) })
	closeKeyService(t, service)
	if returned.Load() != 2 {
		t.Fatal("Close returned before both calls finished")
	}
	// Display shutdown does not close authentication or billing completion.
	authenticateTouch(t, context.Background(), service, created.Secret)
	if calls.Load() != 2 {
		t.Fatal("Close accepted a new display write")
	}
	if reserved, err := service.ReserveBilling(context.Background(), created.Key, "after-touch-close", 10, time.Minute); err != nil || !reserved {
		t.Fatalf("billing reservation after touch close: %v %v", reserved, err)
	}
	if err := service.CancelBilling(context.Background(), "after-touch-close"); err != nil {
		t.Fatal(err)
	}
	closeKeyService(t, service)
}

func TestTouchKeepsThreeSecondDeadlineWithoutClose(t *testing.T) {
	finished := make(chan error, 1)
	service, created := newTouchService(t, func(ctx context.Context, _ uint64) error {
		<-ctx.Done()
		finished <- ctx.Err()
		return ctx.Err()
	})
	authenticateTouch(t, context.Background(), service, created.Secret)
	select {
	case err := <-finished:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Touch timeout: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Touch lost its independent deadline")
	}
}

func TestConcurrentTouchAdmissionAndClose(t *testing.T) {
	var accepted, returned atomic.Int32
	service, created := newTouchService(t, func(ctx context.Context, _ uint64) error {
		accepted.Add(1)
		<-ctx.Done()
		returned.Add(1)
		return ctx.Err()
	})
	start := make(chan struct{})
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, release, err := service.Authenticate(context.Background(), created.Secret)
			if err != nil {
				t.Error(err)
				return
			}
			release()
		}()
	}
	for range 8 {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; closeKeyService(t, service) }()
	}
	close(start)
	workers.Wait()
	closeKeyService(t, service)
	if accepted.Load() != returned.Load() {
		t.Fatalf("accepted=%d returned=%d", accepted.Load(), returned.Load())
	}
	before := accepted.Load()
	for range 8 {
		authenticateTouch(t, context.Background(), service, created.Secret)
	}
	if accepted.Load() != before {
		t.Fatal("authentication admitted Touch after close completed")
	}
}

func TestTouchCapacityDoesNotBlockAuthenticationOrThrottleRejectedWrites(t *testing.T) {
	entered := make(chan context.Context, 1)
	service, created := newTouchService(t, func(ctx context.Context, _ uint64) error {
		entered <- ctx
		return nil
	})
	finishes := make([]func(), 0, 64)
	t.Cleanup(func() {
		for _, finish := range finishes {
			finish()
		}
	})
	for id := uint64(1000); id < 1064; id++ {
		_, finish := service.touches.start(context.Background(), id, time.Now())
		if finish == nil {
			t.Fatalf("bounded display write %d rejected", id)
		}
		finishes = append(finishes, finish)
	}
	authenticateTouch(t, context.Background(), service, created.Secret)
	service.touches.mu.Lock()
	active := len(service.touches.active)
	_, throttled := service.touches.lastTouched[created.Key.ID]
	service.touches.mu.Unlock()
	if active > 64 || throttled {
		t.Fatalf("overloaded display writes must be skipped without throttling: active=%d throttled=%v", active, throttled)
	}
	finishes[0]()
	finishes = finishes[1:]
	authenticateTouch(t, context.Background(), service, created.Secret)
	receiveTouch(t, entered)
}
