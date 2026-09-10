package egress

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestRotationSharedOwnershipAndRate(t *testing.T) {
	for _, driver := range []string{"memory", "redis"} {
		t.Run(driver, func(t *testing.T) {
			ctx, first, repo := newPoolServiceFixture(t)
			var firstLock, secondLock repository.DistributedLock
			var firstRate, secondRate repository.RollingRateLimiter
			if driver == "redis" {
				address := os.Getenv("TEST_REDIS_ADDRESS")
				if address == "" {
					t.Skip("requires an isolated TEST_REDIS_ADDRESS")
				}
				cfg := redisruntime.Config{Address: address, KeyPrefix: "maturity:rotation:" + time.Now().Format("150405.000000000") + ":"}
				a, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer a.Close()
				b, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer b.Close()
				firstLock, secondLock = redisruntime.NewLockStore(a), redisruntime.NewLockStore(b)
				firstRate, secondRate = a, b
			} else {
				firstLock = memory.NewLockStore()
				secondLock = firstLock
				firstRate = memory.NewRateLimiter()
				secondRate = firstRate
			}
			entered, finish := make(chan struct{}, 4), make(chan struct{})
			var finishOnce sync.Once
			unblock := func() { finishOnce.Do(func() { close(finish) }) }
			defer unblock()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				entered <- struct{}{}
				select {
				case <-finish:
					w.WriteHeader(200)
				case <-r.Context().Done():
				}
			}))
			defer func() { unblock(); server.Close() }()
			url := server.URL
			one, err := first.Create(ctx, Input{Name: "one", Enabled: true, RotationURL: &url, RotationEnabled: ptrBool(true)})
			if err != nil {
				t.Fatal(err)
			}
			two, err := first.Create(ctx, Input{Name: "two", Enabled: true, RotationURL: &url, RotationEnabled: ptrBool(true)})
			if err != nil {
				t.Fatal(err)
			}
			cfg := fastRotationConfig()
			cfg.WebhookRetries = 0
			cfg.MaxGlobalPerHour = 1
			setup := func(s *Service, lock repository.DistributedLock, rate repository.RollingRateLimiter) {
				s.SetQualityQuarantiner(&fakeQuarantiner{})
				s.SetNodeProber(&sequenceProber{results: []domain.ProbeResult{{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.99", TestedAt: time.Now()}}})
				s.SetRotationCoordination(lock, rate)
				s.SetRotationConfig(cfg)
			}
			setup(first, firstLock, firstRate)
			second := NewService(repo, first.cipher)
			setup(second, secondLock, secondRate)
			done := make(chan struct{})
			go func() { defer close(done); first.processRotation(ctx, one.ID) }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("first webhook never started")
			}
			reserved, err := repo.GetEgressNode(ctx, one.ID)
			if err != nil || reserved.RotationAttempts != 1 || reserved.LastRotatedAt == nil {
				t.Fatalf("webhook preceded durable reservation: %+v %v", reserved, err)
			}
			second.processRotation(ctx, one.ID)
			if err := second.RotateNode(ctx, one.ID); err == nil {
				t.Fatal("manual reset entered another worker's ownership")
			}
			second.processRotation(ctx, two.ID)
			if calls.Load() != 1 {
				t.Fatalf("two instances oversold node ownership or global rate: %d", calls.Load())
			}
			unblock()
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("first did not complete")
			}
			// A newly constructed service shares the rolling quota. Recreating local
			// schedulers cannot recover a consumed hour slot.
			restarted := NewService(repo, first.cipher)
			setup(restarted, secondLock, secondRate)
			restarted.processRotation(ctx, two.ID)
			if calls.Load() != 1 {
				t.Fatal("new scheduler reset shared quota")
			}
			untouched, err := repo.GetEgressNode(ctx, two.ID)
			if err != nil || untouched.RotationAttempts != 0 {
				t.Fatalf("rate rejection consumed node attempt: %+v %v", untouched, err)
			}
		})
	}
}

