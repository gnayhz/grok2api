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
	"github.com/bogdanfinn/websocket"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type completionFaultStore struct {
	repository.ResponseRepository
	failOwnership, failState   bool
	ownershipCalls, stateCalls atomic.Int32
}

func (s *completionFaultStore) Save(ctx context.Context, value inferencedomain.ResponseOwnership) error {
	s.ownershipCalls.Add(1)
	if s.failOwnership {
		return errors.New("injected ownership failure")
	}
	return s.ResponseRepository.Save(ctx, value)
}
func (s *completionFaultStore) SaveWebState(ctx context.Context, value inferencedomain.WebResponseState) error {
	s.stateCalls.Add(1)
	if s.failState {
		return errors.New("injected native state failure")
	}
	return s.ResponseRepository.SaveWebState(ctx, value)
}

type completionFaultJournal struct {
	repository.ConversationJournal
	fail  bool
	calls atomic.Int32
}

func (j *completionFaultJournal) Commit(ctx context.Context, value repository.JournalCommit) error {
	j.calls.Add(1)
	if j.fail {
		return errors.New("injected history failure")
	}
	return j.ConversationJournal.Commit(ctx, value)
}

type completionReceiptSink struct {
	fail       bool
	completion atomic.Int32
	mu         sync.Mutex
	facts      []attemptmeta.PhysicalFact
}

func (s *completionReceiptSink) RecordPhysicalEvents(_ context.Context, facts []attemptmeta.PhysicalFact) error {
	s.mu.Lock()
	s.facts = append(s.facts, facts...)
	s.mu.Unlock()
	foundUsage := false
	for _, fact := range facts {
		foundUsage = foundUsage || fact.Usage.Found
	}
	if s.fail && foundUsage {
		return errors.New("injected physical receipt failure")
	}
	return nil
}
func (s *completionReceiptSink) RecordQualityEvent(_ context.Context, obs gateway.QualityObservation, _ time.Duration) error {
	if obs.Outcome == gateway.QualityObservedCompleted || obs.Outcome == gateway.QualityObservedInterrupted || obs.Outcome == gateway.QualityObservedCanceled {
		s.completion.Add(1)
		if s.fail {
			return errors.New("injected quality receipt failure")
		}
	}
	return nil
}

