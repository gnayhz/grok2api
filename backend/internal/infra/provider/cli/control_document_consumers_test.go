package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	modelhttp "github.com/chenyme/grok2api/backend/internal/transport/http/model"
	"github.com/gin-gonic/gin"
)

type controlDocumentSessions struct {
	repository.DeviceSessionRepository
	created atomic.Int64
}

func (s *controlDocumentSessions) Create(ctx context.Context, value account.DeviceSession) error {
	err := s.DeviceSessionRepository.Create(ctx, value)
	if err == nil {
		s.created.Add(1)
	}
	return err
}

type controlDocumentFixture struct {
	phase     atomic.Int32
	generated atomic.Int32
	calls     atomic.Int64
	accountID uint64
	adapter   *Adapter
	accounts  *relational.AccountRepository
	models    *relational.ModelRepository
	service   *accountapp.Service
	catalog   *modelapp.Service
	sessions  *controlDocumentSessions
	router    *gin.Engine
}

func newControlDocumentFixture(t testing.TB, driver, operation string) *controlDocumentFixture {
	t.Helper()
	ctx := context.Background()
	db := controlDocumentDatabase(t, driver)
	f := &controlDocumentFixture{accounts: relational.NewAccountRepository(db), models: relational.NewModelRepository(db)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		phase := f.phase.Load()
		var body string
		var limit int
		target := false
		switch r.URL.Path {
		case "/v1/models":
			body = `{"data":[{"id":"grok-4.5"}]}`
			w.Header().Set("ETag", "old")
			if phase > 0 {
				body = `{"data":[{"id":"grok-control-after"}]}`
				w.Header().Set("ETag", "new")
			}
			limit, target = 4<<20, operation == "models"
		case "/v1/billing":
			used := 10
			if phase > 0 && operation == "billing" {
				used = 99
			}
			body = fmt.Sprintf(`{"config":{"creditUsagePercent":%d}}`, used)
			limit, target = 2<<20, operation == "billing"
		case "/v1/user":
			body = `{"subscriptionTier":"Free"}`
			if phase > 0 && operation == "subscription" {
				body = `{"subscriptionTier":"SuperGrokPro"}`
			}
			limit, target = 1<<20, operation == "subscription"
		case "/token":
			body = `{"access_token":"fresh-access","refresh_token":"fresh-refresh","expires_in":3600}`
			limit, target = 1<<20, operation == "refresh" || operation == "device_poll"
		case "/device":
			body = `{"device_code":"local-device","user_code":"ABCD","verification_uri":"https://example.test/device","expires_in":1800,"interval":5}`
			limit, target = 1<<20, operation == "device_start"
		default:
			f.generated.Add(1)
			http.Error(w, "unexpected generation", http.StatusBadRequest)
			return
		}
		if phase == 1 && target {
			body += strings.Repeat(" ", limit-len(body)) + "{"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("original-credential")
	if err != nil {
		t.Fatal(err)
	}
	f.adapter = NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
	t.Cleanup(f.adapter.base.current.Load().CloseIdleConnections)
	f.adapter.oauth.tokenURL, f.adapter.oauth.deviceURL = server.URL+"/token", server.URL+"/device"
	registry := provider.NewRegistry(f.adapter)
	f.sessions = &controlDocumentSessions{DeviceSessionRepository: memory.NewDeviceSessionStore()}
	f.service = accountapp.NewService(f.accounts, relational.NewAuditRepository(db), f.sessions, memory.NewStickyStore(), registry, cipher, nil)
	f.catalog = modelapp.NewService(f.models, f.accounts, f.service, registry)
	t.Cleanup(func() { _ = f.catalog.Close(context.Background()) })
	credential, _, err := f.accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "control", SourceKey: "control", Enabled: true, AuthStatus: account.AuthStatusActive, EncryptedAccessToken: encrypted, EncryptedRefreshToken: encrypted, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	f.accountID = credential.ID
	gin.SetMode(gin.TestMode)
	f.router = gin.New()
	accounthttp.NewHandler(f.service, nil).Register(f.router.Group("/api/admin/v1"))
	modelhttp.NewHandler(f.catalog).Register(f.router.Group("/api/admin/v1"))
	return f
}

func (f *controlDocumentFixture) request(method, path string) *httptest.ResponseRecorder {
	output := httptest.NewRecorder()
	f.router.ServeHTTP(output, httptest.NewRequest(method, "/api/admin/v1"+path, nil))
	return output
}

func TestHTTPBuildControlDocumentsDoNotPublishPartialFacts(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"models", "billing", "subscription", "refresh", "device_start", "device_poll"} {
			t.Run(driver+"/"+operation, func(t *testing.T) {
				f := newControlDocumentFixture(t, driver, operation)
				ctx := context.Background()
				before, err := f.accounts.Get(ctx, f.accountID)
				if err != nil {
					t.Fatal(err)
				}
				if operation == "models" {
					if _, err := f.catalog.SyncAccount(ctx, f.accountID); err != nil {
						t.Fatal(err)
					}
					f.phase.Store(1)
					if _, err := f.catalog.SyncAccount(ctx, f.accountID); err == nil {
						t.Fatal("M05 published a truncated catalog")
					}
					if page := f.request(http.MethodGet, "/models"); page.Code != 200 || strings.Contains(page.Body.String(), "grok-control-after") || !strings.Contains(page.Body.String(), "grok-4.5") {
						t.Fatalf("catalog after failure: status=%d", page.Code)
					}
					if !f.adapter.modelCatalogChanged(f.accountID, "new") {
						t.Fatal("truncated catalog installed its ETag")
					}
					f.phase.Store(2)
					if _, err := f.catalog.SyncAccount(ctx, f.accountID); err != nil {
						t.Fatal(err)
					}
					if page := f.request(http.MethodGet, "/models"); page.Code != 200 || !strings.Contains(page.Body.String(), "grok-control-after") || f.adapter.modelCatalogChanged(f.accountID, "new") {
						t.Fatal("valid catalog did not publish")
					}
				} else if operation == "billing" || operation == "subscription" {
					path := fmt.Sprintf("/accounts/%d/refresh-billing", f.accountID)
					if output := f.request(http.MethodPost, path); output.Code != 200 {
						t.Fatalf("initial billing: %d", output.Code)
					}
					initial, err := f.accounts.GetBilling(ctx, f.accountID)
					if err != nil {
						t.Fatal(err)
					}
					f.phase.Store(1)
					output := f.request(http.MethodPost, path)
					stored, err := f.accounts.GetBilling(ctx, f.accountID)
					if err != nil {
						t.Fatal(err)
					}
					if operation == "billing" {
						if output.Code != 502 || !reflect.DeepEqual(initial, stored) {
							t.Fatalf("bad billing changed snapshot: status=%d", output.Code)
						}
					} else if output.Code != 200 || stored.PlanName == "SuperGrokPro" || stored.CreditUsagePercent != 10 {
						t.Fatalf("optional bad subscription became paid fact: status=%d plan=%s", output.Code, stored.PlanName)
					}
					f.phase.Store(2)
					if output := f.request(http.MethodPost, path); output.Code != 200 {
						t.Fatalf("valid billing: %d", output.Code)
					}
					stored, err = f.accounts.GetBilling(ctx, f.accountID)
					if err != nil || operation == "billing" && stored.CreditUsagePercent != 99 || operation == "subscription" && stored.PlanName != "SuperGrokPro" {
						t.Fatalf("valid facts not installed: %v", err)
					}
				} else if operation == "refresh" {
					path := fmt.Sprintf("/accounts/%d/refresh-token", f.accountID)
					f.phase.Store(1)
					output := f.request(http.MethodPost, path)
					stored, err := f.accounts.Get(ctx, f.accountID)
					if err != nil || output.Code != 502 || stored.EncryptedAccessToken != before.EncryptedAccessToken || stored.EncryptedRefreshToken != before.EncryptedRefreshToken || stored.CredentialRef() != before.CredentialRef() || stored.AuthStatus != before.AuthStatus {
						t.Fatalf("bad token changed installed material: status=%d err=%v", output.Code, err)
					}
					f.phase.Store(2)
					if output := f.request(http.MethodPost, path); output.Code != 200 {
						t.Fatalf("valid refresh: %d", output.Code)
					}
					stored, err = f.accounts.Get(ctx, f.accountID)
					if err != nil || stored.EncryptedAccessToken == before.EncryptedAccessToken || stored.CredentialRef() == before.CredentialRef() {
						t.Fatal("valid token was not installed")
					}
				} else if operation == "device_start" {
					f.phase.Store(1)
					if output := f.request(http.MethodPost, "/accounts/device/start"); output.Code != 502 || f.sessions.created.Load() != 0 {
						t.Fatalf("bad device code installed a session: status=%d", output.Code)
					}
					f.phase.Store(2)
					output := f.request(http.MethodPost, "/accounts/device/start")
					var envelope struct {
						Data struct {
							SessionID string `json:"sessionId"`
						} `json:"data"`
					}
					if err := json.Unmarshal(output.Body.Bytes(), &envelope); err != nil || output.Code != 201 || f.sessions.created.Load() != 1 {
						t.Fatalf("valid session: status=%d err=%v", output.Code, err)
					}
					if session, err := f.sessions.Get(ctx, envelope.Data.SessionID, time.Now()); err != nil || session.DeviceCode != "local-device" {
						t.Fatalf("persisted session: %v", err)
					}
				} else {
					if err := f.sessions.Create(ctx, account.DeviceSession{ID: "pending", DeviceCode: "local-device", Interval: time.Nanosecond, NextPollAt: time.Now().Add(-time.Second), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
						t.Fatal(err)
					}
					f.phase.Store(1)
					if output := f.request(http.MethodPost, "/accounts/device/pending/poll"); output.Code != 502 {
						t.Fatalf("bad grant accepted: %d", output.Code)
					}
					if _, err := f.sessions.Get(ctx, "pending", time.Now()); err != nil {
						t.Fatal("bad grant retired its pending authorization")
					}
					if accounts, err := f.accounts.ListEnabled(ctx, account.ProviderBuild); err != nil || len(accounts) != 1 {
						t.Fatal("bad grant installed an account")
					}
					f.phase.Store(2)
					if output := f.request(http.MethodPost, "/accounts/device/pending/poll"); output.Code != 200 {
						t.Fatalf("valid grant: %d", output.Code)
					}
					if accounts, err := f.accounts.ListEnabled(ctx, account.ProviderBuild); err != nil || len(accounts) != 2 {
						t.Fatal("valid grant did not install an account")
					}
				}
				if f.generated.Load() != 0 {
					t.Fatal("control operation generated inference")
				}
			})
		}
	}
}
