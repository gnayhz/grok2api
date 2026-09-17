package inference

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type responseRetentionHooks struct {
	useModelService bool
	limiter         repository.ConcurrencyLimiter
	wrapAdapter     func(provider.Adapter) provider.Adapter
	middleware      []gin.HandlerFunc
	beforeToken     func(*http.Request)
	tokenBody       func([]byte) []byte
	beforeUsage     func(http.ResponseWriter, *http.Request) bool
	beforeResponse  func(http.ResponseWriter, *http.Request) bool
}

type responseRetentionFixture struct {
	limiter          repository.ConcurrencyLimiter
	gateway          *gateway.Service
	adapter          provider.ResponseAdapter
	accountService   *accountapp.Service
	tokenBody        func([]byte) []byte
	beforeUsage      func(http.ResponseWriter, *http.Request) bool
	accounts         *relational.AccountRepository
	restartGateway   func()
	beforeResponse   func(http.ResponseWriter, *http.Request) bool
	beforeToken      func(*http.Request)
	router           *gin.Engine
	model, secret    string
	keyID, accountID uint64
	states           *completionFaultStore
	journal          *completionFaultJournal
	audits           *relational.AuditRepository
	receipts         *completionReceiptSink
	generated        atomic.Int32
	mu               sync.Mutex
	parents          []string
	wireStores       []any
}

func newResponseRetentionFixture(t testing.TB, dialect string, kind account.Provider, hooks ...responseRetentionHooks) *responseRetentionFixture {
	t.Helper()
	gin.SetMode(gin.TestMode)
	f := &responseRetentionFixture{}
	if len(hooks) > 0 {
		f.beforeToken, f.beforeResponse = hooks[0].beforeToken, hooks[0].beforeResponse
		f.tokenBody, f.beforeUsage = hooks[0].tokenBody, hooks[0].beforeUsage
	}
	ctx := context.Background()
	db := compactionDatabase(t, dialect)

	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	token, _ := cipher.Encrypt("test-sso")
	accounts, models := relational.NewAccountRepository(db), relational.NewModelRepository(db)
	audits := relational.NewAuditRepository(db)
	states := &completionFaultStore{ResponseRepository: relational.NewResponseRepository(db), failOwnership: false, failState: false}
	journal := &completionFaultJournal{ConversationJournal: relational.NewConversationJournal(db, cipher, 8<<20), fail: false}
	network := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = network.Close(ctx) })
	model := "grok-4.5"
	if kind == account.ProviderWeb {
		model = "grok-chat-fast"
	}
	var adapter provider.Adapter
	if kind == account.ProviderWeb {
		upstream := retentionWebUpstream(t, f)
		t.Cleanup(upstream.Close)
		adapter = webprovider.NewAdapter(webprovider.Config{BaseURL: upstream.URL, StatsigMode: "manual", ChatTimeout: 5 * time.Second}, network, cipher, historyapp.NewResponseResources(states), nil)
	} else {
		upstream := retentionHTTPUpstream(t, model, f)
		t.Cleanup(upstream.Close)
		if kind == account.ProviderBuild {
			build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
			build.SetEgress(network)
			history := historyapp.New(memory.NewReasoningReplayStore(8), historyapp.Config{Enabled: true, TTL: time.Hour}, nil)
			history.UseJournal(journal, time.Hour, time.Hour)
			build.SetReasoningReplay(history)
			adapter = build
		} else {
			adapter = console.NewAdapter(console.Config{BaseURL: upstream.URL, Timeout: 5 * time.Second}, network, cipher, nil)
		}
	}
	credential := account.Credential{Provider: kind, Name: "completion", Email: "fixture@example.test", SourceKey: "completion", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"}
	if kind != account.ProviderBuild {
		credential.AuthType = account.AuthTypeSSO
	}
	if kind == account.ProviderWeb {
		credential.WebTier = account.WebTierBasic
	}
	credential, _, err := accounts.UpsertByIdentity(ctx, credential)
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Discover(ctx, models, kind, []string{model}); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{model}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(hooks) > 0 && hooks[0].wrapAdapter != nil {
		adapter = hooks[0].wrapAdapter(adapter)
	}
	registry := providerimpl.NewRegistry(adapter)
	sticky := memory.NewStickyStore()
	var concurrency repository.ConcurrencyLimiter = memory.NewConcurrencyLimiter()
	if len(hooks) > 0 && hooks[0].limiter != nil {
		concurrency = hooks[0].limiter
	}
	f.limiter = concurrency
	accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
	clients := clientkeyapp.NewService("test-owner", relational.NewClientKeyRepository(db), memory.NewRateLimiter(), concurrency, 100000, 4, cipher, security.RandomTokenSource{})
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := clients.Close(closeCtx); err != nil {
			t.Error(err)
		}
	})
	created, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "completion", Enabled: true, RPMLimit: 100000, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	f.restartGateway = func() {
		selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
		var service *gateway.Service
		if len(hooks) > 0 && hooks[0].useModelService {
			modelService := modelapp.NewService(models, accounts, accountService, registry)
			t.Cleanup(func() { _ = modelService.Close(context.Background()) })
			service = gateway.NewService(modelService, audits, accountService, clients, registry, selector, historyapp.NewResponseResources(states), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 3)
		} else {
			service = gateway.NewService(models, audits, accountService, clients, registry, selector, historyapp.NewResponseResources(states), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 3)
		}
		service.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
		service.SetGuardSnapshotSource(gateway.StaticGuardSnapshotSource(gateway.QualityRetryRuntime{Enabled: true, MaxAttempts: 2, GuardedModels: []string{"grok-4.5", "grok-chat-fast"}}))
		receipts := &completionReceiptSink{fail: false}
		service.SetQualityEventRecorder(receipts)
		router := gin.New()
		router.Use(middleware.RequestID(nil))
		if len(hooks) > 0 {
			router.Use(hooks[0].middleware...)
		}
		router.Use(middleware.ClientAuth(clients))
		NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))

		f.router, f.receipts, f.gateway = router, receipts, service
	}
	f.restartGateway()

	f.accountService = accountService
	f.adapter, _ = adapter.(provider.ResponseAdapter)
	f.accounts = accounts
	f.model, f.secret, f.keyID, f.accountID = model, created.Secret, created.Key.ID, credential.ID
	f.states, f.journal, f.audits = states, journal, audits
	return f
}

