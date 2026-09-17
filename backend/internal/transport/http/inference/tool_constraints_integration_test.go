package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// This path includes HTTP decoding/auth, actual routing and account leases,
// Build normalization/cache planning, completion and protocol conversion.
func TestHTTPGatewayBuildToolConstraints(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "tools.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	encrypted, _ := cipher.Encrypt("synthetic-token")
	accounts := relational.NewAccountRepository(db)
	models := relational.NewModelRepository(db)
	audits := relational.NewAuditRepository(db)
	keys := relational.NewClientKeyRepository(db)
	for _, name := range []string{"first", "second"} {
		credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2})
		if err != nil {
			t.Fatal(err)
		}
		if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{"grok-4.5"}, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	if err := testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	var upstreamCalls atomic.Int32
	var upstreamReject atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if upstreamReject.Load() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"unsupported_parameter","param":"tools","message":"private-upstream-detail"}}`)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		tools, _ := body["tools"].([]any)
		if len(tools) != 1 || body["tool_choice"] != "required" {
			t.Errorf("wire authorization=%+v choice=%v", tools, body["tool_choice"])
		} else {
			search := tools[0].(map[string]any)
			filters, _ := search["filters"].(map[string]any)
			domains, _ := filters["allowed_domains"].([]any)
			if search["type"] != "web_search" || len(domains) != 1 || domains[0] != "example.com" {
				t.Errorf("lost domain scope: %+v", search)
			}
		}
		answer := map[string]any{"id": "resp_contract", "object": "response", "status": "completed", "model": "grok-4.5", "output": []any{map[string]any{"type": "message", "role": "assistant", "id": "msg_contract", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "tool contract preserved"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			frame, _ := json.Marshal(map[string]any{"type": "response.completed", "response": answer})
			fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", frame)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(answer)
		}
	}))
	defer upstream.Close()
	build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
	registry := providerimpl.NewRegistry(build)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
	clientService := clientkeyapp.NewService("test-owner", keys, memory.NewRateLimiter(), concurrency, 120, 4, cipher, security.RandomTokenSource{})
	defer closeClientKeyService(t, clientService)
	created, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "contract", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := gateway.NewService(models, audits, accountService, clientService, registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 3)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.RequestID(nil), middleware.ClientAuth(clientService))
	NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
	server := httptest.NewServer(router)
	defer server.Close()
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, rejected := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream=%t/rejected=%t", operation, stream, rejected), func(t *testing.T) {
					payload := map[string]any{"model": "grok-4.5", "stream": stream}
					search := map[string]any{"type": "web_search", "filters": map[string]any{"allowed_domains": []any{"example.com"}}}
					function := map[string]any{"type": "function", "name": "charge", "parameters": map[string]any{"type": "object"}}
					payload["tool_choice"] = map[string]any{"type": "web_search"}
					if operation == "responses" {
						payload["input"] = "hello"
					} else {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					}
					if operation == "chat/completions" {
						function = map[string]any{"type": "function", "function": map[string]any{"name": "charge", "parameters": map[string]any{"type": "object"}}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 128
						search = map[string]any{"type": "web_search_20250305", "name": "web_search", "allowed_domains": []any{"example.com"}}
						function = map[string]any{"name": "charge", "input_schema": map[string]any{"type": "object"}}
						payload["tool_choice"] = map[string]any{"type": "tool", "name": "web_search"}
					}
					if rejected {
						search["max_uses"] = 1
					}
					payload["tools"] = []any{search, function}
					body, _ := json.Marshal(payload)
					request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(body)))
					request.Header.Set("Authorization", "Bearer "+created.Secret)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("anthropic-version", "2023-06-01")
					request.Header.Set("X-Grok-Session-ID", "tool-constraints")
					before := upstreamCalls.Load()
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					output, _ := io.ReadAll(response.Body)
					if rejected {
						if response.StatusCode != 400 || upstreamCalls.Load() != before || !strings.Contains(string(output), "max_uses") {
							t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, upstreamCalls.Load()-before, output)
						}
						records, _, err := audits.List(ctx, 0, 1)
						if err != nil || len(records) != 1 {
							t.Fatalf("request audit missing: records=%v err=%v", records, err)
						}
						if record := records[0]; record.StatusCode != 400 || record.ErrorCode != "unsupported_parameter" || len(record.Attempts) != 0 || record.AccountID != nil {
							t.Fatalf("local validation recorded as upstream failure: %+v", record)
						}
						return
					}
					if response.StatusCode != 200 || upstreamCalls.Load() != before+1 || !strings.Contains(string(output), "tool contract preserved") {
						t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, upstreamCalls.Load()-before, output)
					}
				})
			}
		}
	}
	t.Run("upstream error cannot impersonate local validation", func(t *testing.T) {
		upstreamReject.Store(true)
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"grok-4.5","input":"hello"}`))
		request.Header.Set("Authorization", "Bearer "+created.Secret)
		request.Header.Set("Content-Type", "application/json")
		before := upstreamCalls.Load()
		response, err := server.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		output, _ := io.ReadAll(response.Body)
		if response.StatusCode != 400 || upstreamCalls.Load() != before+1 || strings.Contains(string(output), "private-upstream-detail") {
			t.Fatalf("untrusted upstream error: status=%d calls=%d body=%s", response.StatusCode, upstreamCalls.Load()-before, output)
		}
	})

}
