package relational

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func settingsTargetsOn(t *testing.T, db *Database, base config.Config, notify func(context.Context) error, build func(config.Config) []settingsapp.ApplyTarget) *settingsapp.Service {
	t.Helper()
	cipher, err := security.NewCipher(base.Secrets.CredentialEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeSettingsRepository(db, cipher)
	loaded, stamp, revision, err := loadSettingsConfig(context.Background(), base, repo)
	if err != nil {
		t.Fatal(err)
	}
	service := settingsapp.NewService(config.ToRuntimeSettings(loaded), stamp, revision, repo, notify, build(loaded))
	service.SetFileConfig(config.ToRuntimeSettings(base))
	return service
}

// Exercise the real admission middleware while the loaded value changes. Every
// admitted request holds a slot; the next request must fail at exactly capacity.
func assertSettingsCapacity(t *testing.T, gate *middleware.ConcurrencyGate, capacity int) {
	t.Helper()
	entered := make(chan struct{}, capacity)
	release := make(chan struct{})
	router := gin.New()
	router.Use(gate.Middleware())
	router.GET("/", func(c *gin.Context) { entered <- struct{}{}; <-release; c.Status(http.StatusOK) })
	var wg sync.WaitGroup
	defer func() { close(release); wg.Wait() }()
	for i := 0; i < capacity; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
		}()
		select {
		case <-entered:
		case <-time.After(time.Second):
			t.Fatalf("capacity smaller than %d", capacity)
		}
	}
	overflow := httptest.NewRecorder()
	// In case of a regression with excessive capacity, do not hang the test.
	done := make(chan struct{})
	wg.Add(1)
	go func() { defer wg.Done(); router.ServeHTTP(overflow, httptest.NewRequest("GET", "/", nil)); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("capacity exceeds %d", capacity)
	}
	if overflow.Code != http.StatusServiceUnavailable {
		t.Fatalf("overflow=%d", overflow.Code)
	}
}

