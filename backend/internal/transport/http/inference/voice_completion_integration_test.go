package inference

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	upstreamws "github.com/bogdanfinn/websocket"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	clientws "github.com/gorilla/websocket"
)

type voiceReceiptSink struct {
	mu    sync.Mutex
	facts []attemptmeta.PhysicalFact
	fail  bool
}

func (s *voiceReceiptSink) RecordPhysicalEvents(_ context.Context, facts []attemptmeta.PhysicalFact) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts = append(s.facts, facts...)
	if s.fail {
		return errors.New("injected receipt failure")
	}
	return nil
}
func (*voiceReceiptSink) RecordQualityEvent(context.Context, gateway.QualityObservation, time.Duration) error {
	return nil
}

type voiceCompletionFixture struct {
	registry       *provider.Registry
	dbPath         string
	service        *gateway.Service
	audits         *relational.AuditRepository
	clients        *relational.ClientKeyRepository
	clientService  *clientkeyapp.Service
	accounts       *relational.AccountRepository
	accountService *accountapp.Service
	models         *relational.ModelRepository
	jobs           *relational.MediaJobRepository
	concurrency    repository.ConcurrencyLimiter
	selector       *gateway.Selector
	created        clientkeyapp.Created
	receipts       *voiceReceiptSink
	account        account.Credential
	router         *gin.Engine
	media          *mediaapp.Service
	publicModel    string
	useProxy       func(string)
}

func newVoiceCompletionFixture(t *testing.T, endpoint, model string) voiceCompletionFixture {
	return newMediaCompletionFixture(t, endpoint, model, account.ProviderConsole, nil)
}

// The optional storage wrapper injects only a resource boundary failure. Model,
// account, transport, media persistence and billing use their real components.
func newMediaCompletionFixture(t *testing.T, endpoint, model string, kind account.Provider, wrapStore func(provider.ImageAssetStore) provider.ImageAssetStore) voiceCompletionFixture {
	return newProviderCompletionFixture(t, endpoint, model, kind, wrapStore, nil)
}

func newProviderCompletionFixture(t *testing.T, endpoint, model string, kind account.Provider, wrapStore func(provider.ImageAssetStore) provider.ImageAssetStore, wrapAdapter func(provider.Adapter) provider.Adapter) voiceCompletionFixture {
	return newProviderCompletionFixtureWithAccounts(t, endpoint, model, kind, wrapStore, wrapAdapter, nil)
}

func newProviderCompletionFixtureWithAccounts(t *testing.T, endpoint, model string, kind account.Provider, wrapStore func(provider.ImageAssetStore) provider.ImageAssetStore, wrapAdapter func(provider.Adapter) provider.Adapter, wrapAccounts func(repository.AccountRepository) repository.AccountRepository) voiceCompletionFixture {
	return newProviderCompletionFixtureWithAccountPorts(t, endpoint, model, kind, wrapStore, wrapAdapter, wrapAccounts, nil)
}

func newProviderCompletionFixtureWithAccountPorts(t *testing.T, endpoint, model string, kind account.Provider, wrapStore func(provider.ImageAssetStore) provider.ImageAssetStore, wrapAdapter func(provider.Adapter) provider.Adapter, wrapAccounts, wrapSelector func(repository.AccountRepository) repository.AccountRepository) voiceCompletionFixture {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "voice.db")
	db, err := relational.OpenSQLite(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return newProviderCompletionFixtureOnDatabase(t, endpoint, model, kind, wrapStore, wrapAdapter, wrapAccounts, wrapSelector, db, dbPath)
}