func (f *responseRetentionFixture) request(method, path string, body []byte, session string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(string(body)))
	request.Header.Set("Authorization", "Bearer "+f.secret)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-Id", session)
	if path == "/v1/messages" {
		request.Header.Set("Anthropic-Version", "2023-06-01")
	}
	output := httptest.NewRecorder()
	f.router.ServeHTTP(output, request)
	return output
}

func (f *responseRetentionFixture) create(t testing.TB, streaming bool, fields map[string]any, session string) *httptest.ResponseRecorder {
	t.Helper()
	payload := map[string]any{"model": f.model, "stream": streaming, "input": "hello"}
	for k, v := range fields {
		payload[k] = v
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return f.request(http.MethodPost, "/v1/responses", data, session)
}

func retentionResponse(t testing.TB, output *httptest.ResponseRecorder, streaming bool) map[string]any {
	t.Helper()
	if output.Code != 200 {
		t.Fatalf("generation=%d: %s", output.Code, output.Body.String())
	}
	var value map[string]any
	if !streaming {
		if err := json.Unmarshal(output.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
	} else {
		for _, line := range strings.Split(output.Body.String(), "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event map[string]any
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) == nil && event["type"] == "response.completed" {
				value, _ = event["response"].(map[string]any)
			}
		}
	}
	if id, _ := value["id"].(string); id == "" {
		t.Fatalf("missing response: %s", output.Body.String())
	}
	return value
}

func (f *responseRetentionFixture) lastAudit(t testing.TB) audit.Record {
	t.Helper()
	values, count, err := f.audits.List(context.Background(), 0, 1)
	if err != nil || count == 0 {
		t.Fatalf("audit count=%d err=%v", count, err)
	}
	record, err := f.audits.Get(context.Background(), values[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (f *responseRetentionFixture) assertMissing(t testing.TB, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := f.states.Get(ctx, id, f.keyID, time.Now()); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("unexpected ownership: %v", err)
	}
	if _, err := f.states.GetWebState(ctx, id, time.Now()); !errors.Is(err, repository.ErrNotFound) {
		t.Errorf("unexpected native state: %v", err)
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		got := f.request(method, "/v1/responses/"+id, nil, "")
		if got.Code != http.StatusNotFound {
			t.Errorf("temporary resource %s=%d: %s", method, got.Code, got.Body.String())
		}
	}
}
func retentionHTTPUpstream(t testing.TB, model string, f *responseRetentionFixture) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/dpop/token" {
			if f.beforeToken != nil {
				f.beforeToken(r)
			}
			if r.Context().Err() != nil {
				return
			}
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
			tokenBody, _ := json.Marshal(map[string]any{"access_token": token, "token_type": "DPoP", "expires_in": 300})
			tokenBody = append(tokenBody, '\n')
			if f.tokenBody != nil {
				tokenBody = f.tokenBody(tokenBody)
			}
			_, _ = w.Write(tokenBody)
			return
		}
		if r.URL.Path == "/v1/usage" && f.beforeUsage != nil && f.beforeUsage(w, r) {
			return
		}
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		if f.beforeResponse != nil && f.beforeResponse(w, r) {
			return
		}
		call := f.generated.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		answer := map[string]any{"id": fmt.Sprintf("resp_retention_%d", call), "object": "response", "status": "completed", "model": model, "output": []any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "completion thought"}}}, map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "completion answer"}}}}, "usage": map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25, "output_tokens_details": map[string]any{"reasoning_tokens": 2}}}
		f.mu.Lock()
		f.wireStores = append(f.wireStores, payload["store"])
		f.mu.Unlock()
		answer["store"] = payload["store"]
		suppressThinking := answer["suppress_thinking"] == true
		delete(answer, "suppress_thinking")
		if payload["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []any{
				map[string]any{"type": "response.created", "response": map[string]any{"id": fmt.Sprintf("resp_retention_%d", call), "model": model}},
				map[string]any{"type": "response.reasoning_text.delta", "item_id": "rs_1", "delta": "completion thought"},
				map[string]any{"type": "response.output_text.delta", "item_id": "msg_1", "delta": "completion answer"},
				map[string]any{"type": "response.completed", "response": answer},
			} {
				if (event.(map[string]any)["type"] == "response.reasoning_text.delta" || event.(map[string]any)["type"] == "response.output_text.delta") && suppressThinking {
					continue
				}
				data, _ := json.Marshal(event)
				fmt.Fprintf(w, "data: %s\n\n", data)
				w.(http.Flusher).Flush()
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(answer)
		}
	}))
}

