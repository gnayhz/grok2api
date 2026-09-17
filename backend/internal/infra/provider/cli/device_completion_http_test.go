package cli

import (
	"context"
	"encoding/json"
	"errors"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

type deviceHTTPCompletionBody struct {
	io.ReadCloser
	after func()
}

func (b deviceHTTPCompletionBody) Close() error { err := b.ReadCloser.Close(); b.after(); return err }

type deviceHTTPFailedStore struct {
	repository.DeviceSessionRepository
}

func (s deviceHTTPFailedStore) FinishPoll(context.Context, account.DevicePollReceipt, account.DevicePollCompletion) (bool, error) {
	return false, errors.New("runtime unavailable")
}

func TestDeviceCompletionFormalHTTPAndOAuth(t *testing.T) {
	for _, tc := range []struct {
		name, body                         string
		upstream, status                   int
		cancel, storeFail, saved, terminal bool
	}{
		{name: "pending", body: `{"error":"authorization_pending"}`, upstream: 400, status: 202},
		{name: "slow_down", body: `{"error":"slow_down"}`, upstream: 400, status: 429},
		{name: "denied", body: `{"error":"access_denied"}`, upstream: 400, status: 410, terminal: true},
		{name: "expired", body: `{"error":"expired_token"}`, upstream: 400, status: 410, terminal: true},
		{name: "provider_failure", body: `{"error":"temporarily_unavailable"}`, upstream: 503, status: 502},
		{name: "invalid_success", body: `{broken`, upstream: 200, status: 502},
		{name: "known_success", body: `{"access_token":"local-access","refresh_token":"local-refresh","expires_in":3600}`, upstream: 200, status: 200, saved: true, terminal: true},
		{name: "known_success_after_cancel", body: `{"access_token":"local-access","refresh_token":"local-refresh","expires_in":3600}`, upstream: 200, status: 200, cancel: true, saved: true, terminal: true},
		{name: "pending_store_failure", body: `{"error":"authorization_pending"}`, upstream: 400, status: 502, storeFail: true},
		{name: "success_store_failure", body: `{"access_token":"local-access","refresh_token":"local-refresh","expires_in":3600}`, upstream: 200, status: 502, storeFail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			db, err := relational.OpenSQLite(context.Background(), filepath.Join(t.TempDir(), "device-http.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if err := db.InitializeSchema(context.Background()); err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAccountRepository(db)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			store := memory.NewDeviceSessionStore()
			if err := store.Create(ctx, account.DeviceSession{ID: "session", DeviceCode: "local-device", Interval: time.Second, NextPollAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
				t.Fatal(err)
			}
			var polls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/oauth2/token" || r.Method != "POST" {
					http.Error(w, "unexpected", 400)
					return
				}
				if err := r.ParseForm(); err != nil {
					t.Error(err)
					http.Error(w, "invalid form", 400)
					return
				}
				if r.Form.Get("device_code") != "local-device" || r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || r.Form.Get("client_id") != defaultOAuthClientID || r.Header.Get("x-grok-client-surface") != deviceClientSurface {
					t.Error("device wire contract changed")
				}
				polls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.upstream)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer upstream.Close()
			adapter := NewAdapter(Config{BaseURL: upstream.URL, ClientVersion: "device-test"}, cipher)
			adapter.oauth.tokenURL = upstream.URL + "/oauth2/token"
			adapter.oauth.http = upstream.Client()
			if tc.cancel {
				transport := adapter.oauth.http.Transport
				adapter.oauth.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					resp, err := transport.RoundTrip(r)
					if err == nil {
						resp.Body = deviceHTTPCompletionBody{ReadCloser: resp.Body, after: cancel}
					}
					return resp, err
				})}
			}
			var sessions repository.DeviceSessionRepository = store
			if tc.storeFail {
				sessions = deviceHTTPFailedStore{DeviceSessionRepository: store}
			}
			service := accountapp.NewService(repo, relational.NewAuditRepository(db), sessions, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
			router := gin.New()
			syncService := accountsyncapp.NewService(slog.Default(), service, service, service, service, service, service, service, modelapp.NewService(relational.NewModelRepository(db), repo, service, providerimpl.NewRegistry(adapter)))
			accounthttp.NewHandler(accounthttp.Dependencies{Administration: service, Credentials: service, Maintenance: service, Onboarding: accountsyncapp.NewOnboarding(service, service, syncService), DeviceOnboarding: syncService}).Register(router.Group("/api/admin/v1"))
			request := httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/device/session/poll", nil).WithContext(ctx)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != tc.status || polls.Load() != 1 {
				t.Fatalf("HTTP completion status=%d polls=%d body=%s", recorder.Code, polls.Load(), recorder.Body.String())
			}
			values, err := repo.ListEnabled(context.Background(), account.ProviderBuild)
			if err != nil {
				t.Fatal(err)
			}
			if tc.saved {
				if len(values) != 1 {
					t.Fatalf("known OAuth grant missing: %d", len(values))
				}
				refresh, err := cipher.Decrypt(values[0].EncryptedRefreshToken)
				if err != nil || refresh != "local-refresh" {
					t.Fatalf("refresh persistence: %v", err)
				}
			} else if len(values) != 0 {
				t.Fatal("failed OAuth poll saved an account")
			}
			stored, err := store.Get(context.Background(), "session", time.Now())
			if tc.terminal {
				if !errors.Is(err, repository.ErrNotFound) {
					t.Fatalf("terminal session retained: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if tc.name == "slow_down" && stored.Interval != 6*time.Second {
					t.Fatal("slow down interval not preserved")
				}
				if !tc.storeFail && stored.PollToken != "" {
					t.Fatal("nonterminal poll still owned")
				}
			}
		})
	}
}

