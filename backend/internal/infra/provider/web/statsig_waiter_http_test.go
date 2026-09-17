package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/repository"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	"github.com/gin-gonic/gin"
)

func TestStatsigSharedRefreshHTTPHonorsRequestLifetime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			for _, canceledRole := range []string{"waiter_deadline", "owner_cancel"} {
				t.Run(canceledRole, func(t *testing.T) {
					ctx := context.Background()
					db := statsigWaiterDatabase(t, driver)
					repo := relational.NewAccountRepository(db)
					cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
					if err != nil {
						t.Fatal(err)
					}
					accounts := make([]account.Credential, 2)
					for i := range accounts {
						token, err := cipher.Encrypt(fmt.Sprintf("synthetic-statsig-http-%d", i))
						if err != nil {
							t.Fatal(err)
						}
						accounts[i], _, err = repo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, AuthStatus: account.AuthStatusActive, Enabled: true, Name: fmt.Sprint(i), SourceKey: fmt.Sprint(i), WebTier: account.WebTierBasic, UserID: fmt.Sprintf("497f19f8-49d4-458a-bee4-43ec3dcaf8c%d", i), Email: fmt.Sprintf("synthetic%d@example.test", i), EncryptedAccessToken: token})
						if err != nil {
							t.Fatal(err)
						}
						rev, err := repo.GetQuotaRevision(ctx, accounts[i].ID)
						if err != nil {
							t.Fatal(err)
						}
						now := time.Now().UTC()
						reset := now.Add(time.Hour)
						windows := []account.QuotaWindow{{AccountID: accounts[i].ID, Mode: "auto", Remaining: 0, Total: 7, ResetAt: &reset, Source: account.QuotaSourceUpstream}, {AccountID: accounts[i].ID, Mode: "fast", Remaining: 0, Total: 30, ResetAt: &reset, Source: account.QuotaSourceUpstream}}
						if err := repo.SaveQuotaSnapshot(ctx, repository.QuotaSnapshotWrite{AccountID: accounts[i].ID, Revision: rev, Tier: account.WebTierBasic, SyncedAt: now, Windows: windows, ReplaceAll: true}); err != nil {
							t.Fatal(err)
						}
					}
					if accounts[0].ID == accounts[1].ID {
						t.Fatal("fixture must use different account operations")
					}
					canceledIndex := 1
					if canceledRole == "owner_cancel" {
						canceledIndex = 0
					}
					before, err := repo.GetQuotaWindows(ctx, []uint64{accounts[canceledIndex].ID})
					if err != nil {
						t.Fatal(err)
					}
					revision, err := repo.GetQuotaRevision(ctx, accounts[canceledIndex].ID)
					if err != nil {
						t.Fatal(err)
					}
					var quotaCalls, signCalls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == "/index" {
							_, _ = io.WriteString(w, `<meta name="grok-site-verification" content="synthetic-meta">`)
							return
						}
						quotaCalls.Add(1)
						if !validStatsigID(r.Header.Get("x-statsig-id")) {
							t.Error("successful full quota request lacks signature")
						}
						switch r.URL.Path {
						case "/rest/rate-limits":
							var input struct {
								ModelName string `json:"modelName"`
							}
							if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
								t.Error(err)
								return
							}
							_ = json.NewEncoder(w).Encode(map[string]int{"totalQueries": map[string]int{"auto": 7, "fast": 30}[input.ModelName], "remainingQueries": 3, "windowSizeSeconds": 7200})
						case "/rest/media/imagine/quota_info":
							_, _ = io.Copy(io.Discard, r.Body)
							_, _ = io.WriteString(w, `{"image":null,"imagePro":null,"imageEdit":null,"video":null,"video720p":null}`)
						default:
							t.Errorf("unexpected endpoint %s", r.URL.Path)
							http.NotFound(w, r)
						}
					}))
					t.Cleanup(upstream.Close)
					entered, release := make(chan struct{}), make(chan struct{})
					var once sync.Once
					finish := func() { once.Do(func() { close(release) }) }
					signerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						_, _ = io.Copy(io.Discard, r.Body)
						if signCalls.Add(1) == 1 {
							close(entered)
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
						}
						_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": base64.RawStdEncoding.EncodeToString(make([]byte, 70))})
					}))
					t.Cleanup(signerServer.Close)
					manager := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
					t.Cleanup(func() { _ = manager.Close(ctx) })
					t.Cleanup(finish)
					adapter := NewAdapter(Config{BaseURL: upstream.URL, StatsigMode: "url", StatsigSignerURL: signerServer.URL, QuotaTimeout: 4 * time.Second}, manager, cipher, nil, nil)
					adapter.statsig.client = signerServer.Client()
					adapter.statsig.validateEndpoint = func(context.Context, string) error { return nil }
					service := accountapp.NewService(repo, relational.NewAuditRepository(db), nil, nil, providerimpl.NewRegistry(adapter), cipher, security.RandomTokenSource{}, nil, nil, nil)
					router := gin.New()
					accounthttp.NewHandler(accounthttp.Dependencies{Administration: service, Credentials: service, Maintenance: service, Onboarding: accountsyncapp.NewOnboarding(service, service, nil)}).Register(router.Group("/api/admin/v1"))
					call := func(callCtx context.Context, index int) <-chan *httptest.ResponseRecorder {
						done := make(chan *httptest.ResponseRecorder, 1)
						go func() {
							response := httptest.NewRecorder()
							request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/v1/accounts/%d/refresh-quota", accounts[index].ID), nil).WithContext(callCtx)
							router.ServeHTTP(response, request)
							done <- response
						}()
						return done
					}
					receive := func(done <-chan *httptest.ResponseRecorder) *httptest.ResponseRecorder {
						select {
						case response := <-done:
							return response
						case <-time.After(2 * time.Second):
							t.Fatal("formal HTTP failed to drain")
							return nil
						}
					}
					ownerCtx, cancelOwner := context.WithCancel(ctx)
					defer cancelOwner()
					owner := call(ownerCtx, 0)
					waitStatsigSignal(t, entered)
					waiterCtx := ctx
					cancelWaiter := func() {}
					if canceledRole == "waiter_deadline" {
						waiterCtx, cancelWaiter = context.WithTimeout(ctx, 200*time.Millisecond)
					}
					defer cancelWaiter()
					waiter := call(waiterCtx, 1)
					if canceledRole == "owner_cancel" {
						cancelOwner()
					}
					var canceled, survived *httptest.ResponseRecorder
					if canceledRole == "waiter_deadline" {
						timely := true
						select {
						case canceled = <-waiter:
						case <-time.After(800 * time.Millisecond):
							timely = false
							finish()
							canceled = receive(waiter)
							t.Errorf("canceled waiter stayed blocked on other account's signer")
						}
						if timely && quotaCalls.Load() != 0 {
							t.Errorf("canceled waiter issued quota request while owner held signer: %d", quotaCalls.Load())
						}
						finish()
						survived = receive(owner)
					} else {
						canceled = receive(owner)
						survived = receive(waiter)
					}
					if canceled.Code != 502 || survived.Code != 200 {
						t.Fatalf("canceled=%d %s survivor=%d %s", canceled.Code, canceled.Body.String(), survived.Code, survived.Body.String())
					}
					after, err := repo.GetQuotaWindows(ctx, []uint64{accounts[canceledIndex].ID})
					if err != nil {
						t.Fatal(err)
					}
					afterRevision, err := repo.GetQuotaRevision(ctx, accounts[canceledIndex].ID)
					if err != nil {
						t.Fatal(err)
					}
					if !reflect.DeepEqual(before, after) || revision != afterRevision {
						t.Fatalf("canceled full quota mutated SQL: before=%+v after=%+v revisions=%+v/%+v", before, after, revision, afterRevision)
					}
					if quotaCalls.Load() != 3 {
						t.Fatalf("survivor quota calls=%d", quotaCalls.Load())
					}
					retry := receive(call(ctx, canceledIndex))
					if retry.Code != 200 {
						t.Fatalf("followup=%d %s", retry.Code, retry.Body.String())
					}
					final, err := repo.GetQuotaWindows(ctx, []uint64{accounts[canceledIndex].ID})
					if err != nil {
						t.Fatal(err)
					}
					if len(final[accounts[canceledIndex].ID]) != 2 {
						t.Fatalf("followup windows=%+v", final)
					}
					for _, window := range final[accounts[canceledIndex].ID] {
						if window.Remaining != 3 {
							t.Fatalf("followup=%+v", window)
						}
					}
					expectedSigns := int32(2)
					if canceledRole == "owner_cancel" {
						expectedSigns = 3
					}
					if quotaCalls.Load() != 6 || signCalls.Load() != expectedSigns {
						t.Fatalf("calls quota=%d signs=%d", quotaCalls.Load(), signCalls.Load())
					}
				})
			}
		})
	}
}