func TestSettingsPartialApplyRealConsumers(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			dbA, dbB := settingsDatabasePair(t, dialect)
			base := settingsFileBaseline(t)
			base.Server.MaxConcurrentRequests = 1
			base.Egress.Rotation.MaxGlobalPerHour = 10
			var gateA, gateB *middleware.ConcurrencyGate
			network := egressapp.NewService(nil, nil)
			network.SetRotationConfig(egressapp.RotationConfig{MaxGlobalPerHour: 10})
			failNetwork, failPublish := true, true
			capacityCalls, networkCalls, tailCalls, publishCalls := 0, 0, 0, 0
			a := settingsTargetsOn(t, dbA, base, func(context.Context) error {
				publishCalls++
				if failPublish {
					return errors.New("private notification failure")
				}
				return nil
			}, func(loaded config.Config) []settingsapp.ApplyTarget {
				gateA = middleware.NewConcurrencyGate(loaded.Server.MaxConcurrentRequests)
				return []settingsapp.ApplyTarget{
					{Name: "capacity", Apply: func(_ context.Context, cfg settingsdomain.Config) error {
						capacityCalls++
						gateA.UpdateLimit(cfg.Server.MaxConcurrentRequests)
						return nil
					}},
					{Name: "network", Apply: func(_ context.Context, cfg settingsdomain.Config) error {
						networkCalls++
						if failNetwork {
							panic("secret configuration value")
						}
						network.SetRotationConfig(egressapp.RotationConfig{MaxGlobalPerHour: egressRotationLimit(cfg)})
						return nil
					}},
					{Name: "tail", Apply: func(context.Context, settingsdomain.Config) error { tailCalls++; return nil }},
				}
			})
			b := settingsTargetsOn(t, dbB, base, nil, func(loaded config.Config) []settingsapp.ApplyTarget {
				gateB = middleware.NewConcurrencyGate(loaded.Server.MaxConcurrentRequests)
				return []settingsapp.ApplyTarget{{Name: "capacity", Apply: func(_ context.Context, cfg settingsdomain.Config) error {
					gateB.UpdateLimit(cfg.Server.MaxConcurrentRequests)
					return nil
				}}}
			})
			input := a.Get().Config
			input.Server.MaxConcurrentRequests = 2
			input.EgressRotation.MaxGlobalPerHour = 12
			input.Audit.BufferSize++
			saved, err := a.Update(ctx, 0, input)
			if err != nil || saved.Revision != 1 || !saved.ApplyPending || saved.AppliedRevision != 0 || len(saved.RestartRequired) != 1 {
				t.Fatalf("partial save=%+v err=%v", saved, err)
			}
			if saved.ApplyTargets[0].Pending || !saved.ApplyTargets[1].Pending || saved.ApplyTargets[2].Pending || saved.ApplyTargets[1].Error != "panic" || saved.ApplyTargets[1].LastAttemptAt.IsZero() {
				t.Fatalf("states=%+v", saved.ApplyTargets)
			}
			if saved.Notification.State != "failed" || saved.Notification.Error != "publish_failed" {
				t.Fatalf("notification=%+v", saved.Notification)
			}
			assertSettingsCapacity(t, gateA, 2)
			if network.RotationConfig().MaxGlobalPerHour != 10 || tailCalls != 1 {
				t.Fatal("independent consumers were not preserved")
			}
			saved.ApplyTargets[0].Name = "mutated outside"
			if a.Get().ApplyTargets[0].Name != "capacity" {
				t.Fatal("snapshot aliases state")
			}
			if err := a.ReloadPersisted(ctx); !errors.Is(err, settingsapp.ErrApplyPending) {
				t.Fatalf("pending reload=%v", err)
			}
			if capacityCalls != 1 || tailCalls != 1 || networkCalls != 2 {
				t.Fatal("replayed successful targets")
			}
			// Lost publication does not stop another instance's authoritative read.
			observed, err := b.Read(ctx)
			if err != nil || observed.AppliedRevision != 1 || observed.ApplyPending {
				t.Fatalf("peer read=%+v %v", observed, err)
			}
			assertSettingsCapacity(t, gateB, 2)
			if _, err := b.Update(ctx, 0, observed.Config); !errors.Is(err, settingsapp.ErrConflict) {
				t.Fatalf("stale edit=%v", err)
			}
			// A newer durable intent supersedes the old failing target. A does not publish
			// a stale revision after discovering the peer's write.
			observed.Config.Server.MaxConcurrentRequests = 3
			observed.Config.EgressRotation.MaxGlobalPerHour = 17
			next, err := b.Update(ctx, 1, observed.Config)
			if err != nil || next.Revision != 2 {
				t.Fatalf("peer save=%+v %v", next, err)
			}
			failNetwork, failPublish = false, false
			read, err := a.Read(ctx)
			if err != nil || read.ApplyPending || read.AppliedRevision != 2 || read.Notification.State != "observed" {
				t.Fatalf("recovered=%+v %v", read, err)
			}
			if network.RotationConfig().MaxGlobalPerHour != 17 || capacityCalls != 2 || tailCalls != 2 || publishCalls != 2 {
				t.Fatal("new intent or publication ownership lost")
			}
			assertSettingsCapacity(t, gateA, 3)
			// Publication failure is retryable without reapplying consumers.
			failPublish = true
			saved, err = a.Update(ctx, 2, read.Config)
			if err != nil || saved.ApplyPending || saved.Notification.State != "failed" {
				t.Fatalf("publication failure=%+v %v", saved, err)
			}
			callsBefore := capacityCalls
			failPublish = false
			read, err = a.Read(ctx)
			if err != nil || read.Notification.State != "published" || capacityCalls != callsBefore {
				t.Fatalf("publication recovery=%+v %v", read, err)
			}
			var restartedGate *middleware.ConcurrencyGate
			restarted := settingsTargetsOn(t, dbB, base, nil, func(loaded config.Config) []settingsapp.ApplyTarget {
				restartedGate = middleware.NewConcurrencyGate(loaded.Server.MaxConcurrentRequests)
				return []settingsapp.ApplyTarget{{Name: "capacity", Apply: func(_ context.Context, cfg settingsdomain.Config) error {
					restartedGate.UpdateLimit(cfg.Server.MaxConcurrentRequests)
					return nil
				}}}
			})
			if restarted.Get().AppliedRevision != 3 || len(restarted.Get().RestartRequired) != 0 {
				t.Fatal("startup not built from persisted intent")
			}
			assertSettingsCapacity(t, restartedGate, 3)
			reset, err := a.ResetToDefaults(ctx, 3)
			if err != nil || reset.AppliedRevision != 4 {
				t.Fatalf("reset=%+v %v", reset, err)
			}
			if _, err := b.Read(ctx); err != nil {
				t.Fatal(err)
			}
			assertSettingsCapacity(t, gateA, 1)
			assertSettingsCapacity(t, gateB, 1)
			if err := dbA.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Read(ctx); err == nil {
				t.Fatal("unavailable storage presented as fresh settings")
			}
			if _, err := a.Update(ctx, 4, reset.Config); err == nil || errors.Is(err, repository.ErrConflict) {
				t.Fatalf("unavailable write=%v", err)
			}
			if a.Get().Revision != 4 {
				t.Fatal("failed persistence changed intent")
			}
		})
	}
}