func retentionWebUpstream(t testing.TB, f *responseRetentionFixture) *fhttptest.Server {
	t.Helper()
	return fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
		if r.URL.Path != "/ws/mgw/" {
			w.WriteHeader(404)
			return
		}
		conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		var initial map[string]any
		if err := conn.ReadJSON(&initial); err != nil {
			t.Error(err)
			return
		}
		id := initial["event"].(map[string]any)["event_id"]
		_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": map[string]any{"type": "session.created", "client_event_id": id}})
		_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_completion"}}})
		var item, create map[string]any
		if err := conn.ReadJSON(&item); err != nil {
			t.Error(err)
			return
		}
		if err := conn.ReadJSON(&create); err != nil {
			t.Error(err)
			return
		}
		call := f.generated.Add(1)
		f.mu.Lock()
		parent, _ := item["event"].(map[string]any)["parent_response_id"].(string)
		f.parents = append(f.parents, parent)
		f.mu.Unlock()
		for _, chunk := range []map[string]any{{"text": "completion thought", "channel": "CHANNEL_ASSISTANT_ANALYSIS"}, {"text": "completion answer", "channel": "CHANNEL_ASSISTANT_RESPONSE"}} {
			_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": chunk}}})
		}
		_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": fmt.Sprintf("parent_retention_%d", call), "status": "completed"}}})
	}))
}
