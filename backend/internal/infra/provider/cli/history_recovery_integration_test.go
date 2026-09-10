package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gatewayapp "github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

const rejectedOpaqueJSON = `{"error":{"message":"Could not decrypt the provided encrypted_content. Ensure the value is unmodified."}}`

func recoveryOpaque(seed byte) string {
	raw := make([]byte, 256)
	for i := range raw {
		raw[i] = byte(i) + seed
	}
	return base64.RawStdEncoding.EncodeToString(raw)
}
func recoveryAnswer(id, cipher string) string {
	return `{"id":"` + id + `","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"reasoning","encrypted_content":"` + cipher + `","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`
}
func seedRecoveryHistory(t *testing.T, replay *historyapp.ReasoningReplay, model, key string) {
	t.Helper()
	_, prepared, err := replay.Prepare(t.Context(), model, key, []byte(`{"input":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	captured, commit, discard := prepared.Capture(io.NopCloser(strings.NewReader(recoveryAnswer("seed", recoveryOpaque(0)))), false)
	defer discard()
	_, err = io.Copy(io.Discard, captured)
	_ = captured.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = commit(); err != nil {
		t.Fatal(err)
	}
}
func newRecoveryJournalAdapter(t *testing.T, url string, managed bool) (*Adapter, *historyapp.ReasoningReplay, provider.ResponseResourceRequest) {
	t.Helper()
	db, err := relational.OpenSQLite(t.Context(), filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(t.Context()); err != nil {
		t.Fatal(err)
	}
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-token")
	if err != nil {
		t.Fatal(err)
	}
	replay := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, nil)
	replay.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
	adapter := NewAdapter(Config{BaseURL: url + "/v1"}, cipher)
	adapter.SetReasoningReplay(replay)
	t.Cleanup(func() { adapter.base.current.Load().CloseIdleConnections() })
	if managed {
		manager := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
		adapter.SetEgress(manager)
		t.Cleanup(func() { _ = manager.Close(context.Background()) })
	}
	request := provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: token}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5", PromptCacheKey: "upstream-hint", ReasoningReplayKey: "history-scope", NormalizeBody: true, DeferOutputCommit: true}
	key := adapter.scopedReasoningReplayKey(request, url+"/v1")
	seedRecoveryHistory(t, replay, request.Model, key)
	return adapter, replay, request
}
func recoveryInput(operation string, stream bool) []byte {
	history := []any{map[string]any{"role": "user", "content": "hello"}, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next"}}
	payload := map[string]any{"model": "grok-4.5", "stream": stream}
	if operation == "responses" {
		payload["input"] = history
	} else {
		payload["messages"] = history
	}
	if operation == "messages" {
		payload["max_tokens"] = 128
	}
	data, _ := json.Marshal(payload)
	return data
}

func TestHistoryRecoveryAuthorityRealHTTPAndJournal(t *testing.T) {
	cases := []struct {
		name       string
		limit      int
		mode       historydomain.RecoveryMode
		disable    bool
		statuses   []int
		first      string
		calls      int
		generation int64
		want       int
	}{
		{"opaque recovery", 2, historydomain.AllowLossyRecovery, false, []int{400, 200}, rejectedOpaqueJSON, 2, 2, 200},
		{"no capacity", 1, historydomain.AllowLossyRecovery, false, []int{400}, rejectedOpaqueJSON, 1, 1, 400},
		{"preserve opaque", 3, historydomain.PreserveOpaque, false, []int{400}, rejectedOpaqueJSON, 1, 1, 400},
		{"no controller", 3, historydomain.AllowLossyRecovery, true, []int{400}, rejectedOpaqueJSON, 1, 1, 400},
		{"unrelated 400", 3, historydomain.AllowLossyRecovery, false, []int{400}, `{"error":"invalid request"}`, 1, 1, 400},
		{"echo is not rejection", 3, historydomain.AllowLossyRecovery, false, []int{400}, `{"error":"invalid request","input":"Could not decrypt the provided encrypted_content"}`, 1, 1, 400},
		{"recovery rate limit", 3, historydomain.AllowLossyRecovery, false, []int{400, 429}, rejectedOpaqueJSON, 2, 2, 429},
		{"recovery other failure", 3, historydomain.AllowLossyRecovery, false, []int{400, 503}, rejectedOpaqueJSON, 2, 2, 400},
		{"session hint recovery", 3, historydomain.AllowLossyRecovery, false, []int{400, 400, 200}, rejectedOpaqueJSON, 3, 2, 200},
		{"second step no capacity", 2, historydomain.AllowLossyRecovery, false, []int{400, 400}, rejectedOpaqueJSON, 2, 2, 400},
	}
	for _, managed := range []bool{false, true} {
		for _, operation := range []string{"responses", "chat", "messages"} {
			for _, stream := range []bool{false, true} {
				for _, tc := range cases {
					t.Run(fmt.Sprintf("managed=%t/%s/stream=%t/%s", managed, operation, stream, tc.name), func(t *testing.T) {
						var mu sync.Mutex
						var sent []string
						var hints []string
						upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							data, _ := io.ReadAll(r.Body)
							mu.Lock()
							sent = append(sent, string(data))
							hints = append(hints, r.Header.Get("x-grok-session-id"))
							index := len(sent) - 1
							mu.Unlock()
							if index >= len(tc.statuses) {
								t.Errorf("unauthorized request %d", index+1)
								w.WriteHeader(500)
								return
							}
							status := tc.statuses[index]
							if status != 200 {
								w.Header().Set("Content-Type", "application/json")
								w.WriteHeader(status)
								if index == 0 {
									_, _ = io.WriteString(w, tc.first)
								} else {
									_, _ = io.WriteString(w, rejectedOpaqueJSON)
								}
								return
							}
							answer := recoveryAnswer("recovered", recoveryOpaque(5))
							if stream {
								w.Header().Set("Content-Type", "text/event-stream")
								fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", answer)
							} else {
								w.Header().Set("Content-Type", "application/json")
								_, _ = io.WriteString(w, answer)
							}
						}))
						defer upstream.Close()
						adapter, replay, request := newRecoveryJournalAdapter(t, upstream.URL, managed)
						request.Operation = operation
						request.Streaming = stream
						request.Body = recoveryInput(operation, stream)
						budget := inferencedomain.NewAttemptBudget(tc.limit)
						ctx := attemptmeta.WithRequest(t.Context(), "history-recovery", 0, "", nil)
						ctx = attemptmeta.WithAccount(ctx, 1, string(account.ProviderBuild), request.Model)
						ctx = infraegress.WithPhysicalCallTrace(ctx, string(account.ProviderBuild), operation)
						ctx = infraegress.WithPhysicalCallBudget(ctx, budget)
						if !tc.disable {
							request.HistoryControl = gatewayapp.NewHistoryController(tc.mode, budget)
						}
						response, err := adapter.ForwardResponse(ctx, request)
						if err != nil {
							t.Fatal(err)
						}
						_, err = io.Copy(io.Discard, response.Body)
						_ = response.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						if response.CommitOutput != nil {
							if err = response.CommitOutput(); err != nil {
								t.Fatal(err)
							}
						}
						if response.DiscardOutput != nil {
							response.DiscardOutput()
						}
						mu.Lock()
						defer mu.Unlock()
						if response.StatusCode != tc.want || len(sent) != tc.calls {
							t.Fatalf("status=%d calls=%d want=%d/%d", response.StatusCode, len(sent), tc.want, tc.calls)
						}
						facts := infraegress.PhysicalFacts(ctx)
						if len(facts) != tc.calls || budget.Remaining() != tc.limit-tc.calls {
							t.Fatalf("facts=%d remaining=%d", len(facts), budget.Remaining())
						}
						if !strings.Contains(sent[0], recoveryOpaque(0)) {
							t.Fatal("initial durable opaque missing")
						}
						if len(sent) > 1 && strings.Contains(sent[1], recoveryOpaque(0)) {
							t.Fatal("approved recovery restored rejected opaque")
						}
						if len(sent) > 2 && (hints[2] != "" || strings.Contains(sent[2], `"prompt_cache_key"`)) {
							t.Fatal("session hint not cleared")
						}
						key := adapter.scopedReasoningReplayKey(request, upstream.URL+"/v1")
						probe, prepared, err := replay.Prepare(t.Context(), request.Model, key, recoveryInput("responses", false))
						if err != nil {
							t.Fatal(err)
						}
						defer prepared.Discard()
						if prepared.Generation() != tc.generation {
							t.Fatalf("generation=%d want=%d", prepared.Generation(), tc.generation)
						}
						if tc.generation == 1 && !strings.Contains(string(probe), recoveryOpaque(0)) {
							t.Fatal("denied recovery changed durable history")
						}
					})
				}
			}
		}
	}
}

func TestConcurrentRecoveryCannotResetNewGeneration(t *testing.T) {
	var mu sync.Mutex
	originalCalls, recoveryCalls := 0, 0
	gate := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		if strings.Contains(string(data), recoveryOpaque(0)) {
			mu.Lock()
			originalCalls++
			if originalCalls == 2 {
				close(gate)
			}
			mu.Unlock()
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(400)
			_, _ = io.WriteString(w, rejectedOpaqueJSON)
			return
		}
		mu.Lock()
		recoveryCalls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, recoveryAnswer("winner", recoveryOpaque(5)))
	}))
	defer upstream.Close()
	adapter, replay, baseRequest := newRecoveryJournalAdapter(t, upstream.URL, true)
	baseRequest.Operation = "responses"
	baseRequest.Body = recoveryInput("responses", false)
	type result struct {
		status int
		err    error
		calls  int
	}
	results := make(chan result, 2)
	for writer := range 2 {
		go func() {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			budget := inferencedomain.NewAttemptBudget(2)
			ctx = attemptmeta.WithRequest(ctx, fmt.Sprintf("writer-%d", writer), 0, "", nil)
			ctx = infraegress.WithPhysicalCallBudget(infraegress.WithPhysicalCallTrace(ctx, string(account.ProviderBuild), "responses"), budget)
			request := baseRequest
			// Different transport hints permit concurrent HTTP/1 calls while retaining the same durable history scope.
			request.PromptCacheKey = fmt.Sprintf("writer-%d", writer)
			request.HistoryControl = gatewayapp.NewHistoryController(historydomain.AllowLossyRecovery, budget)
			response, err := adapter.ForwardResponse(ctx, request)
			status := 0
			if response != nil {
				status = response.StatusCode
				_, readErr := io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if err == nil {
					err = readErr
				}
				if response.CommitOutput != nil && err == nil {
					err = response.CommitOutput()
				}
				if response.DiscardOutput != nil {
					response.DiscardOutput()
				}
			}
			results <- result{status, err, len(infraegress.PhysicalFacts(ctx))}
		}()
	}
	statuses := map[int]int{}
	facts := 0
	for range 2 {
		r := <-results
		if r.err != nil {
			t.Fatal(r.err)
		}
		statuses[r.status]++
		facts += r.calls
	}
	mu.Lock()
	defer mu.Unlock()
	if statuses[200] != 1 || statuses[400] != 1 || originalCalls != 2 || recoveryCalls != 1 || facts != 3 {
		t.Fatalf("statuses=%v initial=%d recovery=%d facts=%d", statuses, originalCalls, recoveryCalls, facts)
	}
	body := []byte(`{"input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"},{"role":"assistant","content":"answer"},{"role":"user","content":"again"}]}`)
	key := adapter.scopedReasoningReplayKey(baseRequest, upstream.URL+"/v1")
	restored, prepared, err := replay.Prepare(t.Context(), baseRequest.Model, key, body)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Discard()
	if prepared.Generation() != 2 || !strings.Contains(string(restored), recoveryOpaque(5)) || strings.Contains(string(restored), recoveryOpaque(0)) {
		t.Fatal("stale recovery erased winner history or restored rejected opaque")
	}
}

func TestRecoveryCancellationStoredParentAndVetoLeaveHistoryIntact(t *testing.T) {
	for _, scenario := range []string{"canceled before authorization", "stored parent with opaque", "automatic replay veto"} {
		t.Run(scenario, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				mu.Lock()
				calls++
				mu.Unlock()
				w.WriteHeader(400)
				_, _ = io.WriteString(w, rejectedOpaqueJSON)
			}))
			defer upstream.Close()
			adapter, replay, request := newRecoveryJournalAdapter(t, upstream.URL, true)
			request.Operation = "responses"
			request.Body = recoveryInput("responses", false)
			request.DisableAutomaticReplay = scenario == "automatic replay veto"
			if scenario == "stored parent with opaque" {
				request.Body = []byte(`{"model":"grok-4.5","previous_response_id":"seed","input":[{"type":"reasoning","encrypted_content":"` + recoveryOpaque(0) + `","summary":[]},{"role":"user","content":"next"}]}`)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			budget := inferencedomain.NewAttemptBudget(3)
			ctx = infraegress.WithPhysicalCallBudget(infraegress.WithPhysicalCallTrace(ctx, string(account.ProviderBuild), "responses"), budget)
			controller := gatewayapp.NewHistoryController(historydomain.AllowLossyRecovery, budget)
			request.HistoryControl = observedRecoveryController{HistoryController: controller, before: func() {
				if scenario == "canceled before authorization" {
					cancel()
				}
			}}
			response, err := adapter.ForwardResponse(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			mu.Lock()
			count := calls
			mu.Unlock()
			if count != 1 || response.StatusCode != 400 || budget.Remaining() != 2 {
				t.Fatalf("calls=%d status=%d remaining=%d", count, response.StatusCode, budget.Remaining())
			}
			key := adapter.scopedReasoningReplayKey(request, upstream.URL+"/v1")
			restored, prepared, err := replay.Prepare(t.Context(), request.Model, key, recoveryInput("responses", false))
			if err != nil {
				t.Fatal(err)
			}
			defer prepared.Discard()
			if prepared.Generation() != 1 || !strings.Contains(string(restored), recoveryOpaque(0)) {
				t.Fatal("unapproved recovery changed durable state")
			}
		})
	}
}

func TestHistoryRecoveryBudgetIncludesPlaneFallback(t *testing.T) {
	for _, limit := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/build/responses" {
					w.WriteHeader(403)
					_, _ = io.WriteString(w, `{"error":"build denied"}`)
					return
				}
				data, _ := io.ReadAll(r.Body)
				if strings.Contains(string(data), `"encrypted_content"`) {
					w.WriteHeader(400)
					_, _ = io.WriteString(w, rejectedOpaqueJSON)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, recoveryAnswer("fallback", recoveryOpaque(5)))
			}))
			defer upstream.Close()
			adapter, token := newReasoningRecoveryTestAdapter(t)
			cfg := adapter.config()
			cfg.BaseURL = upstream.URL + "/build"
			cfg.FallbackBaseURL = upstream.URL + "/xai"
			adapter.UpdateConfig(cfg)
			adapter.SetFallbackMarker(reasoningRecoveryFallbackMarker{})
			t.Cleanup(func() { adapter.base.current.Load().CloseIdleConnections() })
			budget := inferencedomain.NewAttemptBudget(limit)
			ctx := attemptmeta.WithRequest(t.Context(), "plane-budget", 0, "", nil)
			ctx = infraegress.WithPhysicalCallBudget(infraegress.WithPhysicalCallTrace(ctx, string(account.ProviderBuild), "responses"), budget)
			response, err := adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: token, BuildRouteMode: account.BuildRouteAuto, BuildSuperEntitled: true}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5", Body: []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","encrypted_content":"opaque","summary":[]},{"role":"user","content":"next"}]}`), HistoryControl: gatewayapp.NewHistoryController(historydomain.AllowLossyRecovery, budget)})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			want := 403
			if limit == 3 {
				want = 200
			}
			if response.StatusCode != want || int(calls.Load()) != limit || len(infraegress.PhysicalFacts(ctx)) != limit {
				t.Fatalf("status=%d calls=%d facts=%d", response.StatusCode, calls.Load(), len(infraegress.PhysicalFacts(ctx)))
			}
		})
	}
}

