package account

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func quotaQueueDatabase(t testing.TB, dialect string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if dialect == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "quota.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			t.Fatal(openErr)
		}
		schema := fmt.Sprintf("g27_quota_%d", time.Now().UnixNano())
		if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		db, err = relational.OpenPostgres(ctx, parsed.String(), 8, 4)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}

type quotaHTTPFixture struct {
	service              *Service
	repo                 *relational.AccountRepository
	id                   uint64
	calls, status, clock atomic.Int64
}

func newQuotaHTTPFixture(t testing.TB, dialect string, pair quotaRuntimePair, handler func(http.ResponseWriter, *http.Request)) *quotaHTTPFixture {
	t.Helper()
	ctx := context.Background()
	db := quotaQueueDatabase(t, dialect)
	f := &quotaHTTPFixture{repo: relational.NewAccountRepository(db)}
	f.clock.Store(time.Now().UTC().UnixNano())
	f.status.Store(http.StatusServiceUnavailable)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic")
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := f.repo.UpsertByIdentity(ctx, accountdomain.Credential{Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, SourceKey: "quota", Name: "quota", EncryptedAccessToken: token, AuthStatus: accountdomain.AuthStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	f.id = v.ID
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		if r.URL.Path != "/rest/rate-limits" {
			t.Errorf("unexpected upstream path: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
			return
		}
		if handler != nil {
			handler(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(int(f.status.Load()))
		_, _ = io.WriteString(w, `{"remainingQueries":5,"totalQueries":30,"windowSizeSeconds":3600}`)
	}))
	t.Cleanup(server.Close)
	adapter, closeNetwork := NewQuotaQueueWebFixture(db, cipher, server.URL)
	t.Cleanup(func() { _ = closeNetwork(ctx) })
	f.service = NewService(f.repo, nil, nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, pair.lock)
	f.service.SetQuotaRefreshCoordinator(pair.first)
	f.service.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	f.service.now = func() time.Time { return time.Unix(0, f.clock.Load()).UTC() }
	return f
}

func (f *quotaHTTPFixture) runOne(t testing.TB, ctx context.Context) {
	t.Helper()
	var request quotaRefreshRequest
	select {
	case request = <-f.service.quotaRefresh.queue:
	default:
		t.Fatal("expected pending quota request")
	}
	f.service.quotaRefresh.mu.Lock()
	state := f.service.quotaRefresh.obs[request.key]
	state.queued = false
	state.running = true
	state.pending = false
	f.service.quotaRefresh.mu.Unlock()
	f.service.runQuotaRefresh(ctx, request)
	if stats := f.service.syncPool.Snapshot(); stats.Active != 0 || stats.Queued != 0 {
		t.Fatalf("quota pool retained work: %+v", stats)
	}
}

func (f *quotaHTTPFixture) retry() {
	f.clock.Add(int64(quotaRefreshBackoffMax + time.Minute))
	f.service.requeueQuotaRefreshes()
	f.service.recoverSharedQuotaRefreshes(context.Background(), f.service.now())
	f.service.requeueQuotaRefreshes()
}

func TestQuotaQueueActualHTTPBudgetAndNewDemand(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			for name, pair := range quotaTestStores(t) {
				t.Run(name, func(t *testing.T) {
					for _, durable := range []bool{false, true} {
						t.Run(fmt.Sprint("durable=", durable), func(t *testing.T) {
							ctx := context.Background()
							f := newQuotaHTTPFixture(t, dialect, pair, nil)
							fact := accountdomain.QuotaConsumption{EventID: fmt.Sprint(name, durable), AccountID: f.id, Mode: "fast", Units: 1}
							if durable {
								if _, err := f.repo.ConsumeQuota(ctx, fact, f.service.now()); err != nil {
									t.Fatal(err)
								}
								f.service.recoverDurableQuotaRefreshes(ctx, 0)
							} else {
								f.service.QueueQuotaRefresh(f.id, "fast")
							}
							for range quotaRefreshFailureBudget {
								f.runOne(t, ctx)
								f.retry()
							}
							for range 3 {
								f.service.recoverDurableQuotaRefreshes(ctx, 0)
								f.service.recoverSharedQuotaRefreshes(ctx, f.service.now())
								f.service.requeueQuotaRefreshes()
							}
							if got := f.calls.Load(); got != quotaRefreshFailureBudget || len(f.service.quotaRefresh.queue) != 0 {
								t.Fatalf("same demand resumed: calls=%d queue=%d", got, len(f.service.quotaRefresh.queue))
							}
							if durable {
								if receipt, err := f.repo.ConsumeQuota(ctx, fact, f.service.now()); err != nil || receipt.State != accountdomain.QuotaConsumptionPendingRefresh {
									t.Fatalf("parking changed durable fact: %+v %v", receipt, err)
								}
							}
							// A second coordinator instance observes the same dirty identity without
							// resetting the episode, but publishing a new identity must resume it.
							version, dirty, err := pair.second.GetQuotaRefreshState(ctx, f.id, "fast")
							if err != nil || !dirty {
								t.Fatalf("lost shared demand: %+v %v %v", version, dirty, err)
							}
							if _, err = pair.second.MarkQuotaRefreshDirty(ctx, f.id, "fast", quotaRefreshDirtyTTL); err != nil {
								t.Fatal(err)
							}
							f.status.Store(200)
							f.service.recoverSharedQuotaRefreshes(ctx, f.service.now())
							f.runOne(t, ctx)
							if f.calls.Load() != quotaRefreshFailureBudget+1 {
								t.Fatalf("new demand calls=%d", f.calls.Load())
							}
							if _, dirty, err = pair.first.GetQuotaRefreshState(ctx, f.id, "fast"); err != nil || dirty {
								t.Fatalf("successful new demand not confirmed: %v %v", dirty, err)
							}
							if durable {
								if receipt, err := f.repo.ConsumeQuota(ctx, fact, f.service.now()); err != nil || receipt.State != accountdomain.QuotaConsumptionRefreshed {
									t.Fatalf("successful query did not resolve SQL: %+v %v", receipt, err)
								}
							}
							if pair.lock != nil {
								release, acquired, err := pair.lock.Acquire(ctx, "quota-refresh:"+strconv.FormatUint(f.id, 10)+":fast", time.Second)
								if err != nil || !acquired {
									t.Fatalf("shared lock leaked: %v %v", acquired, err)
								}
								release()
							}
						})
					}
				})
			}
		})
	}
}