func TestAutomaticRotationBudgetSurvivesServiceRestart(t *testing.T) {
	ctx, service, repo := newPoolServiceFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	url := server.URL
	node, err := service.Create(ctx, Input{Name: "failing", Enabled: true, RotationURL: &url, RotationEnabled: ptrBool(true)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := fastRotationConfig()
	cfg.WebhookRetries = 0
	cfg.MinNodeInterval = time.Nanosecond
	setup := func(s *Service) { s.SetQualityQuarantiner(&fakeQuarantiner{}); setTestRotationConfig(s, cfg) }
	setup(service)
	for i := 0; i < 3; i++ {
		if err := service.RotateNodeAutomatically(ctx, node.ID); err != nil {
			t.Fatal(err)
		}
		service.processRotation(ctx, node.ID)
	}
	restarted := NewService(repo, service.cipher)
	setup(restarted)
	if err := restarted.RotateNodeAutomatically(ctx, node.ID); err == nil {
		t.Fatal("restart or automatic trigger reset exhausted budget")
	}
	restarted.processRotation(ctx, node.ID)
	stored, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil || calls.Load() != 3 || stored.RotationAttempts != 3 || stored.LastRotatedAt == nil {
		t.Fatalf("budget not durable: calls=%d node=%+v err=%v", calls.Load(), stored, err)
	}
	if err := restarted.RotateNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	restarted.processRotation(ctx, node.ID)
	stored, err = repo.GetEgressNode(ctx, node.ID)
	if err != nil || calls.Load() != 4 || stored.RotationAttempts != 1 {
		t.Fatalf("manual review did not reopen exactly one cycle: calls=%d node=%+v err=%v", calls.Load(), stored, err)
	}
}

type failingRotationReservation struct{ *relational.EgressRepository }

func (r failingRotationReservation) UpdateEgressNodeRotationState(context.Context, uint64, *time.Time, int, string) error {
	return errors.New("injected rotation write outage")
}

func (r failingRotationReservation) UpdateEgressNodeRotationStateForBinding(context.Context, domain.Node, *time.Time, int, string) error {
	return errors.New("injected rotation write outage")
}

func TestRotationReservationFailurePreventsWebhookAndManualSuccess(t *testing.T) {
	ctx, service, repo := newPoolServiceFixture(t)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1) }))
	defer server.Close()
	url := server.URL
	node, err := service.Create(ctx, Input{Name: "outage", Enabled: true, RotationURL: &url, RotationEnabled: ptrBool(true)})
	if err != nil {
		t.Fatal(err)
	}
	service.repository = failingRotationReservation{repo}
	service.SetQualityQuarantiner(&fakeQuarantiner{})
	setTestRotationConfig(service, fastRotationConfig())
	service.processRotation(ctx, node.ID)
	if calls.Load() != 0 {
		t.Fatal("webhook ran after reservation failed")
	}
	if err := repo.UpdateEgressNodeRotationState(ctx, node.ID, nil, 3, "exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := service.RotateNode(ctx, node.ID); err == nil {
		t.Fatal("manual reset reported success after storage rejected it")
	}
}

func TestRotationWorkerCanEnableAfterDisabledStartup(t *testing.T) {
	ctx, service, _ := newPoolServiceFixture(t)
	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called <- struct{}{} }))
	defer server.Close()
	url := server.URL
	node, err := service.Create(ctx, Input{Name: "hot-enable", Enabled: true, RotationURL: &url, RotationEnabled: ptrBool(true)})
	if err != nil {
		t.Fatal(err)
	}
	cfg := fastRotationConfig()
	cfg.Enabled = false
	service.SetQualityQuarantiner(&fakeQuarantiner{})
	service.SetNodeProber(&sequenceProber{results: []domain.ProbeResult{{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.2", TestedAt: time.Now()}}})
	setTestRotationConfig(service, cfg)
	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); service.RunRotationWorker(workerCtx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("worker did not stop")
		}
	}()
	select {
	case <-done:
		t.Fatal("disabled startup permanently removed worker")
	case <-time.After(20 * time.Millisecond):
	}
	cfg.Enabled = true
	service.SetRotationConfig(cfg)
	if err := service.RotateNodeAutomatically(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("hot-enabled worker did not run")
	}
}

func TestRotationQueueReportsAdmissionAndDeduplicates(t *testing.T) {
	_, service, _ := newPoolServiceFixture(t)
	service.SetRotationConfig(fastRotationConfig())
	for id := uint64(1); id <= 4096; id++ {
		if err := service.enqueueRotation(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.enqueueRotation(1); err != nil {
		t.Fatalf("duplicate is already admitted: %v", err)
	}
	if err := service.enqueueRotation(4097); !errors.Is(err, ErrRotationQueueFull) {
		t.Fatalf("full queue admission=%v", err)
	}
	service.rotation.mu.Lock()
	depth := len(service.rotation.queue)
	service.rotation.closed = true
	service.rotation.mu.Unlock()
	if depth != 4096 {
		t.Fatalf("queue grew beyond capacity: %d", depth)
	}
	if err := service.enqueueRotation(1); !errors.Is(err, ErrOperationsUnavailable) {
		t.Fatalf("closed queue admission=%v", err)
	}
}