// The caller owns the initialized database, allowing the same real request and
// billing fixture to exercise SQLite and isolated PostgreSQL without rewiring it.
func newProviderCompletionFixtureOnDatabase(t *testing.T, endpoint, model string, kind account.Provider, wrapStore func(provider.ImageAssetStore) provider.ImageAssetStore, wrapAdapter func(provider.Adapter) provider.Adapter, wrapAccounts, wrapSelector func(repository.AccountRepository) repository.AccountRepository, db *relational.Database, dbPath string, limiters ...repository.ConcurrencyLimiter) voiceCompletionFixture {
	t.Helper()
	ctx := context.Background()
	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	token, _ := cipher.Encrypt("synthetic-sso")
	accounts, models := relational.NewAccountRepository(db), relational.NewModelRepository(db)
	audits, clients := relational.NewAuditRepository(db), relational.NewClientKeyRepository(db)
	network := egress.NewManager(relational.NewEgressRepository(db), cipher)
	t.Cleanup(func() { _ = network.Close(ctx) })
	var assetService *mediaapp.Service
	var store provider.ImageAssetStore
	if wrapStore != nil {
		objects, err := localmedia.NewLocalStore(filepath.Join(t.TempDir(), "objects"))
		if err != nil {
			t.Fatal(err)
		}
		assetService = mediaapp.NewServiceWithTickets(relational.NewMediaAssetRepository(db), relational.NewMediaJobRepository(db), relational.NewMediaUploadTicketRepository(db), objects, nil, mediaapp.Config{PublicBaseURL: "https://local.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Minute})
		store = wrapStore(assetService)
	}
	var adapter provider.Adapter = console.NewAdapter(console.Config{BaseURL: endpoint, Timeout: 5 * time.Second}, network, cipher, store)
	catalog := modeldomain.CatalogRoutes(account.ProviderConsole)
	if kind == account.ProviderWeb {
		adapter = webprovider.NewAdapter(webprovider.Config{BaseURL: endpoint, StatsigMode: "manual", ImageTimeout: 5 * time.Second, ChatTimeout: 5 * time.Second, MaxInputImageBytes: 32 << 20}, network, cipher, historyapp.NewResponseResources(relational.NewResponseRepository(db)), store)
		catalog = modeldomain.CatalogRoutes(account.ProviderWeb)
	}
	if kind == account.ProviderBuild {
		build := cli.NewAdapter(cli.Config{BaseURL: endpoint + "/v1"}, cipher)
		build.SetEgress(network)
		adapter = build
	}
	if wrapAdapter != nil {
		adapter = wrapAdapter(adapter)
	}
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: kind, Name: "voice", SourceKey: "voice", AuthType: account.AuthTypeSSO, EncryptedAccessToken: token, Enabled: true, AuthStatus: account.AuthStatusActive, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", WebTier: account.WebTierBasic, MaxConcurrent: 1, ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	var routes []modeldomain.Route
	publicModel := ""
	for _, route := range catalog {
		if route.UpstreamModel == model {
			routes = append(routes, route)
			publicModel = modeldomain.ExternalPublicID(kind, route.PublicID)
		}
	}
	if kind == account.ProviderBuild {
		if err := testsupport.Discover(ctx, models, kind, []string{model}); err != nil {
			t.Fatal(err)
		}
		publicModel = modeldomain.ExternalPublicID(kind, model)
	}
	if err := testsupport.Routes(ctx, models, routes); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{model}, time.Now()); err != nil {
		t.Fatal(err)
	}
	registry := provider.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	var concurrency repository.ConcurrencyLimiter = memory.NewConcurrencyLimiter()
	if len(limiters) > 0 {
		concurrency = limiters[0]
	}
	var accountStore repository.AccountRepository = accounts
	if wrapAccounts != nil {
		accountStore = wrapAccounts(accounts)
	}
	accountService := accountapp.NewService(accountStore, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
	clientService := clientkeyapp.NewService("test-owner", clients, memory.NewRateLimiter(), concurrency, 120, 4, cipher)
	t.Cleanup(func() { closeClientKeyService(t, clientService) })
	created, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "voice", Enabled: true, RPMLimit: 120, MaxConcurrent: 4, BillingLimitUSDTicks: 100_000_000_000})
	if err != nil {
		t.Fatal(err)
	}
	var selectorStore repository.AccountRepository = accounts
	if wrapSelector != nil {
		selectorStore = wrapSelector(accounts)
	}
	selector := gateway.NewSelector(selectorStore, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := gateway.NewService(models, audits, accountService, clientService, registry, selector, relational.NewResponseRepository(db), 1)
	receipts := &voiceReceiptSink{}
	service.SetQualityEventRecorder(receipts)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestID(), middleware.ClientAuth(clientService))
	modelService := modelapp.NewService(models, accounts, accountService, registry)
	t.Cleanup(func() { _ = modelService.Close(context.Background()) })
	NewHandler(service, modelService, 1<<20).Register(router.Group("/v1"))
	return voiceCompletionFixture{dbPath: dbPath, registry: registry, accountService: accountService, service: service, audits: audits, clients: clients, accounts: accounts, clientService: clientService, models: models, jobs: relational.NewMediaJobRepository(db), concurrency: concurrency, selector: selector, created: created, receipts: receipts, account: credential, router: router, media: assetService, publicModel: publicModel,
		useProxy: func(proxyURL string) {
			ciphertext, err := cipher.Encrypt(proxyURL)
			if err != nil {
				t.Fatal(err)
			}
			repo := relational.NewEgressRepository(db)
			node, err := repo.CreateEgressNode(ctx, egressdomain.Node{Name: "test-proxy", Enabled: true, Health: 1, EncryptedProxyURL: ciphertext})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.SaveEgressOperationsConfig(ctx, egressdomain.OperationsConfig{DefaultTarget: egressdomain.RoutingTarget{Mode: egressdomain.RoutingTargetNode, NodeID: node.ID}}, func(egressdomain.Node) error {
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			network.InvalidateNodeSnapshots()
			network.InvalidateOperationsConfig()
		},
	}
}

// Cold DPoP, actual Console WS transport, HTTP relay, SQL audit and billing are
// exercised together; only the independent physical receipt store may fail.
func TestVoiceHTTPCompletionIntegration(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) { testVoiceHTTPCompletionIntegration(t, dialect) })
	}
}

