package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestHTTPModelRestrictionsUseActualAttemptGeneration(t *testing.T) {
	for _, scenario := range []string{"current_quota", "quota_after_reset", "current_denial", "denial_after_reimport"} {
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", scenario, operation, stream), func(t *testing.T) {
					ctx := context.Background()
					now := time.Now().UTC()
					db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "http-model.db"))
					if err != nil {
						t.Fatal(err)
					}
					defer db.Close()
					if err := db.InitializeSchema(ctx); err != nil {
						t.Fatal(err)
					}
					cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
					token, _ := cipher.Encrypt("model-token")
					accounts := relational.NewAccountRepository(db)
					models := relational.NewModelRepository(db)
					audits := relational.NewAuditRepository(db)
					v, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "model", SourceKey: "model", EncryptedAccessToken: token, ExpiresAt: now.Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 1})
					if err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Discover(ctx, models, v.Provider, []string{"grok-4.5"}); err != nil {
						t.Fatal(err)
					}
					if err := testsupport.Capabilities(ctx, models, accounts, v.ID, []string{"grok-4.5"}, now); err != nil {
						t.Fatal(err)
					}
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if scenario == "quota_after_reset" {
							if err := accounts.ResetQuotaState(ctx, v.Provider, []uint64{v.ID}); err != nil {
								t.Error(err)
							}
						}
						if scenario == "denial_after_reimport" {
							fresh := v
							fresh.EncryptedAccessToken, _ = cipher.Encrypt("replacement-token")
							if _, _, err := accounts.UpsertByIdentity(ctx, fresh); err != nil {
								t.Error(err)
							}
						}
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusForbidden)
						if strings.Contains(scenario, "quota") {
							fmt.Fprint(w, `{"error":"You've used all the included free usage for model grok-4.5"}`)
						} else {
							fmt.Fprint(w, `{"error":"Access to the chat endpoint is denied"}`)
						}
					}))
					defer upstream.Close()
					registry := provider.NewRegistry(cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher))
					concurrency := memory.NewConcurrencyLimiter()
					sticky := memory.NewStickyStore()
					accountService := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
					clientService := clientkeyapp.NewService("model-owner", relational.NewClientKeyRepository(db), memory.NewRateLimiter(), concurrency, 120, 4, cipher)
					defer closeClientKeyService(t, clientService)
					key, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "model", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					selector := gateway.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
					accounts.SetInvalidationObserver(func(_ context.Context, e repository.InvalidationEvent) { selector.ApplyInvalidation(e) })
					service := gateway.NewService(models, audits, accountService, clientService, registry, selector, relational.NewResponseRepository(db), 2)
					gin.SetMode(gin.TestMode)
					router := gin.New()
					router.Use(middleware.RequestID(), middleware.ClientAuth(clientService))
					NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
					server := httptest.NewServer(router)
					defer server.Close()
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
					request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					request.Header.Set("Authorization", "Bearer "+key.Secret)
					request.Header.Set("Content-Type", "application/json")
					if operation == "messages" {
						request.Header.Set("anthropic-version", "2023-06-01")
					}
					response, err := server.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					data, readErr := io.ReadAll(response.Body)
					response.Body.Close()
					if readErr != nil {
						t.Fatal(readErr)
					}
					if calls.Load() != 1 {
						t.Fatalf("physical attempts=%d status=%d body=%s", calls.Load(), response.StatusCode, data)
					}
					if response.StatusCode < 400 && !strings.Contains(string(data), "error") {
						t.Fatalf("denial became success: status=%d body=%s", response.StatusCode, data)
					}
					candidates, err := accounts.ListRoutingCandidates(ctx, v.Provider, 0, "grok-4.5", "")
					if err != nil || len(candidates) != 1 {
						t.Fatalf("candidates=%d err=%v", len(candidates), err)
					}
					wantBlock := strings.HasPrefix(scenario, "current_")
					block := candidates[0].ModelQuotaBlock
					if (block != nil) != wantBlock {
						t.Fatalf("block=%+v scenario=%s", block, scenario)
					}
					if wantBlock {
						reason := "model_access_denied"
						if scenario == "current_quota" {
							reason = "model_quota_depleted"
						}
						if block.Reason != reason {
							t.Fatalf("restriction reason=%s", block.Reason)
						}
					}
					lease, err := selector.Acquire(ctx, v.Provider, 0, "grok-4.5", "", "", nil, false)
					if err == nil {
						lease.Release()
					}
					if (err != nil) != wantBlock {
						t.Fatalf("cached selector block=%t want=%t err=%v", err != nil, wantBlock, err)
					}
					current, err := accounts.Get(ctx, v.ID)
					if err != nil || current.AuthStatus != account.AuthStatusActive || current.FailureCount != 0 || current.CooldownUntil != nil {
						t.Fatalf("model failure changed another dimension: %+v err=%v", current, err)
					}
					slot, ok, err := concurrency.Acquire(ctx, repository.AccountConcurrencyKey(v.ID), 1)
					if err != nil || !ok {
						t.Fatalf("leaked account slot: %v", err)
					}
					slot()
				})
			}
		}
	}
}
