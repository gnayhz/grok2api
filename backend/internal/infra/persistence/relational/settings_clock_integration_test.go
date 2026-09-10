package relational

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// Each pair owns two real connections. PostgreSQL uses a private schema even
// when other integration packages are sharing the same isolated test database.
func settingsDatabasePair(t *testing.T, dialect string) (*Database, *Database) {
	t.Helper()
	ctx := context.Background()
	var open func() (*Database, error)
	if dialect == "postgres" {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		admin, err := OpenPostgres(ctx, dsn, 4, 4)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = admin.Close() })
		schema := fmt.Sprintf("settings_clock_%d", time.Now().UnixNano())
		if err := admin.db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := admin.db.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
				t.Error(err)
			}
		})
		parsed, err := url.Parse(dsn)
		if err != nil || parsed.Scheme == "" {
			t.Fatal("settings integration requires a PostgreSQL URL")
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		open = func() (*Database, error) { return OpenPostgres(ctx, parsed.String(), 4, 4) }
	} else {
		path := filepath.Join(t.TempDir(), "clock.db")
		open = func() (*Database, error) { return OpenSQLite(ctx, path) }
	}
	a, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	if err := a.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return a, b
}

func settingsFileBaseline(t *testing.T) config.Config {
	t.Helper()
	t.Setenv(config.DatabaseURLEnv, "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"), 0600); err != nil {
		t.Fatal(err)
	}
	base, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return base
}

func settingsServiceOn(t *testing.T, db *Database, base config.Config, notify func(context.Context), apply func(config.Config)) *settingsapp.Service {
	t.Helper()
	cipher, err := security.NewCipher(base.Secrets.CredentialEncryptionKey)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRuntimeSettingsRepository(db, cipher)
	loaded, stamp, revision, err := settingsapp.LoadPersisted(context.Background(), base, repo)
	if err != nil {
		t.Fatal(err)
	}
	var publish func(context.Context) error
	if notify != nil {
		publish = func(ctx context.Context) error { notify(ctx); return nil }
	}
	var targets []settingsapp.ApplyTarget
	if apply != nil {
		targets = []settingsapp.ApplyTarget{{Name: "test", Apply: func(_ context.Context, next config.Config) error { apply(next); return nil }}}
	}
	service := settingsapp.NewService(loaded, stamp, revision, repo, publish, targets)
	service.SetFileConfig(base)
	return service
}

func TestRuntimeSettingsDurableClockIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			dbA, dbB := settingsDatabasePair(t, dialect)
			baseA := settingsFileBaseline(t)
			baseB := baseA
			baseB.Server.MaxConcurrentRequests = 1536
			a := settingsServiceOn(t, dbA, baseA, nil, nil)
			change := a.Get()
			change.Config.Server.MaxConcurrentRequests = 2048
			if _, err := a.Update(ctx, change.Revision, change.Config); err != nil {
				t.Fatal(err)
			}
			reset, err := a.ResetToDefaults(ctx, a.Get().Revision)
			if err != nil || reset.Revision != 2 {
				t.Fatalf("reset revision=%d err=%v", reset.Revision, err)
			}
			// Restart/bootstrap must preserve the reset marker through schema migrations.
			if err := dbB.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			b := settingsServiceOn(t, dbB, baseB, nil, nil)
			if got := b.Get(); got.Revision != 2 || got.Config.Server.MaxConcurrentRequests != 1536 {
				t.Fatalf("reset bootstrap revision=%d max=%d", got.Revision, got.Config.Server.MaxConcurrentRequests)
			}
			change = b.Get()
			change.Config.Server.MaxConcurrentRequests = 3072
			if _, err := b.Update(ctx, change.Revision, change.Config); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if err := a.ReloadPersisted(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if got := a.Get(); got.Revision != 3 || got.Config.Server.MaxConcurrentRequests != 3072 {
				t.Fatalf("A did not converge: revision=%d max=%d", got.Revision, got.Config.Server.MaxConcurrentRequests)
			}
			if _, err := b.ResetToDefaults(ctx, 3); err != nil {
				t.Fatal(err)
			}
			if err := a.ReloadPersisted(ctx); err != nil {
				t.Fatal(err)
			}
			if a.Get().Config.Server.MaxConcurrentRequests != baseA.Server.MaxConcurrentRequests || b.Get().Config.Server.MaxConcurrentRequests != 1536 {
				t.Fatal("reset did not restore each instance's own baseline")
			}
			if _, err := a.ResetToDefaults(ctx, 4); err != nil {
				t.Fatal(err)
			}
			if _, err := b.ResetToDefaults(ctx, 4); !errors.Is(err, settingsapp.ErrConflict) {
				t.Fatalf("stale reset: %v", err)
			}
			// Save versus reset on separate service locks and separate SQL connections.
			for round := 0; round < 8; round++ {
				if err := a.ReloadPersisted(ctx); err != nil {
					t.Fatal(err)
				}
				if err := b.ReloadPersisted(ctx); err != nil {
					t.Fatal(err)
				}
				before := a.Get()
				input := before.Config
				input.Server.MaxConcurrentRequests = 4000 + round
				start := make(chan struct{})
				results := make(chan error, 2)
				go func() { <-start; _, err := a.Update(ctx, before.Revision, input); results <- err }()
				go func() { <-start; _, err := b.ResetToDefaults(ctx, before.Revision); results <- err }()
				close(start)
				wins, conflicts := 0, 0
				for range 2 {
					err := <-results
					if err == nil {
						wins++
					} else if errors.Is(err, settingsapp.ErrConflict) {
						conflicts++
					} else {
						t.Fatal(err)
					}
				}
				if wins != 1 || conflicts != 1 {
					t.Fatalf("round %d: wins=%d conflicts=%d", round, wins, conflicts)
				}
				for _, service := range []*settingsapp.Service{a, b} {
					if err := service.ReloadPersisted(ctx); err != nil {
						t.Fatal(err)
					}
					if service.Get().Revision != before.Revision+1 {
						t.Fatal("CAS did not advance exactly once")
					}
				}
			}
			before := a.Get().Revision
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			if _, err := a.ResetToDefaults(canceled, before); err == nil {
				t.Fatal("canceled reset succeeded")
			}
			restarted := settingsServiceOn(t, dbB, baseB, nil, nil)
			if restarted.Get().Revision != before {
				t.Fatal("cancel changed durable revision")
			}
			t.Logf("save=1 reset=2 restart=2 save=3; distinct baselines, repeated reset and 8 concurrent CAS rounds passed; final revision=%d", before)
		})
	}
}