func TestHistoryBudgetIncludesCompactionRetries(t *testing.T) {
	for _, limit := range []int{1, 2} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":"temporary"}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, compactionSampleSSE("compact", healthyCompactionSummary()))
			}))
			defer upstream.Close()
			adapter, token := newReasoningRecoveryTestAdapter(t)
			cfg := adapter.config()
			cfg.BaseURL = upstream.URL + "/v1"
			adapter.UpdateConfig(cfg)
			t.Cleanup(func() { adapter.base.current.Load().CloseIdleConnections() })
			budget := inferencedomain.NewAttemptBudget(limit)
			ctx := attemptmeta.WithRequest(t.Context(), "compaction-budget", 0, "", nil)
			ctx = infraegress.WithPhysicalCallBudget(infraegress.WithPhysicalCallTrace(ctx, string(account.ProviderBuild), "compaction"), budget)
			request := compactionProviderRequest(token)
			response, err := adapter.forwardGatewayCompactionWithPolicy(ctx, request, "access-token", request.Body, "", 3, 0)
			if limit == 1 {
				if !errors.Is(err, inferencedomain.ErrAttemptBudget) {
					t.Fatalf("exhaustion err=%v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != 200 {
					t.Fatalf("status=%d", response.StatusCode)
				}
			}
			if int(calls.Load()) != limit || len(infraegress.PhysicalFacts(ctx)) != limit {
				t.Fatalf("calls=%d facts=%d", calls.Load(), len(infraegress.PhysicalFacts(ctx)))
			}
		})
	}
}

type observedRecoveryController struct {
	provider.HistoryController
	before func()
}

func (c observedRecoveryController) Recover(ctx context.Context, proposal provider.HistoryRecoveryRequest) provider.HistoryRecoveryResult {
	c.before()
	return c.HistoryController.Recover(ctx, proposal)
}
