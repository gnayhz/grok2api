package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

func TestApplicationQualityReceiptAndIncidentConsumer(t *testing.T) {
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, clean := range []bool{true, false} {
			for _, streaming := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/clean=%v/stream=%v", operation, clean, streaming), func(t *testing.T) {
					ctx := context.Background()
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/v1/responses" {
							http.Error(w, "unused fixture route", 404)
							return
						}
						calls.Add(1)
						var payload map[string]any
						if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
							t.Error(err)
							return
						}
						if payload["stream"] != true {
							output := []any{}
							if clean {
								output = append(output, map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "calculate"}}})
							}
							output = append(output, map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "This is the calculated answer."}}})
							w.Header().Set("Content-Type", "application/json")
							json.NewEncoder(w).Encode(map[string]any{"id": "receipt-response", "object": "response", "status": "completed", "model": "grok-4.6", "output": output, "usage": map[string]any{"input_tokens": 20, "output_tokens": 64}})
							return
						}
						w.Header().Set("Content-Type", "text/event-stream")
						io.WriteString(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"receipt-response\",\"status\":\"in_progress\"}}\n\n")
						if clean {
							io.WriteString(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"calculate\"}\n\n")
						}
						io.WriteString(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"This is the calculated answer.\"}\n\n")
						io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"receipt-response\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"This is the calculated answer.\"}]}],\"usage\":{\"input_tokens\":20,\"output_tokens\":64}}}\n\n")
					}))
					defer upstream.Close()
					a := newLifecycleApplication(t, func(cfg *config.Config) {
						cfg.Provider.Build.BaseURL = upstream.URL + "/v1"
						cfg.Provider.Build.FallbackBaseURL = "disabled"
						cfg.Provider.Web.BaseURL = upstream.URL
						cfg.Provider.Console.BaseURL = upstream.URL
						cfg.Routing.MaxAttempts = 1
						cfg.RequestRetry.Enabled = true
						cfg.RequestRetry.MaxAttempts = 1
					})
					cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
					if err != nil {
						t.Fatal(err)
					}
					encrypted, err := cipher.Encrypt("synthetic-receipt-token")
					if err != nil {
						t.Fatal(err)
					}
					credential, _, err := a.accountRepo.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "receipt", SourceKey: "receipt", EncryptedAccessToken: encrypted, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
					if err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Discover(ctx, a.modelRepo, account.ProviderBuild, []string{"grok-4.6"}); err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Capabilities(ctx, a.modelRepo, a.accountRepo, credential.ID, []string{"grok-4.6"}, time.Now()); err != nil {
						t.Fatal(err)
					}
					target, _ := url.Parse(upstream.URL)
					proxy := httptest.NewServer(httputil.NewSingleHostReverseProxy(target))
					defer proxy.Close()
					encryptedProxy, err := cipher.Encrypt(proxy.URL)
					if err != nil {
						t.Fatal(err)
					}
					node, err := relational.NewEgressRepository(a.database).CreateEgressNode(ctx, egressdomain.Node{Name: "receipt-proxy", Enabled: true, Health: 1, EncryptedProxyURL: encryptedProxy})
					if err != nil {
						t.Fatal(err)
					}
					if _, _, err := a.quality.AdvanceEpoch(ctx, node.ID, model.ExitIdentityFromAggregate("198.51.100.1")); err != nil {
						t.Fatal(err)
					}
					key, err := a.clientKeys.Create(ctx, clientkeyapp.CreateInput{Name: "receipt", Enabled: true, BillingLimitUSDTicks: 100000000000})
					if err != nil {
						t.Fatal(err)
					}
					a.startup.setPhase("running")
					server := httptest.NewServer(a.server.Handler)
					defer server.Close()
					payload := map[string]any{"model": "grok-4.6", "stream": streaming}
					if operation == "responses" {
						payload["input"] = "hello"
						payload["reasoning"] = map[string]any{"effort": "high"}
					} else {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 2048
						payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
					}
					if operation == "chat/completions" {
						payload["reasoning_effort"] = "high"
					}
					raw, err := json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(raw)))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+key.Secret)
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("anthropic-version", "2023-06-01")
					response, err := http.DefaultClient.Do(request)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(response.Body)
					response.Body.Close()
					if err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 1 || (response.StatusCode == 200) != clean {
						t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), body)
					}
					var rows []journal.EventRow
					if err := a.quality.DB().Order("stage").Find(&rows).Error; err != nil {
						t.Fatal(err)
					}
					if len(rows) != 3 {
						t.Fatalf("durable stages=%+v body=%s", rows, body)
					}
					seen := map[string]string{}
					physicalID := ""
					for _, row := range rows {
						var event model.Event
						if err := json.Unmarshal([]byte(row.Payload), &event); err != nil {
							t.Fatal(err)
						}
						seen[row.Stage] = row.Outcome
						if event.Attempt.AccountID != credential.ID || event.Attempt.Path.NodeID != node.ID || event.Attempt.Path.Epoch != 1 || event.Attempt.ID == "" {
							t.Fatalf("identity=%+v", event.Attempt)
						}
						if physicalID != "" && physicalID != event.Attempt.ID {
							t.Fatalf("stage identity split %+v", rows)
						}
						physicalID = event.Attempt.ID
					}
					wantAdmission, wantCompletion := "delivered", "completed"
					if !clean {
						wantAdmission, wantCompletion = "degraded", "interrupted"
					}
					if seen["admission"] != wantAdmission || seen["completion"] != wantCompletion || seen["exchange"] != "observed" {
						t.Fatalf("stages=%v", seen)
					}
					if cases, err := a.quality.ListOpenCases(ctx); err != nil || len(cases) != 0 {
						t.Fatalf("request synchronously ran Court: %v %v", cases, err)
					}
					allowed, err := a.qualityJournal.AccountAllowed(ctx, credential.ID, time.Now().UTC())
					if err != nil || allowed != clean {
						t.Fatalf("receipt admission=%v err=%v", allowed, err)
					}
					workerCtx, cancel := context.WithCancel(ctx)
					done := make(chan error, 1)
					go func() { done <- a.qualityEvents.Run(workerCtx) }()
					deadline := time.Now().Add(3 * time.Second)
					for {
						stats, err := a.qualityEvents.Backlog(ctx)
						if err != nil {
							cancel()
							t.Fatal(err)
						}
						if stats.Pending == 0 {
							break
						}
						if time.Now().After(deadline) {
							cancel()
							t.Fatal("event did not complete")
						}
						time.Sleep(time.Millisecond)
					}
					cancel()
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("event worker did not stop")
					}
					count, err := a.qualityEvidence.Count(ctx)
					want := int64(0)
					if !clean {
						want = 1
					}
					if err != nil || count != want {
						t.Fatalf("evidence count=%d want=%d err=%v", count, want, err)
					}
					cases, err := a.quality.ListOpenCases(ctx)
					if err != nil || int64(len(cases)) != want {
						t.Fatalf("cases=%v err=%v", cases, err)
					}
					if !clean && (a.quality.AccountState(credential.ID).State != model.AccountRemanded || a.quality.ExitStateOfCurrentEpoch(node.ID).State != model.ExitRemanded) {
						t.Fatal("durable incident protection did not replace receipt hold")
					}
					if err := a.Close(); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}