type quotaCompletionFault struct {
	repository.QuotaRefreshCoordinator
	phase    string
	reads    int
	disabled bool
}

func (c *quotaCompletionFault) MarkQuotaRefreshDirty(ctx context.Context, id uint64, mode string, ttl time.Duration) (repository.QuotaRefreshVersion, error) {
	if !c.disabled && c.phase == "publish" {
		return repository.QuotaRefreshVersion{}, errors.New("publish unavailable")
	}
	return c.QuotaRefreshCoordinator.MarkQuotaRefreshDirty(ctx, id, mode, ttl)
}
func (c *quotaCompletionFault) GetQuotaRefreshState(ctx context.Context, id uint64, mode string) (repository.QuotaRefreshVersion, bool, error) {
	c.reads++
	if !c.disabled && c.phase == "read" {
		return repository.QuotaRefreshVersion{}, false, errors.New("read unavailable")
	}
	return c.QuotaRefreshCoordinator.GetQuotaRefreshState(ctx, id, mode)
}
func (c *quotaCompletionFault) ClearQuotaRefreshDirty(ctx context.Context, id uint64, mode string, version repository.QuotaRefreshVersion) (bool, error) {
	if !c.disabled {
		switch c.phase {
		case "clear_error":
			return false, errors.New("clear unavailable")
		case "clear_false":
			return false, nil
		}
	}
	return c.QuotaRefreshCoordinator.ClearQuotaRefreshDirty(ctx, id, mode, version)
}

