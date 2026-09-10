package app

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/quality/enforcement"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type observedRotationRate struct {
	repository.RollingRateLimiter
	decisions chan bool
}

func (r observedRotationRate) AllowRolling(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	allowed, wait, err := r.RollingRateLimiter.AllowRolling(ctx, key, limit, window)
	if err == nil {
		r.decisions <- allowed
	}
	return allowed, wait, err
}

func TestQualityRotationCommandsRespectNetworkCapacity(t *testing.T) {
	for _, driver := range []string{"memory", "redis"} {
		t.Run(driver, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			path := filepath.Join(t.TempDir(), "capacity.db")
			db, err := relational.OpenSQLite(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			quality, err := registry.Open(ctx, registry.Options{SQLitePath: path})
			if err != nil {
				t.Fatal(err)
			}
			defer quality.Close()
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			repo := relational.NewEgressRepository(db)
			networkA, networkB := egressapp.NewService(repo, cipher), egressapp.NewService(repo, cipher)
			manager := infraegress.NewManager(repo, cipher)
			defer manager.Close(context.Background())
			var lockA, lockB repository.DistributedLock
			var rateA, rateB repository.RollingRateLimiter
			if driver == "redis" {
				address := os.Getenv("TEST_REDIS_ADDRESS")
				if address == "" {
					t.Skip("TEST_REDIS_ADDRESS is not configured")
				}
				cfg := redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("quality-rotation-%d", time.Now().UnixNano())}
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
				lockA, lockB = redisruntime.NewLockStore(a), redisruntime.NewLockStore(b)
				rateA, rateB = a, b
			} else {
				lockA = memory.NewLockStore()
				lockB = lockA
				rateA = memory.NewRateLimiter()
				rateB = rateA
			}
			decisions := make(chan bool, 16)
			cfg := egressapp.DefaultRotationConfig()
			cfg.Enabled, cfg.MaxGlobalPerHour, cfg.MaxAttemptsPerQuarantine, cfg.WebhookRetries = true, 2, 1, 0
			for i, network := range []*egressapp.Service{networkA, networkB} {
				network.SetQualityQuarantiner(manager)
				network.SetRotationConfig(cfg)
				if i == 0 {
					network.SetRotationCoordination(lockA, observedRotationRate{rateA, decisions})
				} else {
					network.SetRotationCoordination(lockB, observedRotationRate{rateB, decisions})
				}
			}
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(503) }))
			defer server.Close()
			encrypted, err := cipher.Encrypt(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			nodes := make([]egressdomain.Node, 3)
			for i := range nodes {
				node, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: fmt.Sprintf("capacity-%d", i), Enabled: true, RotationEnabled: true, EncryptedRotationURL: encrypted})
				if err != nil {
					t.Fatal(err)
				}
				nodes[i] = node
			}
			source := baseNodeSource{egress: networkA}
			a := enforcement.New(enforcement.DefaultConfig(), nil, source, nil, baseRotator{egress: networkA})
			defer a.Close(context.Background())
			b := enforcement.New(enforcement.DefaultConfig(), nil, source, nil, baseRotator{egress: networkB})
			defer b.Close(context.Background())
			workerCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{}, 2)
			go func() { networkA.RunRotationWorker(workerCtx); done <- struct{}{} }()
			go func() { networkB.RunRotationWorker(workerCtx); done <- struct{}{} }()
			defer func() {
				stop()
				<-done
				<-done
				cfg.Enabled = false
				networkA.SetRotationConfig(cfg)
				networkB.SetRotationConfig(cfg)
			}()
			awaitDecision := func(want bool) {
				t.Helper()
				select {
				case got := <-decisions:
					if got != want {
						t.Fatalf("network admission=%v want=%v", got, want)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			awaitCalls := func(want int32) {
				t.Helper()
				for calls.Load() != want {
					select {
					case <-time.After(time.Millisecond):
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
				}
			}
			if err := a.RotateNode(ctx, nodes[0].ID); err != nil {
				t.Fatal(err)
			}
			awaitDecision(true)
			awaitCalls(1)
			if err := b.RotateNode(ctx, nodes[1].ID); err != nil {
				t.Fatal(err)
			}
			awaitDecision(true)
			awaitCalls(2)
			// Quality accepts the command; the shared network scheduler determines
			// whether and when it can dispatch. Its global budget is never local to A.
			if err := a.RotateNode(ctx, nodes[2].ID); err != nil {
				t.Fatal(err)
			}
			awaitDecision(false)
			untouched, err := repo.GetEgressNode(ctx, nodes[2].ID)
			if err != nil || untouched.RotationAttempts != 0 || calls.Load() != 2 {
				t.Fatal("quality bypassed the network budget")
			}
			// A network-owned capacity change admits the waiting third node without
			// changing any quality setting or maintaining another local hour counter.
			cfg.MaxGlobalPerHour = 3
			networkA.SetRotationConfig(cfg)
			networkB.SetRotationConfig(cfg)
			// The limiter publishes its decision before the first worker releases
			// the node lease. Wait for that legitimate in-flight owner to finish
			// before expecting a second instance to accept the command.
			for {
				err := b.RotateNode(ctx, nodes[2].ID)
				if err == nil {
					break
				}
				if err.Error() != "node rotation already in progress" {
					t.Fatal(err)
				}
				select {
				case <-time.After(time.Millisecond):
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			awaitDecision(true)
			awaitCalls(3)
			t.Log("quality → production adapter → two real network workers → shared limiter → HTTP webhook preserved global capacity")
		})
	}
}