// The matrix uses actual auth, account leases, Provider HTTP/WS protocols,
// conversion, durable history and response state, completion barriers and SQL
// audit writes. Failure ports are injected only at the persistence boundary.
func TestHTTPCompletionProviderMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, streaming := range []bool{false, true} {
				stages := []string{"success", "receipt"}
				if kind == account.ProviderBuild {
					stages = append(stages, "history", "short_reasoning")
				}
				if operation == "responses" && kind != account.ProviderConsole {
					stages = append(stages, "ownership")
				}
				if operation == "responses" && kind == account.ProviderWeb {
					stages = append(stages, "state")
				}
				for _, stage := range stages {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", kind, operation, streaming, stage), func(t *testing.T) {
						ctx := context.Background()
						db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "completion.db"))
						if err != nil {
							t.Fatal(err)
						}
						defer db.Close()
						if err := db.InitializeSchema(ctx); err != nil {
							t.Fatal(err)
						}
						cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
						token, _ := cipher.Encrypt("test-sso")
						accounts, models := relational.NewAccountRepository(db), relational.NewModelRepository(db)
						audits := relational.NewAuditRepository(db)
						states := &completionFaultStore{ResponseRepository: relational.NewResponseRepository(db), failOwnership: stage == "ownership", failState: stage == "state"}
						journal := &completionFaultJournal{ConversationJournal: relational.NewConversationJournal(db, cipher, 8<<20), fail: stage == "history"}
						network := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
						defer network.Close(ctx)
						model := "grok-4.5"
						if kind == account.ProviderWeb {
							model = "grok-chat-fast"
						}
						var generated atomic.Int32
						var adapter provider.Adapter
						if kind == account.ProviderWeb {
							upstream := completionWebUpstream(t, &generated)
							defer upstream.Close()
							adapter = webprovider.NewAdapter(webprovider.Config{BaseURL: upstream.URL, StatsigMode: "manual", ChatTimeout: 5 * time.Second}, network, cipher, historyapp.NewResponseResources(states), nil)
						} else {
							upstream := completionHTTPUpstreamWithResponse(t, model, &generated, func(_ int32, answer map[string]any) {
								if stage == "short_reasoning" {
									raw := make([]byte, 32)
									for i := range raw {
										raw[i] = byte(i)
									}
									answer["output"].([]any)[0].(map[string]any)["encrypted_content"] = base64.RawStdEncoding.EncodeToString(raw)
								}
							})
							defer upstream.Close()
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
						credential := account.Credential{Provider: kind, Name: "completion", SourceKey: "completion", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca"}
						if kind != account.ProviderBuild {
							credential.AuthType = account.AuthTypeSSO
						}
						if kind == account.ProviderWeb {
							credential.WebTier = account.WebTierBasic
						}
						credential, _, err = accounts.UpsertByIdentity(ctx, credential)
						if err != nil {
							t.Fatal(err)
						}
						if err := testsupport.Discover(ctx, models, kind, []string{model}); err != nil {
							t.Fatal(err)
						}
						if err := testsupport.Capabilities(ctx, models, accounts, credential.ID, []string{model}, time.Now()); err != nil {
							t.Fatal(err)
						}
						registry := providerimpl.NewRegistry(adapter)
						sticky, concurrency := memory.NewStickyStore(), memory.NewConcurrencyLimiter()
						accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
						clients := clientkeyapp.NewService("test-owner", relational.NewClientKeyRepository(db), memory.NewRateLimiter(), concurrency, 120, 4, cipher, security.RandomTokenSource{})
						defer closeClientKeyService(t, clients)
						created, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "completion", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
						if err != nil {
							t.Fatal(err)
						}
						selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
						service := gateway.NewService(models, audits, accountService, clients, registry, selector, historyapp.NewResponseResources(states), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 3)
						service.SetGuardSnapshotSource(gateway.StaticGuardSnapshotSource(gateway.QualityRetryRuntime{Enabled: true, MaxAttempts: 2, GuardedModels: []string{"grok-4.5", "grok-chat-fast"}}))
						receipts := &completionReceiptSink{fail: stage == "receipt"}
						service.SetQualityEventRecorder(receipts)
						router := gin.New()
						router.Use(middleware.RequestID(nil), middleware.ClientAuth(clients))
						NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
						payload := map[string]any{"model": model, "stream": streaming}
						if operation == "responses" {
							payload["input"] = "hello"
						} else {
							payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
						}
						if operation == "messages" {
							payload["max_tokens"] = 2048
							payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
						}
						data, _ := json.Marshal(payload)
						request := httptest.NewRequest(http.MethodPost, "/v1/"+operation, strings.NewReader(string(data)))
						request.Header.Set("Authorization", "Bearer "+created.Secret)
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("anthropic-version", "2023-06-01")
						request.Header.Set("X-Session-Id", "completion-session")
						output := httptest.NewRecorder()
						router.ServeHTTP(output, request)
						if generated.Load() != 1 {
							t.Fatalf("completion failure generated again: calls=%d status=%d body=%s", generated.Load(), output.Code, output.Body.String())
						}
						values, count, err := audits.List(ctx, 0, 1)
						if err != nil || count != 1 {
							t.Fatalf("missing completion audit count=%d err=%v status=%d body=%s", count, err, output.Code, output.Body.String())
						}
						record, err := audits.Get(ctx, values[0].ID)
						if err != nil {
							t.Fatal(err)
						}
						if len(record.GenerationUsages) != 1 {
							t.Fatalf("generation facts=%+v", record.GenerationUsages)
						}
						fact := record.GenerationUsages[0]
						if !fact.Selected || fact.AccountID != credential.ID || fact.Outcome != "completed" || fact.InputTokens != record.InputTokens || fact.OutputTokens != record.OutputTokens || fact.PhysicalID == "" {
							t.Fatalf("generation/selected mismatch: fact=%+v audit=%+v", fact, record)
						}
						if kind != account.ProviderWeb && (fact.InputTokens != 20 || fact.OutputTokens != 5 || fact.ReasoningTokens != 2) {
							t.Fatalf("canonical counters lost during conversion: %+v", fact)
						}
						wantCode, history, state, owner := "", "not_required", "not_required", "not_required"
						if kind == account.ProviderBuild {
							history = "committed"
						}
						if kind == account.ProviderWeb && operation == "responses" {
							state = "committed"
						}
						if operation == "responses" && kind != account.ProviderConsole {
							owner = "committed"
						}
						switch stage {
						case "history":
							history, wantCode = "failed", "history_commit_failed"
							if operation == "responses" {
								owner = "not_committed"
							}
						case "state":
							state, owner, wantCode = "failed", "not_committed", "provider_state_commit_failed"
						case "ownership":
							owner, wantCode = "failed", "response_ownership_commit_failed"
						}
						if record.HistoryCommit != history || record.ProviderStateCommit != state || record.OwnershipCommit != owner || record.ErrorCode != wantCode || record.GenerationOutcome != "completed" || record.AdmissionOutcome != "admitted" || record.LedgerOutcome != "committed" {
							t.Fatalf("wrong independent outcomes: %+v status=%d body=%s", record, output.Code, output.Body.String())
						}
						if record.InputTokens == 0 || record.OutputTokens == 0 || record.DeliveredBytes != int64(output.Body.Len()) {
							t.Fatalf("lost usage/write facts: %+v bodyBytes=%d", record, output.Body.Len())
						}
						if stage == "receipt" && record.PhysicalReceipt != "failed" {
							t.Fatalf("physical receipt not independent: %+v", record)
						}
						if stage != "receipt" && record.PhysicalReceipt != "committed" {
							t.Fatalf("missing actual physical receipt: %+v", record)
						}
						if kind == account.ProviderWeb {
							receipts.mu.Lock()
							facts := append([]attemptmeta.PhysicalFact(nil), receipts.facts...)
							receipts.mu.Unlock()
							if len(facts) != 1 || facts[0].Status != 101 || facts[0].HeaderOutcome != "upgraded" || facts[0].Attempt.AccountID != credential.ID || facts[0].BodyBytes == 0 || !facts[0].Usage.Found || facts[0].Usage.Input != record.InputTokens || facts[0].Usage.Output != record.OutputTokens {
								t.Fatalf("Web protocol lost actual WS identity or usage: %+v audit=%+v", facts, record)
							}
						}
						if wantCode != "" {
							if (operation != "messages" && !strings.Contains(output.Body.String(), wantCode)) || !strings.Contains(output.Body.String(), "error") || record.DeliveryOutcome == "completed" {
								t.Fatalf("failure missing from delivery: %+v body=%s", record, output.Body.String())
							}
							if streaming {
								if strings.Contains(output.Body.String(), "[DONE]") || strings.Contains(output.Body.String(), `"type":"message_stop"`) || strings.Contains(output.Body.String(), `"type":"response.completed"`) {
									t.Fatalf("success leaked before required commit: %s", output.Body.String())
								}
							} else if output.Code != 502 || record.StatusCode != 502 || record.UpstreamStatusCode != 200 {
								t.Fatalf("JSON failure status/output mismatch: %+v status=%d", record, output.Code)
							}
						} else if output.Code != 200 || record.DeliveryOutcome != "completed" || !strings.Contains(output.Body.String(), "completion answer") {
							t.Fatalf("successful completion unavailable: %+v status=%d body=%s", record, output.Code, output.Body.String())
						}
						if owner == "committed" {
							if _, err := states.Get(ctx, record.ResponseID, created.Key.ID, time.Now()); err != nil {
								t.Fatalf("missing durable ownership: %v", err)
							}
							if states.ownershipCalls.Load() != 1 {
								t.Fatal("duplicate ownership save")
							}
						}
						if state == "committed" {
							if _, err := states.GetWebState(ctx, record.ResponseID, time.Now()); err != nil {
								t.Fatalf("missing native state: %v", err)
							}
						}
						if history == "committed" && journal.calls.Load() != 1 {
							t.Fatal("history was not committed once")
						}
					})
				}
			}
		}
	}
}