func testVoiceHTTPCompletionIntegration(t *testing.T, dialect string) {
	for _, tc := range []struct {
		name, path, model, generation, delivery, closeSide string
		events                                             []string
		duration                                           int64
		retry, rejected, receiptFailure                    bool
	}{
		{name: "stt", path: "stt", model: "grok-stt", generation: "completed", delivery: "completed", events: []string{`{"type":"transcript.created"}`, `{"type":"transcript.done","duration":3.45}`}, duration: 3450},
		{name: "stt_client_disconnect", path: "stt", model: "grok-stt", generation: "completed", delivery: "canceled", closeSide: "client", events: []string{`{"type":"transcript.done","duration":3.45}`}, duration: 3450},
		{name: "stt_request_cancel", path: "stt", model: "grok-stt", generation: "completed", delivery: "canceled", closeSide: "cancel", events: []string{`{"type":"transcript.done","duration":3.45}`}, duration: 3450},
		{name: "stt_receipt_failure", path: "stt", model: "grok-stt", generation: "completed", delivery: "completed", events: []string{`{"type":"transcript.done","duration":3.45}`}, duration: 3450, receiptFailure: true},
		{name: "realtime_unknown", path: "realtime", model: "grok-voice-latest", generation: "unconfirmed", delivery: "completed", events: []string{`{"type":"session.created"}`}},
		{name: "realtime_partial", path: "realtime", model: "grok-voice-latest", generation: "partial", delivery: "completed", events: []string{`{"type":"response.done","response":{"status":"completed"}}`, `{"type":"response.created"}`}},
		{name: "realtime_failed", path: "realtime", model: "grok-voice-latest", generation: "failed", delivery: "completed", events: []string{`{"type":"response.done","response":{"status":"failed"}}`}},
		{name: "upstream_abrupt_close", path: "realtime", model: "grok-voice-latest", generation: "unconfirmed", delivery: "failed", closeSide: "upstream", events: []string{`{"type":"response.created"}`}},
		{name: "dpop_retry", path: "stt", model: "grok-stt", generation: "completed", delivery: "completed", events: []string{`{"type":"transcript.done","duration":0}`}, retry: true},
		{name: "rejected", path: "stt", model: "grok-stt", generation: "not_started", delivery: "not_started", rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handshakes atomic.Int32
			upstream := voiceCompletionUpstream(t, &handshakes, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if tc.rejected || (tc.retry && handshakes.Load() == 1) {
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
					return
				}
				conn, err := (&upstreamws.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				typ, data, err := conn.ReadMessage()
				if err != nil || typ != upstreamws.BinaryMessage || string(data) != "synthetic audio" {
					t.Errorf("upstream input=%s type=%d err=%v", data, typ, err)
					return
				}
				for _, event := range tc.events {
					if err := conn.WriteMessage(upstreamws.TextMessage, []byte(event)); err != nil {
						t.Error(err)
						return
					}
				}
				if tc.closeSide == "client" || tc.closeSide == "cancel" {
					_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					_, _, _ = conn.ReadMessage()
					return
				}
				if tc.closeSide != "upstream" {
					_ = conn.WriteControl(upstreamws.CloseMessage, upstreamws.FormatCloseMessage(1000, "done"), time.Now().Add(time.Second))
				}
			})
			defer upstream.Close()
			fx := newProviderCompletionFixtureOnDatabase(t, upstream.URL, tc.model, account.ProviderConsole, nil, nil, nil, nil, compactionDatabase(t, dialect), "")
			fx.receipts.fail = tc.receiptFailure
			requestCtx, cancelRequest := context.WithCancel(context.Background())
			defer cancelRequest()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fx.router.ServeHTTP(w, r.WithContext(requestCtx)) }))
			defer server.Close()
			conn, response, err := clientws.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/"+tc.path+"?model="+tc.model, http.Header{"Authorization": []string{"Bearer " + fx.created.Secret}})
			var bytes int64
			if tc.rejected {
				if err == nil || response == nil || response.StatusCode == 101 {
					t.Fatalf("rejection: %v %v", response, err)
				}
				if response.Body != nil {
					_ = response.Body.Close()
				}
			} else {
				if err != nil {
					t.Fatalf("dial: %v response=%v", err, response)
				}
				defer conn.Close()
				if err := conn.WriteMessage(clientws.BinaryMessage, []byte("synthetic audio")); err != nil {
					t.Fatal(err)
				}
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				for _, event := range tc.events {
					_, data, err := conn.ReadMessage()
					if err != nil || string(data) != event {
						t.Fatalf("payload=%s err=%v", data, err)
					}
					bytes += int64(len(data))
				}
				if tc.closeSide == "client" {
					_ = conn.Close()
				} else if tc.closeSide == "cancel" {
					cancelRequest()
					_, _, _ = conn.ReadMessage()
				} else {
					_, _, _ = conn.ReadMessage()
				}
			}
			record := waitVoiceAudit(t, fx.audits)
			if record.GenerationOutcome != tc.generation || record.DeliveryOutcome != tc.delivery || record.AudioDurationMS != tc.duration || record.DeliveredBytes != bytes || record.DeliveredEvents != int64(len(tc.events)) || record.LedgerOutcome != "committed" {
				t.Fatalf("completion: %+v", record)
			}
			if record.AccountID == nil || *record.AccountID != fx.account.ID {
				t.Fatalf("account attribution: %+v", record)
			}
			wantReceipt := "committed"
			if tc.receiptFailure {
				wantReceipt = "failed"
			}
			if record.PhysicalReceipt != wantReceipt {
				t.Fatalf("independent receipt: %+v", record)
			}
			key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
			if err != nil || key.BilledUsageUSDTicks != record.EstimatedCostInUSDTicks {
				t.Fatalf("billing=%+v record=%+v err=%v", key, record, err)
			}
			if tc.duration > 0 && (record.UsageSource != audit.UsageSourceUpstream || record.EstimatedCostInUSDTicks <= 0) {
				t.Fatalf("known usage disappeared: %+v", record)
			}
			if tc.path == "realtime" && (record.TotalTokens != 0 || record.UsageSource != audit.UsageSourceNone) {
				t.Fatalf("invented unknown usage: %+v", record)
			}
			fx.receipts.mu.Lock()
			facts := append([]attemptmeta.PhysicalFact(nil), fx.receipts.facts...)
			fx.receipts.mu.Unlock()
			wantCalls := 2
			if tc.retry || tc.rejected {
				wantCalls = 4
			}
			if len(facts) != wantCalls {
				t.Fatalf("calls=%+v", facts)
			}
			var payloadBytes int64
			for i, fact := range facts {
				if fact.Attempt.Ordinal != uint64(i+1) || fact.Attempt.AccountID != fx.account.ID || fact.Attempt.RequestID != record.EventID || fact.BodyOutcome == "pending" {
					t.Fatalf("unfinished identity=%+v", fact)
				}
				if fact.Status == 101 {
					payloadBytes += fact.BodyBytes
				}
			}
			if payloadBytes != bytes {
				t.Fatalf("physical bytes=%d delivered=%d facts=%+v", payloadBytes, bytes, facts)
			}
		})
	}
}

