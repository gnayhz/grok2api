package inference

import (
	"context"
	"encoding/json"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestHTTPCompactionRetainsToolsAndPhysicalBudget(t *testing.T) {
	gin.SetMode(gin.TestMode)
	functions := `[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"save","parameters":{"type":"object"}}]`
	mcp := `[{"type":"mcp","server_label":"docs","server_url":"https://example.test/mcp","allowed_tools":["lookup"],"require_approval":"never"},{"type":"function","name":"save","parameters":{"type":"object"}}]`
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, streaming := range []bool{false, true} {
			for _, tc := range []struct {
				name, tools, choice, want string
				toolCount, status, calls  int
				failFirst                 bool
			}{
				{"none", functions, `"none"`, `"none"`, 2, 200, 1, false},
				{"required", functions, `"required"`, `"required"`, 2, 200, 1, false},
				{"function", functions, `{"type":"function","name":"lookup"}`, `{"type":"function","name":"lookup"}`, 2, 200, 1, false},
				{"hosted", `[{"type":"web_search"},{"type":"function","name":"save","parameters":{"type":"object"}}]`, `{"type":"web_search"}`, `"required"`, 1, 200, 1, false},
				{"mcp", mcp, `{"type":"mcp","server_label":"docs"}`, `"required"`, 1, 200, 1, false},
				{"default", functions, `null`, `"auto"`, 2, 200, 1, false},
				{"undeclared", functions, `{"type":"function","name":"undeclared"}`, `null`, 0, 400, 0, false},
				// The raw request's unknown server tool is harmless when disabled; the
				// normalized summary request must retain that fact for the shared budget.
				{"disabled_mcp_retry", mcp, `"none"`, `"none"`, 2, 200, 2, true},
				{"disabled_mcp_budget", mcp, `"none"`, `"none"`, 2, 503, 2, true},
				{"enabled_mcp_no_retry", mcp, `{"type":"mcp","server_label":"docs"}`, `"required"`, 1, 503, 1, true},
			} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", dialect, streaming, tc.name), func(t *testing.T) {
					var mu sync.Mutex
					var wire []map[string]json.RawMessage
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						data, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						var payload map[string]json.RawMessage
						if err := json.Unmarshal(data, &payload); err != nil {
							t.Error(err)
							return
						}
						mu.Lock()
						wire = append(wire, payload)
						count := len(wire)
						mu.Unlock()
						if tc.failFirst && (count == 1 || tc.name == "disabled_mcp_budget") {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusServiceUnavailable)
							_, _ = io.WriteString(w, `{"error":{"type":"server_error","message":"synthetic unavailable"}}`)
							return
						}
						summary := strings.Repeat("The synthetic user requested a local summary. Retain the chosen tool permission and pending steps. ", 10)
						response := map[string]any{"id": "resp_tool_summary", "object": "response", "status": "completed", "model": "grok-4.5", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": summary}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
						event, err := json.Marshal(map[string]any{"type": "response.completed", "response": response})
						if err != nil {
							t.Error(err)
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", event)
					}))
					t.Cleanup(upstream.Close)
					endpoint, secret, handled := compactionToolGateway(t, dialect, upstream.URL)
					body := fmt.Sprintf(`{"model":"grok-4.5","stream":%t,"input":[{"role":"user","content":"summarize"},{"type":"compaction_trigger"}],"tools":%s,"tool_choice":%s}`, streaming, tc.tools, tc.choice)
					req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, endpoint.URL+"/v1/responses", strings.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+secret)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("X-Session-Id", "compaction-tool-contract")
					res, err := endpoint.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					data, readErr := io.ReadAll(res.Body)
					_ = res.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
					select {
					case <-handled:
					case <-time.After(3 * time.Second):
						t.Fatal("HTTP completion did not finish")
					}
					if res.StatusCode != tc.status {
						t.Fatalf("status=%d want=%d body=%s", res.StatusCode, tc.status, data)
					}
					if tc.status == 200 && !strings.Contains(string(data), `"type":"compaction"`) {
						t.Fatalf("missing compaction result: %s", data)
					}
					mu.Lock()
					defer mu.Unlock()
					if len(wire) != tc.calls {
						t.Fatalf("physical calls=%d want=%d", len(wire), tc.calls)
					}
					for index, payload := range wire {
						var got, want any
						if err := json.Unmarshal(payload["tool_choice"], &got); err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal([]byte(tc.want), &want); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(got, want) {
							t.Errorf("attempt %d choice=%v want=%v", index, got, want)
						}
						var tools []map[string]any
						if err := json.Unmarshal(payload["tools"], &tools); err != nil {
							t.Fatal(err)
						}
						if len(tools) != tc.toolCount {
							t.Fatalf("attempt %d tools=%v", index, tools)
						}
						if tc.name == "mcp" || tc.name == "enabled_mcp_no_retry" {
							if tools[0]["type"] != "mcp" || tools[0]["server_label"] != "docs" {
								t.Errorf("forced MCP scope changed: %v", tools)
							}
						}
					}
				})
			}
		}
	}
}

func compactionToolGateway(t *testing.T, dialect, upstream string) (*httptest.Server, string, <-chan struct{}) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ctx := context.Background()
	db := compactionDatabase(t, dialect)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	accounts := relational.NewAccountRepository(db)
	models := relational.NewModelRepository(db)
	audits := relational.NewAuditRepository(db)
	keys := relational.NewClientKeyRepository(db)
	token, err := cipher.Encrypt("synthetic-compaction-tools")
	if err != nil {
		t.Fatal(err)
	}
	credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "compaction-tools", SourceKey: "compaction-tools", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive, MaxConcurrent: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{"grok-4.5"}, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
		t.Fatal(err)
	}
	network := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
	network.SetLogger(logger)
	t.Cleanup(func() { _ = network.Close(context.Background()) })
	build := cli.NewAdapter(cli.Config{BaseURL: upstream + "/v1"}, cipher)
	build.SetEgress(network)
	build.SetLogger(logger)
	history := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, logger)
	history.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
	build.SetReasoningReplay(history)
	registry := providerimpl.NewRegistry(build)
	sticky := memory.NewStickyStore()
	concurrency := memory.NewConcurrencyLimiter()
	maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
	clients := clientkeyapp.NewService("compaction-tools", keys, memory.NewRateLimiter(), concurrency, 120, 4, cipher, security.RandomTokenSource{})
	t.Cleanup(func() { closeClientKeyService(t, clients) })
	key, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "compaction-tools", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
	if err != nil {
		t.Fatal(err)
	}
	selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
	service := gateway.NewService(models, audits, maintenance, clients, registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
	service.SetLogger(logger)
	router := gin.New()
	router.Use(middleware.RequestID(nil), middleware.ClientAuth(clients))
	NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
	handled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { router.ServeHTTP(w, r); handled <- struct{}{} }))
	t.Cleanup(server.Close)
	return server, key.Secret, handled
}
