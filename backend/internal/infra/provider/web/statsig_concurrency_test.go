package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type statsigConcurrencyFixture struct {
	adapter                  *Adapter
	credential               account.Credential
	entered, release, exited chan struct{}
	releaseOnce              sync.Once
	signs, quotas            atomic.Int32
}

func newStatsigConcurrencyFixture(t *testing.T) *statsigConcurrencyFixture {
	t.Helper()
	f := &statsigConcurrencyFixture{entered: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{})}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/index" {
			_, _ = io.WriteString(w, `<meta name="grok-site-verification" content="synthetic-meta">`)
			return
		}
		if r.URL.Path != "/rest/rate-limits" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		f.quotas.Add(1)
		if !validStatsigID(r.Header.Get("x-statsig-id")) {
			t.Error("live request lost valid shared signature")
		}
		_, _ = io.WriteString(w, `{"totalQueries":30,"remainingQueries":4,"windowSizeSeconds":7200}`)
	}))
	t.Cleanup(upstream.Close)
	signerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if f.signs.Add(1) == 1 {
			defer close(f.exited)
			close(f.entered)
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": base64.RawStdEncoding.EncodeToString(make([]byte, 70))})
	}))
	t.Cleanup(signerServer.Close)
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-statsig-credential")
	if err != nil {
		t.Fatal(err)
	}
	manager := infraegress.NewManager(egressRepositoryStub{}, cipher)
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	t.Cleanup(f.finish)
	f.adapter = NewAdapter(Config{BaseURL: upstream.URL, StatsigMode: "url", StatsigSignerURL: signerServer.URL, QuotaTimeout: 3 * time.Second}, manager, cipher, nil, nil)
	f.adapter.statsig.client = signerServer.Client()
	f.adapter.statsig.validateEndpoint = func(context.Context, string) error { return nil }
	f.credential = account.Credential{ID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: token}
	return f
}
func (f *statsigConcurrencyFixture) finish() { f.releaseOnce.Do(func() { close(f.release) }) }
func (f *statsigConcurrencyFixture) quota(ctx context.Context) <-chan error {
	done := make(chan error, 1)
	go func() {
		got, err := f.adapter.SyncQuotaMode(ctx, f.credential, "fast")
		if err == nil && got.Remaining != 4 {
			err = errors.New("incorrect successful quota")
		}
		done <- err
	}()
	return done
}
func waitStatsigError(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("Statsig caller did not drain")
		return nil
	}
}
func waitStatsigSignal(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Statsig network phase not reached")
	}
}

func TestStatsigQuotaCanceledWaiterLeavesOwnerRunning(t *testing.T) {
	f := newStatsigConcurrencyFixture(t)
	owner := f.quota(context.Background())
	waitStatsigSignal(t, f.entered)
	ctx, cancel := context.WithCancel(context.Background())
	waiter := f.quota(ctx)
	cancel()
	if err := waitStatsigError(t, waiter); !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter=%v", err)
	}
	select {
	case <-f.exited:
		t.Fatal("waiter canceled owner HTTP request")
	default:
	}
	f.finish()
	if err := waitStatsigError(t, owner); err != nil {
		t.Fatal(err)
	}
	if f.signs.Load() != 1 || f.quotas.Load() != 1 {
		t.Fatalf("signs=%d quotas=%d", f.signs.Load(), f.quotas.Load())
	}
}

func TestStatsigQuotaLiveCallersRecoverCanceledOwner(t *testing.T) {
	f := newStatsigConcurrencyFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	owner := f.quota(ctx)
	waitStatsigSignal(t, f.entered)
	waiters := make([]<-chan error, 8)
	for i := range waiters {
		waiters[i] = f.quota(context.Background())
	}
	cancel()
	if err := waitStatsigError(t, owner); !errors.Is(err, context.Canceled) {
		t.Fatalf("owner=%v", err)
	}
	waitStatsigSignal(t, f.exited)
	for _, waiter := range waiters {
		if err := waitStatsigError(t, waiter); err != nil {
			t.Fatalf("live waiter inherited canceled owner: %v", err)
		}
	}
	if err := waitStatsigError(t, f.quota(context.Background())); err != nil {
		t.Fatal(err)
	}
	if f.signs.Load() != 2 || f.quotas.Load() != 9 {
		t.Fatalf("signs=%d quotas=%d", f.signs.Load(), f.quotas.Load())
	}
}

func TestStatsigQuotaConcurrentSuccessUsesOneSignature(t *testing.T) {
	f := newStatsigConcurrencyFixture(t)
	owner := f.quota(context.Background())
	waitStatsigSignal(t, f.entered)
	waiters := make([]<-chan error, 12)
	for i := range waiters {
		waiters[i] = f.quota(context.Background())
	}
	f.finish()
	if err := waitStatsigError(t, owner); err != nil {
		t.Fatal(err)
	}
	for _, waiter := range waiters {
		if err := waitStatsigError(t, waiter); err != nil {
			t.Fatal(err)
		}
	}
	if err := waitStatsigError(t, f.quota(context.Background())); err != nil {
		t.Fatal(err)
	}
	if f.signs.Load() != 1 || f.quotas.Load() != 14 {
		t.Fatalf("signs=%d quotas=%d", f.signs.Load(), f.quotas.Load())
	}
}

type statsigWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *statsigWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestStatsigInterruptedRefreshReleasesWaitersAndAllowsRetry(t *testing.T) {
	for _, kind := range []string{"panic", "goexit"} {
		t.Run(kind, func(t *testing.T) {
			s := newStatsigSigner()
			entered, release, ownerDone := make(chan struct{}), make(chan struct{}), make(chan struct{})
			s.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) {
				close(entered)
				<-release
				if kind == "panic" {
					panic("synthetic interruption")
				}
				runtime.Goexit()
				return "", nil
			}
			go func() {
				defer close(ownerDone)
				defer func() { _ = recover() }()
				_, _, _ = s.Sign(context.Background(), "https://grok.test", "https://signer.test", "synthetic", nil, "POST", "https://grok.test/rest/test")
			}()
			waitStatsigSignal(t, entered)
			waiterCtx := &statsigWaitContext{Context: context.Background(), entered: make(chan struct{})}
			waiter := make(chan error, 1)
			go func() {
				_, _, err := s.Sign(waiterCtx, "https://grok.test", "https://signer.test", "synthetic", nil, "POST", "https://grok.test/rest/test")
				waiter <- err
			}()
			waitStatsigSignal(t, waiterCtx.entered)
			close(release)
			waitStatsigSignal(t, ownerDone)
			if err := waitStatsigError(t, waiter); err == nil {
				t.Fatal("interrupted refresh reported success")
			}
			retryErr := errors.New("next independent refresh")
			s.fetchMeta = func(context.Context, string, string, *infraegress.Lease) (string, error) { return "", retryErr }
			_, _, err := s.Sign(context.Background(), "https://grok.test", "https://signer.test", "synthetic", nil, "POST", "https://grok.test/rest/test")
			if !errors.Is(err, retryErr) {
				t.Fatalf("refresh slot stuck: %v", err)
			}
		})
	}
}