func TestVoiceUnclaimedCancellationIntegration(t *testing.T) {
	var handshakes atomic.Int32
	upstream := voiceCompletionUpstream(t, &handshakes, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		conn, err := (&upstreamws.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, _, _ = conn.ReadMessage()
	})
	defer upstream.Close()
	fx := newVoiceCompletionFixture(t, upstream.URL, "grok-stt")
	ctx, cancel := context.WithCancel(context.Background())
	session, err := fx.service.OpenVoiceWebSocket(ctx, gateway.VoiceWebSocketInput{RequestID: "unclaimed", ClientKey: fx.created.Key, PublicModel: "grok-stt", Path: "/stt"})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cancel()
	record := waitVoiceAudit(t, fx.audits)
	if record.GenerationOutcome != "unconfirmed" || record.DeliveryOutcome != "canceled" || record.AdmissionOutcome != "not_admitted" || record.PhysicalReceipt != "committed" || record.DeliveredBytes != 0 {
		t.Fatalf("abandoned session: %+v", record)
	}
	if !errors.Is(session.BeginDelivery(), context.Canceled) {
		t.Fatal("claimed already canceled session")
	}
	session.Finalize(gateway.VoiceWebSocketOutcome{})
	_, count, err := fx.audits.List(context.Background(), 0, 10)
	if err != nil || count != 1 {
		t.Fatalf("duplicate audit count=%d err=%v", count, err)
	}
	// The one-slot account can be reacquired after the abandoned caller is gone.
	next, err := fx.service.OpenVoiceWebSocket(context.Background(), gateway.VoiceWebSocketInput{RequestID: "next", ClientKey: fx.created.Key, PublicModel: "grok-stt", Path: "/stt"})
	if err != nil {
		t.Fatalf("account/network capacity leaked: %v", err)
	}
	next.Finalize(gateway.VoiceWebSocketOutcome{ErrorCode: "request_canceled"})
}