func TestQuotaQueueCompletionFailuresUseBudget(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			for _, phase := range []string{"publish", "read", "clear_error", "clear_false"} {
				t.Run(phase, func(t *testing.T) {
					ctx := context.Background()
					f := newQuotaHTTPFixture(t, "sqlite", pair, nil)
					f.status.Store(200)
					fault := &quotaCompletionFault{QuotaRefreshCoordinator: pair.first, phase: phase}
					f.service.SetQuotaRefreshCoordinator(fault)
					f.service.QueueQuotaRefresh(f.id, "fast")
					for range quotaRefreshFailureBudget {
						before := f.calls.Load()
						f.runOne(t, ctx)
						if got := f.calls.Load() - before; got > 1 {
							t.Fatalf("completion failure queried without yielding: phase=%s calls=%d", phase, got)
						}
						f.retry()
					}
					if len(f.service.quotaRefresh.queue) != 0 {
						t.Fatalf("completion failure bypassed parking: %s", phase)
					}
					state := f.service.quotaRefresh.obs[strconv.FormatUint(f.id, 10)+":fast"]
					if state == nil || state.failures != quotaRefreshFailureBudget {
						t.Fatalf("lost completion failure budget: %+v", state)
					}
					fault.disabled = true
					f.service.QueueQuotaRefresh(f.id, "fast")
					f.runOne(t, ctx)
					if len(f.service.quotaRefresh.obs) != 0 {
						t.Fatalf("explicit demand did not recover after %s: %+v", phase, f.service.QuotaRefreshStats())
					}
				})
			}
		})
	}
}

