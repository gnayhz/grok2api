package inference

import (
	"context"
	"errors"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type httpResourceFaultStore struct {
	repository.ResponseRepository
	failRead, failDelete, failWebRead, failWebDelete atomic.Bool
	cause                                            error
}

func (f *httpResourceFaultStore) Get(ctx context.Context, id string, key uint64, now time.Time) (inferencedomain.ResponseOwnership, error) {
	if f.failRead.Load() {
		return inferencedomain.ResponseOwnership{}, f.cause
	}
	return f.ResponseRepository.Get(ctx, id, key, now)
}
func (f *httpResourceFaultStore) Delete(ctx context.Context, id string, key uint64) error {
	if f.failDelete.Load() {
		return f.cause
	}
	return f.ResponseRepository.Delete(ctx, id, key)
}
func (f *httpResourceFaultStore) GetWebState(ctx context.Context, id string, now time.Time) (inferencedomain.WebResponseState, error) {
	if f.failWebRead.Load() {
		return inferencedomain.WebResponseState{}, f.cause
	}
	return f.ResponseRepository.GetWebState(ctx, id, now)
}
func (f *httpResourceFaultStore) DeleteWebState(ctx context.Context, id string) error {
	if f.failWebDelete.Load() {
		return f.cause
	}
	return f.ResponseRepository.DeleteWebState(ctx, id)
}

// Actual HTTP/auth/SQL ownership, real Build HTTP/M13 and Web compatibility
// state, rebuilt Gateway instances, and store-boundary faults. No production I/O.
func TestHTTPResponseResourceStoreFailuresAndRecovery(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderWeb} {
			t.Run(dialect+"/"+string(kind), func(t *testing.T) {
				ctx := context.Background()
				db := compactionDatabase(t, dialect)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					t.Fatal(err)
				}
				accounts, models, audits, keys := relational.NewAccountRepository(db), relational.NewModelRepository(db), relational.NewAuditRepository(db), relational.NewClientKeyRepository(db)
				token, err := cipher.Encrypt("synthetic-token")
				if err != nil {
					t.Fatal(err)
				}
				model, authType := "grok-4.6", account.AuthTypeOAuth
				if kind == account.ProviderWeb {
					model, authType = "grok-chat-fast", account.AuthTypeSSO
				}
				c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, AuthType: authType, Name: "resource", SourceKey: "resource", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 4, WebTier: account.WebTierBasic})
				if err != nil {
					t.Fatal(err)
				}
				if err = testsupport.Discover(ctx, models, kind, []string{model}); err != nil {
					t.Fatal(err)
				}
				if err = testsupport.Capabilities(ctx, models, accounts, c.ID, []string{model}, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
				route, err := models.GetByProviderUpstream(ctx, kind, model)
				if err != nil {
					t.Fatal(err)
				}
				sticky, capacity := memory.NewStickyStore(), memory.NewConcurrencyLimiter()
				clients := clientkeyapp.NewService("resource-owner", keys, memory.NewRateLimiter(), capacity, 1000, 8, cipher, security.RandomTokenSource{})
				t.Cleanup(func() { closeClientKeyService(t, clients) })
				owner, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "owner", Enabled: true, RPMLimit: 1000, MaxConcurrent: 8})
				if err != nil {
					t.Fatal(err)
				}
				other, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "other", Enabled: true, RPMLimit: 1000, MaxConcurrent: 8})
				if err != nil {
					t.Fatal(err)
				}
				store := &httpResourceFaultStore{ResponseRepository: relational.NewResponseRepository(db), cause: errors.New("private database endpoint and query text")}
				network := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
				t.Cleanup(func() { _ = network.Close(context.Background()) })
				var calls atomic.Int32
				var recoverCredential atomic.Bool
				var rejectNext atomic.Bool
				var status atomic.Int32
				status.Store(200)
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					expectedAuthorization := "Bearer synthetic-token"
					reject := recoverCredential.Load() && rejectNext.Swap(false)
					if recoverCredential.Load() && !reject {
						expectedAuthorization = "Bearer synthetic-rotated"
					}
					if r.Header.Get("Authorization") != expectedAuthorization {
						t.Error("stored response used wrong account secret")
					}
					w.Header().Set("Content-Type", "application/json")
					code := int(status.Load())
					if reject {
						code = 401
					}
					if code >= 400 {
						w.Header().Set("X-Private-Upstream", "secret")
						w.WriteHeader(code)
						_, _ = io.WriteString(w, `{"error":"private upstream message"}`)
						return
					}
					if r.Method == http.MethodDelete {
						_, _ = io.WriteString(w, `{"id":"resource","object":"response.deleted","deleted":true}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"resource","status":"completed","output":[],"model":"grok-4.6"}`)
				}))
				t.Cleanup(upstream.Close)
				var adapter provider.Adapter
				if kind == account.ProviderBuild {
					build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
					build.SetEgress(network)
					adapter = build
				} else {
					adapter = webprovider.NewAdapter(webprovider.Config{BaseURL: upstream.URL}, network, cipher, historyapp.NewResponseResources(store), nil)
				}
				registry := providerimpl.NewRegistry(adapter)
				maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
				router := func() *gin.Engine {
					selector := selector.NewSelector(accounts, capacity, sticky, registry, time.Hour, time.Second, time.Minute)
					service := gateway.NewService(models, audits, maintenance, clients, registry, selector, historyapp.NewResponseResources(store), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
					r := gin.New()
					r.Use(middleware.RequestID(nil), middleware.ClientAuth(clients))
					NewHandler(service, nil, 1<<20).Register(r.Group("/v1"))
					return r
				}
				var active atomic.Pointer[gin.Engine]
				active.Store(router())
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { active.Load().ServeHTTP(w, r) }))
				t.Cleanup(server.Close)
				seed := func() {
					t.Helper()
					now := time.Now().UTC()
					if err := store.Save(ctx, inferencedomain.ResponseOwnership{ResponseID: "resource", AccountID: c.ID, ClientKeyID: owner.Key.ID, ModelRouteID: route.ID, Provider: kind, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
						t.Fatal(err)
					}
					if kind == account.ProviderWeb {
						if err := store.SaveWebState(ctx, inferencedomain.WebResponseState{ResponseID: "resource", AccountID: c.ID, ConversationID: "conversation", UpstreamParentResponseID: "parent", ResponseJSON: `{"id":"resource","status":"completed","output":[],"model":"grok-chat-fast"}`, Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
							t.Fatal(err)
						}
					}
				}
				state := func(wantOwner, wantNative bool) {
					t.Helper()
					_, e := store.ResponseRepository.Get(ctx, "resource", owner.Key.ID, time.Now().UTC())
					if wantOwner && e != nil || !wantOwner && !errors.Is(e, repository.ErrNotFound) {
						t.Fatalf("ownership remains=%t err=%v", wantOwner, e)
					}
					if kind == account.ProviderWeb {
						_, e = store.ResponseRepository.GetWebState(ctx, "resource", time.Now().UTC())
						if wantNative && e != nil || !wantNative && !errors.Is(e, repository.ErrNotFound) {
							t.Fatalf("native remains=%t err=%v", wantNative, e)
						}
					}
				}
				send := func(method, secret string, want int, code string) {
					t.Helper()
					path, body := "/v1/responses/resource", ""
					if method == http.MethodPost {
						path = "/v1/responses"
						body = fmt.Sprintf(`{"model":%q,"input":"next","previous_response_id":"resource"}`, model)
					}
					req, err := http.NewRequestWithContext(ctx, method, server.URL+path, strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+secret)
					req.Header.Set("Content-Type", "application/json")
					res, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					data, err := io.ReadAll(res.Body)
					_ = res.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if res.StatusCode != want || code != "" && !strings.Contains(string(data), code) {
						t.Fatalf("%s status=%d body=%s", method, res.StatusCode, data)
					}
					if strings.Contains(string(data), "private") || res.Header.Get("X-Private-Upstream") != "" {
						t.Fatalf("private cause leaked: %s", data)
					}
				}
				seed()
				send(http.MethodGet, other.Secret, 404, "response_not_found")
				send(http.MethodDelete, other.Secret, 404, "response_not_found")
				if calls.Load() != 0 {
					t.Fatal("foreign key touched upstream")
				}
				state(true, true)
				store.failRead.Store(true)
				for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodPost} {
					send(method, owner.Secret, 503, "response_state_unavailable")
					state(true, true)
				}
				if calls.Load() != 0 {
					t.Fatal("lookup failure sent upstream")
				}
				store.failRead.Store(false)
				send(http.MethodGet, owner.Secret, 200, "")
				if kind == account.ProviderWeb {
					store.failWebRead.Store(true)
					send(http.MethodGet, owner.Secret, 503, "response_state_unavailable")
					state(true, true)
					store.failWebRead.Store(false)
					store.failWebDelete.Store(true)
					send(http.MethodDelete, owner.Secret, 503, "response_state_unavailable")
					state(true, true)
					store.failWebDelete.Store(false)
				}
				store.failDelete.Store(true)
				send(http.MethodDelete, owner.Secret, 503, "response_state_unavailable")
				state(true, false)
				// A fresh instance can use the retained identity to complete local deletion
				// when the provider now confirms it is gone.
				active.Store(router())
				store.failDelete.Store(false)
				status.Store(404)
				send(http.MethodDelete, owner.Secret, 404, "response_not_found")
				state(false, false)
				// Ordinary successful deletion and GET of an upstream missing resource both
				// remove exactly the local identity that was actually authenticated.
				seed()
				status.Store(200)
				send(http.MethodDelete, owner.Secret, 200, "")
				state(false, false)
				seed()
				if kind == account.ProviderWeb {
					if err := store.ResponseRepository.DeleteWebState(ctx, "resource"); err != nil {
						t.Fatal(err)
					}
				} else {
					status.Store(410)
				}
				send(http.MethodGet, owner.Secret, 404, "response_not_found")
				state(false, false)
				if kind == account.ProviderWeb && calls.Load() != 0 {
					t.Fatal("local Web resource caused network request")
				}
				if kind == account.ProviderBuild {
					var oauth atomic.Int32
					proxy := imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, r *http.Request) {
						if r.Host != "auth.x.ai" || r.URL.Path != "/oauth2/token" {
							t.Error("unexpected authentication endpoint")
							w.WriteHeader(500)
							return
						}
						oauth.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"access_token":"synthetic-rotated","refresh_token":"synthetic-refresh","expires_in":3600}`)
					})
					encryptedProxy, err := cipher.Encrypt(proxy)
					if err != nil {
						t.Fatal(err)
					}
					egressStore := relational.NewEgressRepository(db)
					node, err := egressStore.CreateEgressNode(ctx, egressdomain.Node{Name: "resource-auth", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy})
					if err != nil {
						t.Fatal(err)
					}
					if _, err = egressStore.SaveEgressOperationsConfig(ctx, egressdomain.OperationsConfig{DefaultTarget: egressdomain.RoutingTarget{Mode: egressdomain.RoutingTargetNode, NodeID: node.ID}}, func(egressdomain.Node) error {
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					network.InvalidateNodeSnapshots()
					network.InvalidateOperationsConfig()
					refreshToken, err := cipher.Encrypt("synthetic-refresh")
					if err != nil {
						t.Fatal(err)
					}
					recoverCredential.Store(true)
					status.Store(200)
					for _, method := range []string{http.MethodGet, http.MethodDelete} {
						c.EncryptedAccessToken, c.EncryptedRefreshToken = token, refreshToken
						if _, _, err = accounts.UpsertByIdentity(ctx, c); err != nil {
							t.Fatal(err)
						}
						seed()
						rejectNext.Store(true)
						before, authBefore := calls.Load(), oauth.Load()
						active.Store(router())
						send(method, owner.Secret, 200, "")
						if calls.Load()-before != 2 || oauth.Load()-authBefore != 1 {
							t.Fatalf("credential recovery physical=%d oauth=%d", calls.Load()-before, oauth.Load()-authBefore)
						}
						fresh, err := accounts.Get(ctx, c.ID)
						if err != nil {
							t.Fatal(err)
						}
						plain, err := cipher.Decrypt(fresh.EncryptedAccessToken)
						if err != nil || plain != "synthetic-rotated" {
							t.Fatal("credential refresh was not persisted")
						}
						state(method == http.MethodGet, false)
					}
				}

			})
		}
	}
}
