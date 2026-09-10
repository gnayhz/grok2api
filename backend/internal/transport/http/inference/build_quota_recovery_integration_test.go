package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestHTTPBuildQuotaProbeGeneration(t *testing.T) {
	for _, scenario := range []string{"free_success", "free_new_exhaustion", "paid_promoted_exhaustion", "paid_reset"} {
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", scenario, operation, stream), func(t *testing.T) {
					ctx := context.Background()
					now := time.Now().UTC().Truncate(time.Microsecond)
					db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "gateway-recovery.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if err := db.InitializeSchema(ctx); err != nil {
						t.Fatal(err)
					}
					cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
					token, _ := cipher.Encrypt("synthetic-token")
					accounts := relational.NewAccountRepository(db)
					models := relational.NewModelRepository(db)
					audits := relational.NewAuditRepository(db)
					v, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "probe", SourceKey: "probe", EncryptedAccessToken: token, ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
					if err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Discover(ctx, models, v.Provider, []string{"grok-4.5"}); err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Capabilities(ctx, models, accounts, v.ID, []string{"grok-4.5"}, now); err != nil {
						t.Fatal(err)
					}
					seed := account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: now.Add(-25 * time.Hour), Used: 100, Limit: 100}
					paid := strings.HasPrefix(scenario, "paid_")
					if paid {
						seed = account.RecoveryEvent{Kind: account.RecoveryBillingObserved, OccurredAt: now, Billing: &account.Billing{AccountID: v.ID, MonthlyLimit: 100, Used: 100, BillingPeriodEnd: now.Add(-time.Minute).Format(time.RFC3339)}}
					}
					seeded, err := accounts.ApplyQuotaRecovery(ctx, v.QuotaRecoveryRef(), seed)
					if err != nil || !seeded.Applied {
						t.Fatal(err)
					}
					var generated, billingCalls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch r.URL.Path {
						case "/v1/billing":
							billingCalls.Add(1)
							if scenario == "paid_reset" {
								if err := accounts.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
									t.Error(err)
								}
							}
							w.Header().Set("Content-Type", "application/json")
							fmt.Fprintf(w, `{"monthlyLimit":100,"used":0,"billingPeriodEnd":%q}`, now.Add(time.Hour).Format(time.RFC3339))
							return
						case "/v1/user":
							fmt.Fprint(w, `{"subscriptionTier":"SuperGrok"}`)
							return
						}
						generated.Add(1)
						if scenario == "paid_promoted_exhaustion" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusPaymentRequired)
							fmt.Fprint(w, `{"error":{"code":"personal-team-blocked:spending-limit","message":"spending limit exhausted"}}`)
							return
						}
						if scenario == "free_new_exhaustion" {
							current, err := accounts.Get(ctx, v.ID)
							if err != nil {
								t.Error(err)
								return
							}
							result, err := accounts.ApplyQuotaRecovery(ctx, current.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, Used: 200, Limit: 200})
							if err != nil || !result.Applied {
								t.Errorf("new exhaustion: %+v %v", result, err)
								return
							}
						}
						answer := map[string]any{"id": "resp_probe", "object": "response", "status": "completed", "model": "grok-4.5", "output": []any{map[string]any{"type": "message", "role": "assistant", "id": "msg_probe", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "probe generation answer"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
						if stream {
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
					registry := provider.NewRegistry(build)
					sticky := memory.NewStickyStore()
					concurrency := memory.NewConcurrencyLimiter()
					accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
					clientService := clientkeyapp.NewService("quota-owner", relational.NewClientKeyRepository(db), memory.NewRateLimiter(), concurrency, 120, 4, cipher)
					defer closeClientKeyService(t, clientService)
					key, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "quota", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					selector := gateway.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
					service := gateway.NewService(models, audits, accountService, clientService, registry, selector, relational.NewResponseRepository(db), 2)
					gin.SetMode(gin.TestMode)
					router := gin.New()
					router.Use(middleware.RequestID(), middleware.ClientAuth(clientService))
					NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
					payload := map[string]any{"model": "grok-4.5", "stream": stream}
					if operation == "responses" {
						payload["input"] = "hello"
					} else {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 64
					}
					body, _ := json.Marshal(payload)
					request := httptest.NewRequest(http.MethodPost, "/v1/"+operation, bytes.NewReader(body))
					request.Header.Set("Authorization", "Bearer "+key.Secret)
					request.Header.Set("Content-Type", "application/json")
					if operation == "messages" {
						request.Header.Set("anthropic-version", "2023-06-01")
					}
					out := httptest.NewRecorder()
					router.ServeHTTP(out, request)
					recovery, recoveryErr := accounts.GetQuotaRecovery(ctx, v.ID)
					switch scenario {
					case "free_success":
						if !errors.Is(recoveryErr, repository.ErrNotFound) {
							t.Fatalf("successful probe did not clear its revision: %+v %v", recovery, recoveryErr)
						}
					case "free_new_exhaustion":
						if recoveryErr != nil || recovery.ConfirmedUsed != 200 {
							t.Fatalf("old HTTP completion erased newer exhaustion: %+v %v", recovery, recoveryErr)
						}
					case "paid_promoted_exhaustion":
						if recoveryErr != nil || recovery.Status != account.QuotaRecoveryStatusExhausted {
							t.Fatalf("promoted request failed to record new exhaustion: %+v %v status=%d body=%s", recovery, recoveryErr, out.Code, out.Body.String())
						}
					case "paid_reset":
						if !errors.Is(recoveryErr, repository.ErrNotFound) || generated.Load() != 0 {
							t.Fatalf("obsolete Billing promoted request: generated=%d recovery=%v status=%d", generated.Load(), recoveryErr, out.Code)
						}
					}
					wantCalls := int32(1)
					if scenario == "paid_reset" {
						wantCalls = 0
					}
					if generated.Load() != wantCalls {
						t.Fatalf("model calls=%d want=%d status=%d body=%s", generated.Load(), wantCalls, out.Code, out.Body.String())
					}
					if paid && billingCalls.Load() != 1 {
						t.Fatalf("Billing calls=%d", billingCalls.Load())
					}
					if !paid && (out.Code != 200 || !strings.Contains(out.Body.String(), "probe generation answer")) {
						t.Fatalf("probe response: %d %s", out.Code, out.Body.String())
					}
					// Every acquired slot is released, including stale paid-probe promotion.
					slot, acquired, err := concurrency.Acquire(ctx, repository.AccountConcurrencyKey(v.ID), 1)
					if err != nil {
						t.Fatal(err)
					}
					if !acquired || slot == nil {
						t.Fatal("probe completion leaked account capacity")
					}
					slot()
				})
			}
		}
	}
}
