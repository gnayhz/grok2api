package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

// Real transport/auth/routing/account/history/Build cooperation verifies that
// recovery is part of a single request-wide budget rather than an adapter opt-in
// applied only by isolated tests.
func TestHTTPGatewayHistoryRecoveryBudget(t *testing.T) {
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, tc := range []struct {
				limit       int
				rateLimited bool
				failed      bool
			}{{1, false, false}, {2, false, false}, {3, false, false}, {3, true, false}, {2, false, true}} {
				limit, rateLimited := tc.limit, tc.rateLimited
				t.Run(fmt.Sprintf("%s/stream=%t/limit=%d/rate=%t/failed=%t", operation, stream, limit, rateLimited, tc.failed), func(t *testing.T) {
					var strictReject atomic.Bool
					var mu sync.Mutex
					calls := 0
					followupCalls := 0
					var authorization []string
					raw := make([]byte, 256)
					for i := range raw {
						raw[i] = byte(i)
					}
					opaque := base64.RawStdEncoding.EncodeToString(raw)
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						var payload map[string]any
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
						}
						mu.Lock()
						calls++
						authorization = append(authorization, r.Header.Get("Authorization"))
						call := calls
						if call > 1 {
							followupCalls++
						}
						followup := followupCalls
						mu.Unlock()
						if rateLimited && followup == 2 {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(429)
							_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
							return
						}
						if strictReject.Load() || tc.failed && call > 1 || call > 1 && followup < limit || call == 2 {
							if followup == 1 && !strictReject.Load() {
								data, _ := json.Marshal(payload)
								if !strings.Contains(string(data), opaque) {
									t.Error("durable history not restored")
								}
							}
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(400)
							_, _ = io.WriteString(w, `{"error":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}`)
							return
						}
						if call > 1 {
							data, _ := json.Marshal(payload)
							if strings.Contains(string(data), opaque) {
								t.Error("recovery sent rejected opaque")
							}
							if limit == 3 && !rateLimited && r.Header.Get("x-grok-session-id") != "" {
								t.Error("second recovery retained session hint")
							}
						}
						answer := map[string]any{"id": fmt.Sprintf("response-%d", call), "object": "response", "status": "completed", "model": "grok-4.5", "output": []any{map[string]any{"type": "reasoning", "encrypted_content": opaque, "summary": []any{}}, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "answer"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
						if payload["stream"] == true {
							w.Header().Set("Content-Type", "text/event-stream")
							data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": answer})
							fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_ = json.NewEncoder(w).Encode(answer)
						}
					}))
					defer upstream.Close()
					ctx := context.Background()
					db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "history.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if err = db.InitializeSchema(ctx); err != nil {
						t.Fatal(err)
					}
					cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))

					accounts := relational.NewAccountRepository(db)
					models := relational.NewModelRepository(db)
					audits := relational.NewAuditRepository(db)
					keys := relational.NewClientKeyRepository(db)
					for _, name := range []string{"one", "two"} {
						token, _ := cipher.Encrypt("synthetic-" + name)
						credential, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: name, SourceKey: name, EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2})
						if err != nil {
							t.Fatal(err)
						}
						if err = testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{"grok-4.5"}, time.Now()); err != nil {
							t.Fatal(err)
						}
					}
					if err = testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
						t.Fatal(err)
					}
					build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
					replay := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, nil)
					replay.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
					build.SetReasoningReplay(replay)
					egress := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
					defer egress.Close(ctx)
					build.SetEgress(egress)
					registry := providerimpl.NewRegistry(build)
					sticky := memory.NewStickyStore()
					concurrency := memory.NewConcurrencyLimiter()
					accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
					clientService := clientkeyapp.NewService("test-owner", keys, memory.NewRateLimiter(), concurrency, 120, 4, cipher, security.RandomTokenSource{})
					defer closeClientKeyService(t, clientService)
					created, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "history", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
					service := gateway.NewService(models, audits, accountService, clientService, registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, limit)
					gin.SetMode(gin.TestMode)
					router := gin.New()
					router.Use(middleware.RequestID(nil), middleware.ClientAuth(clientService))
					NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
					server := httptest.NewServer(router)
					defer server.Close()
					for turn := 0; turn < 2; turn++ {
						history := []any{map[string]any{"role": "user", "content": "hello"}}
						if turn == 1 {
							history = append(history, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next"})
						}
						payload := map[string]any{"model": "grok-4.5", "stream": stream}
						if operation == "responses" {
							payload["input"] = history
						} else {
							payload["messages"] = history
						}
						if operation == "messages" {
							payload["max_tokens"] = 128
						}
						body, _ := json.Marshal(payload)
						request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(body)))
						request.Header.Set("Authorization", "Bearer "+created.Secret)
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("anthropic-version", "2023-06-01")
						request.Header.Set("X-Session-Id", "history-client")
						response, err := server.Client().Do(request)
						if err != nil {
							t.Fatal(err)
						}
						output, _ := io.ReadAll(response.Body)
						_ = response.Body.Close()
						want := 200
						if turn == 1 && (limit == 1 || tc.failed) {
							want = 400
						}
						if response.StatusCode != want {
							t.Fatalf("turn=%d status=%d want=%d body=%s", turn, response.StatusCode, want, output)
						}
						if turn == 1 && limit > 1 && !strings.Contains(response.Header.Get("X-Grok2API-Compatibility-Warnings"), "reasoning_encrypted_content_downgraded") {
							t.Fatal("recovery loss warning missing")
						}
					}
					mu.Lock()
					count := followupCalls
					if rateLimited && (len(authorization) != 4 || authorization[1] != authorization[2] || authorization[2] == authorization[3]) {
						t.Error("429 recovery did not remain on same account before failover")
					}
					mu.Unlock()
					if count != limit {
						t.Fatalf("physical calls=%d want=%d", count, limit)
					}
					if operation == "responses" && !stream && limit == 1 {
						strictReject.Store(true)
						service.UpdateMaxAttempts(3)
						mode := historydomain.PreserveOpaque
						response, err := service.CreateResponse(ctx, gateway.Input{RequestID: "strict-policy", ClientKey: created.Key, PublicModel: "grok-4.5", PromptCacheKey: "strict", HistoryRecoveryPolicy: &mode, Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","encrypted_content":"` + opaque + `","summary":[]},{"role":"user","content":"next"}]}`)})
						var failure *gateway.UpstreamFailure
						if response != nil {
							_ = response.Body.Close()
						}
						if !errors.As(err, &failure) || failure.HTTPStatus != 400 {
							t.Fatalf("strict response=%v error=%v", response, err)
						}
						mu.Lock()
						strictCalls := followupCalls - count
						mu.Unlock()
						if strictCalls != 1 {
							t.Fatalf("strict policy made %d attempts", strictCalls)
						}
					}

				})
			}
		}
	}
}