func TestQuotaQueueNewDemandDuringFailedAttempt(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			for _, source := range []string{"local", "shared"} {
				t.Run(source, func(t *testing.T) {
					entered, release := make(chan struct{}), make(chan struct{})
					var requests atomic.Int64
					f := newQuotaHTTPFixture(t, "sqlite", pair, func(w http.ResponseWriter, r *http.Request) {
						if requests.Add(1) == 1 {
							close(entered)
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
							w.WriteHeader(503)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"remainingQueries":5,"totalQueries":30,"windowSizeSeconds":3600}`)
					})
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					defer func() {
						select {
						case <-release:
						default:
							close(release)
						}
					}()
					f.service.QueueQuotaRefresh(f.id, "fast")
					done := make(chan struct{})
					go func() { defer close(done); f.runOne(t, ctx) }()
					select {
					case <-entered:
					case <-time.After(3 * time.Second):
						t.Fatal("quota HTTP did not start")
					}
					if source == "local" {
						f.service.QueueQuotaRefresh(f.id, "fast")
					} else {
						if _, err := pair.second.MarkQuotaRefreshDirty(ctx, f.id, "fast", quotaRefreshDirtyTTL); err != nil {
							t.Fatal(err)
						}
						f.service.recoverSharedQuotaRefreshes(ctx, f.service.now())
					}
					close(release)
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("old attempt did not exit")
					}
					state := f.service.quotaRefresh.obs[strconv.FormatUint(f.id, 10)+":fast"]
					if state == nil || state.failures != 0 || !state.pending || state.running {
						t.Fatalf("old attempt charged new demand: %+v", state)
					}
					f.service.requeueQuotaRefreshes()
					f.runOne(t, ctx)
					if f.calls.Load() != 2 || len(f.service.quotaRefresh.obs) != 0 {
						t.Fatalf("new demand did not finish once: calls=%d states=%d", f.calls.Load(), len(f.service.quotaRefresh.obs))
					}
				})
			}
		})
	}
}

func TestQuotaQueueCancellationAndRestart(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			entered, closed := make(chan struct{}), make(chan struct{})
			abort := make(chan struct{})
			defer close(abort)
			var requests atomic.Int64
			f := newQuotaHTTPFixture(t, "sqlite", pair, func(w http.ResponseWriter, r *http.Request) {
				if requests.Add(1) == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(200)
					w.(http.Flusher).Flush()
					close(entered)
					select {
					case <-r.Context().Done():
						close(closed)
					case <-abort:
					}

					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"remainingQueries":5,"totalQueries":30,"windowSizeSeconds":3600}`)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fact := accountdomain.QuotaConsumption{EventID: "restart", AccountID: f.id, Mode: "fast", Units: 1}
			if _, err := f.service.ConsumeQuota(ctx, fact); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			go func() { defer close(done); f.service.RunQuotaRefresh(ctx) }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("worker did not start HTTP")
			}
			cancel()
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("quota worker did not join on cancellation")
			}
			select {
			case <-closed:
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation did not close HTTP")
			}
			if stats := f.service.syncPool.Snapshot(); stats.Active != 0 || stats.Queued != 0 {
				t.Fatalf("cancelled pool retained work: %+v", stats)
			}
			freshCtx := context.Background()
			if receipt, err := f.repo.ConsumeQuota(freshCtx, fact, time.Now()); err != nil || receipt.State != accountdomain.QuotaConsumptionPendingRefresh {
				t.Fatalf("cancel lost SQL demand: %+v %v", receipt, err)
			}
			release, acquired, err := pair.lock.Acquire(freshCtx, "quota-refresh:"+strconv.FormatUint(f.id, 10)+":fast", time.Second)
			if err != nil || !acquired {
				t.Fatalf("cancelled worker retained shared lock: %v %v", acquired, err)
			}
			release()
			restored := NewService(f.repo, nil, nil, nil, f.service.providers, f.service.cipher, security.RandomTokenSource{}, nil, nil, pair.lock)
			restored.SetQuotaRefreshCoordinator(pair.second)
			restored.recoverDurableQuotaRefreshes(freshCtx, 0)
			(&quotaHTTPFixture{service: restored}).runOne(t, freshCtx)
			if receipt, err := f.repo.ConsumeQuota(freshCtx, fact, time.Now()); err != nil || receipt.State != accountdomain.QuotaConsumptionRefreshed {
				t.Fatalf("restart did not recover SQL: %+v %v", receipt, err)
			}
			if f.calls.Load() != 2 {
				t.Fatalf("restart calls=%d want=2", f.calls.Load())
			}
		})
	}
}