func TestDeviceStartFormalHTTPPreservesPollingContract(t *testing.T) {
	var starts, polls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth2/device/code" {
			starts.Add(1)
			_, _ = io.WriteString(w, `{"device_code":"started","user_code":"ABCD","verification_uri":"https://example.test/activate","interval":5,"expires_in":60}`)
			return
		}
		polls.Add(1)
		http.Error(w, "unexpected poll", 500)
	}))
	defer upstream.Close()
	adapter := NewAdapter(Config{BaseURL: upstream.URL}, nil)
	adapter.oauth.http = upstream.Client()
	adapter.oauth.deviceURL = upstream.URL + "/oauth2/device/code"
	adapter.oauth.tokenURL = upstream.URL + "/oauth2/token"
	db := controlDocumentDatabase(t, "sqlite")
	repo := relational.NewAccountRepository(db)
	service := accountapp.NewService(repo, relational.NewAuditRepository(db), memory.NewDeviceSessionStore(), nil, providerimpl.NewRegistry(adapter), nil, security.RandomTokenSource{}, nil, nil, nil)
	router := gin.New()
	syncService := accountsyncapp.NewService(slog.Default(), service, service, service, service, service, service, service, modelapp.NewService(relational.NewModelRepository(db), repo, service, providerimpl.NewRegistry(adapter)))
	accounthttp.NewHandler(accounthttp.Dependencies{Administration: service, Credentials: service, Maintenance: service, Onboarding: accountsyncapp.NewOnboarding(service, service, syncService), DeviceOnboarding: syncService}).Register(router.Group("/api/admin/v1"))
	out := httptest.NewRecorder()
	router.ServeHTTP(out, httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/device/start", nil))
	var payload struct {
		Data struct {
			SessionID string `json:"sessionId"`
			Interval  int    `json:"intervalSeconds"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if out.Code != 201 || payload.Data.SessionID == "" || payload.Data.Interval != 5 {
		t.Fatalf("start result %d %s", out.Code, out.Body.String())
	}
	poll := httptest.NewRecorder()
	router.ServeHTTP(poll, httptest.NewRequest(http.MethodPost, "/api/admin/v1/accounts/device/"+payload.Data.SessionID+"/poll", nil))
	if poll.Code != 429 || starts.Load() != 1 || polls.Load() != 0 {
		t.Fatalf("early poll %d starts=%d polls=%d", poll.Code, starts.Load(), polls.Load())
	}
}