func TestRuntimeSettingsRedisReconcileIntegration(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("TEST_REDIS_ADDRESS is not configured")
	}
	dialect := "sqlite"
	if os.Getenv("TEST_POSTGRES_DSN") != "" {
		dialect = "postgres"
	}
	dbA, dbB := settingsDatabasePair(t, dialect)
	base := settingsFileBaseline(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	prefix := fmt.Sprintf("settings-clock-%d", time.Now().UnixNano())
	storeA, err := redisruntime.Open(ctx, redisruntime.Config{Address: address, KeyPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := redisruntime.Open(ctx, redisruntime.Config{Address: address, KeyPrefix: prefix})
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	a := settingsServiceOn(t, dbA, base, nil, nil)
	b := settingsServiceOn(t, dbB, base, nil, nil)
	heard := make(chan struct{}, 1)
	listenCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		done <- storeB.ListenSettingsChanges(listenCtx, func(ctx context.Context) error {
			if err := b.ReloadPersisted(ctx); err != nil {
				return err
			}
			select {
			case heard <- struct{}{}:
			default:
			}
			return nil
		})
	}()
	stopListening := sync.OnceFunc(func() {
		stop()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	defer stopListening()
	// Publish until the listener acknowledges readiness; no fixed sleep assumes subscription.
	ready := false
	for !ready {
		if err := storeA.PublishSettingsChanged(ctx); err != nil {
			t.Fatal(err)
		}
		select {
		case <-heard:
			ready = true
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	input := a.Get()
	input.Config.Server.MaxConcurrentRequests = 2222
	if _, err := a.Update(ctx, input.Revision, input.Config); err != nil {
		t.Fatal(err)
	}
	if err := storeA.PublishSettingsChanged(ctx); err != nil {
		t.Fatal(err)
	}
	for b.Get().Revision != a.Get().Revision {
		select {
		case <-heard:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if b.Get().Config.Server.MaxConcurrentRequests != 2222 {
		t.Fatal("notification did not apply persisted settings")
	}
	stopListening()
	// Drop notification intentionally: the same reload used by periodic reconcile recovers reset.
	if _, err := a.ResetToDefaults(ctx, a.Get().Revision); err != nil {
		t.Fatal(err)
	}
	if err := b.ReloadPersisted(ctx); err != nil {
		t.Fatal(err)
	}
	if b.Get().Revision != a.Get().Revision || b.Get().Config.Server.MaxConcurrentRequests != base.Server.MaxConcurrentRequests {
		t.Fatal("missed notification prevented reset convergence")
	}
	t.Log("real Redis notification and notification-free SQL reconciliation passed")
}

func TestRuntimeSettingsResetApplyReplay(t *testing.T) {
	db, _ := settingsDatabasePair(t, "sqlite")
	base := settingsFileBaseline(t)
	var mu sync.Mutex
	calls := 0
	service := settingsServiceOn(t, db, base, nil, func(config.Config) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls == 2 {
			panic("reset apply interruption")
		}
	})
	ctx := context.Background()
	first := service.Get()
	first.Config.Server.MaxConcurrentRequests = 2048
	if _, err := service.Update(ctx, first.Revision, first.Config); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResetToDefaults(ctx, service.Get().Revision); err != nil {
		t.Fatal(err)
	}
	if err := service.ReloadPersisted(ctx); err != nil {
		t.Fatal(err)
	}
	if calls != 3 || service.Get().Revision != 2 {
		t.Fatalf("reset apply replay calls=%d revision=%d", calls, service.Get().Revision)
	}
}