func TestQuotaQueueParkingExpiresWithoutLosingDurableFact(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			for _, resolution := range []string{"temporary", "new_shared", "manual_refresh", "deleted"} {
				t.Run(resolution, func(t *testing.T) {
					f := newQuotaHTTPFixture(t, "sqlite", pair, nil)
					ctx := context.Background()
					durable := resolution != "temporary"
					fact := accountdomain.QuotaConsumption{EventID: resolution, AccountID: f.id, Mode: "fast", Units: 1}
					if durable {
						if _, err := f.repo.ConsumeQuota(ctx, fact, f.service.now()); err != nil {
							t.Fatal(err)
						}
						f.service.recoverDurableQuotaRefreshes(ctx, 0)
					} else {
						f.service.QueueQuotaRefresh(f.id, "fast")
					}
					for range quotaRefreshFailureBudget {
						f.runOne(t, ctx)
						f.retry()
					}
					key := strconv.FormatUint(f.id, 10) + ":fast"
					state := f.service.quotaRefresh.obs[key]
					if state == nil {
						t.Fatal("missing parked demand")
					}
					f.clock.Store(state.sharedVersion.ExpiresAt.Add(time.Second).UnixNano())
					f.service.recoverSharedQuotaRefreshes(ctx, f.service.now())
					f.service.requeueQuotaRefreshes()
					if !durable {
						if f.service.quotaRefresh.obs[key] != nil {
							t.Fatal("expired temporary parking retained forever")
						}
						return
					}
					if state = f.service.quotaRefresh.obs[key]; state == nil || state.failures != quotaRefreshFailureBudget || state.sharedVersion.Generation != 0 {
						t.Fatalf("expiry reset SQL failure budget: %+v", state)
					}
					f.service.recoverDurableQuotaRefreshes(ctx, 0)
					if len(f.service.quotaRefresh.queue) != 0 {
						t.Fatal("SQL scan restarted expired parked demand")
					}
					f.status.Store(200)
					switch resolution {
					case "new_shared":
						// Advance the coordinator's expiry past the deterministic service clock.
						// The old shared incarnation was retired by the scan above.
						version, err := pair.second.MarkQuotaRefreshDirty(ctx, f.id, "fast", 48*time.Hour)
						if err != nil {
							t.Fatal(err)
						}
						if version.Generation != 1 {
							t.Fatalf("shared counter did not restart: %+v", version)
						}
						f.service.recoverSharedQuotaRefreshes(ctx, f.service.now())
						f.runOne(t, ctx)
					case "manual_refresh":
						if _, err := f.service.RefreshQuotaMode(ctx, f.id, "fast"); err != nil {
							t.Fatal(err)
						}
						f.service.recoverDurableQuotaRefreshes(ctx, 0)
					case "deleted":
						if err := f.repo.Delete(ctx, f.id); err != nil {
							t.Fatal(err)
						}
						cursor := f.service.recoverDurableQuotaRefreshes(ctx, 0)
						f.service.recoverDurableQuotaRefreshes(ctx, cursor)
						f.service.recoverDurableQuotaRefreshes(ctx, 0)
					}
					if f.service.quotaRefresh.obs[key] != nil {
						t.Fatalf("resolved SQL demand retained parking: %+v", f.service.quotaRefresh.obs[key])
					}
					receipt, err := f.repo.ConsumeQuota(ctx, fact, f.service.now())
					want := accountdomain.QuotaConsumptionRefreshed
					if resolution == "deleted" {
						want = accountdomain.QuotaConsumptionAccountDeleted
					}
					if err != nil || receipt.State != want {
						t.Fatalf("SQL resolution=%+v want=%s err=%v", receipt, want, err)
					}
				})
			}
		})
	}
}

type quotaPublishRace struct {
	repository.QuotaRefreshCoordinator
	other     repository.QuotaRefreshCoordinator
	after     func()
	published bool
}

func (c *quotaPublishRace) MarkQuotaRefreshDirty(ctx context.Context, id uint64, mode string, ttl time.Duration) (repository.QuotaRefreshVersion, error) {
	version, err := c.QuotaRefreshCoordinator.MarkQuotaRefreshDirty(ctx, id, mode, ttl)
	if err == nil && !c.published {
		c.published = true
		if _, err = c.other.MarkQuotaRefreshDirty(ctx, id, mode, ttl); err != nil {
			return version, err
		}
		c.after()
	}
	return version, err
}

func TestQuotaQueuePublicationReplyDoesNotReplaceNewerObservation(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newQuotaHTTPFixture(t, "sqlite", pair, nil)
			race := &quotaPublishRace{QuotaRefreshCoordinator: pair.first, other: pair.second}
			race.after = func() { f.service.recoverSharedQuotaRefreshes(ctx, f.service.now()) }
			f.service.SetQuotaRefreshCoordinator(race)
			f.service.QueueQuotaRefresh(f.id, "fast")
			f.runOne(t, ctx)
			state := f.service.quotaRefresh.obs[strconv.FormatUint(f.id, 10)+":fast"]
			if state.failures != 0 || state.sharedVersion.Generation != 2 || state.runningVersion.Generation != 1 {
				t.Fatalf("old publication charged/regressed new demand: %+v", state)
			}
			f.status.Store(200)
			f.service.requeueQuotaRefreshes()
			f.runOne(t, ctx)
			version, dirty, err := pair.second.GetQuotaRefreshState(ctx, f.id, "fast")
			if err != nil || dirty || version.Generation != 2 {
				t.Fatalf("duplicate publication or incomplete new demand: %+v dirty=%v err=%v", version, dirty, err)
			}
			if f.calls.Load() != 2 {
				t.Fatalf("actual HTTP calls=%d", f.calls.Load())
			}
		})
	}
}