func completionHTTPUpstream(t *testing.T, model string, generated *atomic.Int32) *httptest.Server {
	return completionHTTPUpstreamWithResponse(t, model, generated, nil)
}

func completionHTTPUpstreamWithResponse(t *testing.T, model string, generated *atomic.Int32, mutate func(int32, map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		call := generated.Add(1)
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
			return
		}
		answer := map[string]any{"id": "resp_completion", "object": "response", "status": "completed", "model": model, "output": []any{map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "completion thought"}}}, map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "completion answer"}}}}, "usage": map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25, "output_tokens_details": map[string]any{"reasoning_tokens": 2}}}
		if mutate != nil {
			mutate(call, answer)
		}
		suppressThinking := answer["suppress_thinking"] == true
		delete(answer, "suppress_thinking")
		if payload["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []any{
				map[string]any{"type": "response.created", "response": map[string]any{"id": answer["id"], "model": model}},
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

func completionWebUpstream(t *testing.T, generated *atomic.Int32) *fhttptest.Server {
	return completionWebUpstreamWithExtra(t, generated, nil)
}

func completionWebUpstreamWithExtra(t *testing.T, generated *atomic.Int32, extra []map[string]any) *fhttptest.Server {
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
		generated.Add(1)
		for _, chunk := range []map[string]any{{"text": "completion thought", "channel": "CHANNEL_ASSISTANT_ANALYSIS"}, {"text": "completion answer", "channel": "CHANNEL_ASSISTANT_RESPONSE"}} {
			_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": chunk}}})
		}
		for _, event := range extra {
			_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": event})
		}
		_ = conn.WriteJSON(map[string]any{"session_id": "conv_completion", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent_completion", "status": "completed"}}})
	}))
}