func waitVoiceAudit(t *testing.T, audits *relational.AuditRepository) audit.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		values, count, err := audits.List(context.Background(), 0, 2)
		if err != nil {
			t.Fatal(err)
		}
		if count > 0 {
			if count != 1 {
				t.Fatalf("duplicate audits: %d", count)
			}
			return values[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("voice finalization did not produce an audit")
	return audit.Record{}
}
func voiceCompletionUpstream(t *testing.T, handshakes *atomic.Int32, serve func(fhttp.ResponseWriter, *fhttp.Request)) *fhttptest.Server {
	t.Helper()
	return fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
		if r.URL.Path == "/v1/dpop/token" {
			var input struct {
				JWK map[string]string `json:"jwk"`
			}
			if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
				t.Error(err)
				return
			}
			canonical, _ := json.Marshal(map[string]string{"crv": input.JWK["crv"], "kty": input.JWK["kty"], "x": input.JWK["x"], "y": input.JWK["y"]})
			digest := sha256.Sum256(canonical)
			claims, _ := json.Marshal(map[string]any{"sub": "synthetic", "exp": time.Now().Add(5 * time.Minute).Unix(), "cnf": map[string]any{"jkt": base64.RawURLEncoding.EncodeToString(digest[:])}})
			token := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`)) + "." + base64.RawURLEncoding.EncodeToString(claims) + ".dGVzdA"
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 300})
			return
		}
		handshakes.Add(1)
		serve(w, r)
	}))
}