type quotaPanickingAdapter struct{ provider.Adapter }

func (quotaPanickingAdapter) SyncQuotaMode(context.Context, accountdomain.Credential, string) (accountdomain.QuotaWindow, error) {
	panic("quota provider failed")
}

func TestQuotaQueuePanicReleasesSharedLock(t *testing.T) {
	for name, pair := range quotaTestStores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newQuotaHTTPFixture(t, "sqlite", pair, nil)
			original := f.service.providers
			adapter, _ := original.Get(accountdomain.ProviderWeb)
			f.service.providers = providerimpl.NewRegistry(quotaPanickingAdapter{adapter})
			f.service.QueueQuotaRefresh(f.id, "fast")
			f.runOne(t, ctx)
			state := f.service.quotaRefresh.obs[strconv.FormatUint(f.id, 10)+":fast"]
			if state == nil || state.running || state.failures != 1 {
				t.Fatalf("panic lost retry state: %+v", state)
			}
			release, acquired, err := pair.lock.Acquire(ctx, "quota-refresh:"+strconv.FormatUint(f.id, 10)+":fast", time.Second)
			if err != nil || !acquired {
				t.Fatalf("panic retained lock: %v %v", acquired, err)
			}
			release()
			f.service.providers = original
			f.status.Store(200)
			f.retry()
			f.runOne(t, ctx)
			if f.calls.Load() != 1 || len(f.service.quotaRefresh.obs) != 0 {
				t.Fatalf("panic recovery: calls=%d states=%d", f.calls.Load(), len(f.service.quotaRefresh.obs))
			}
		})
	}
}

type quotaScanFault struct {
	repository.AccountRepository
	fail bool
}

func (r *quotaScanFault) ListPendingQuotaRefreshes(ctx context.Context, afterID uint64, limit int) ([]accountdomain.PendingQuotaRefresh, error) {
	if r.fail {
		return nil, errors.New("pending scan failed")
	}
	return r.AccountRepository.ListPendingQuotaRefreshes(ctx, afterID, limit)
}

func TestQuotaQueueSQLScanFailureCannotRetireParking(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			pair := quotaTestStores(t)["memory"]
			f := newQuotaHTTPFixture(t, dialect, pair, nil)
			ctx := context.Background()
			fact := accountdomain.QuotaConsumption{EventID: "scan-fault", AccountID: f.id, Mode: "fast", Units: 1}
			if _, err := f.repo.ConsumeQuota(ctx, fact, f.service.now()); err != nil {
				t.Fatal(err)
			}
			f.service.recoverDurableQuotaRefreshes(ctx, 0)
			for range quotaRefreshFailureBudget {
				f.runOne(t, ctx)
				f.retry()
			}
			key := strconv.FormatUint(f.id, 10) + ":fast"
			state := f.service.quotaRefresh.obs[key]
			f.clock.Store(state.sharedVersion.ExpiresAt.Add(time.Second).UnixNano())
			f.service.recoverSharedQuotaRefreshes(ctx, f.service.now())
			f.service.requeueQuotaRefreshes()
			fault := &quotaScanFault{AccountRepository: f.repo, fail: true}
			f.service.accounts = fault
			f.service.recoverDurableQuotaRefreshes(ctx, 0)
			if f.service.quotaRefresh.obs[key] == nil {
				t.Fatal("failed SQL pass retired demand")
			}
			fault.fail = false
			cursor := f.service.recoverDurableQuotaRefreshes(ctx, 0)
			f.service.recoverDurableQuotaRefreshes(ctx, cursor)
			if len(f.service.quotaRefresh.queue) != 0 || !state.durable || state.failures != quotaRefreshFailureBudget {
				t.Fatalf("recovered SQL scan reset budget: %+v", state)
			}
		})
	}
}
