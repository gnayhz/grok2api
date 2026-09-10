package inference

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func BenchmarkCompactionInputPipeline(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"plain", "owned", "foreign"} {
			b.Run(dialect+"/"+kind, func(b *testing.B) {
				logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
				ctx := context.Background()
				db := compactionDatabase(b, dialect)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					b.Fatal(err)
				}
				var calls atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.Copy(io.Discard, r.Body)
					index := calls.Add(1)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"id":"resp_compaction_cost_%d","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, index)
				}))
				b.Cleanup(upstream.Close)
				accounts := relational.NewAccountRepository(db)
				models := relational.NewModelRepository(db)
				audits := relational.NewAuditRepository(db)
				keys := relational.NewClientKeyRepository(db)
				token, err := cipher.Encrypt("synthetic")
				if err != nil {
					b.Fatal(err)
				}
				c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: "cost", SourceKey: "cost", EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), AuthStatus: account.AuthStatusActive, MaxConcurrent: 2})
				if err != nil {
					b.Fatal(err)
				}
				if err = testsupport.Capabilities(ctx, models, accounts, c.ID, []string{"grok-4.5"}, time.Now()); err != nil {
					b.Fatal(err)
				}
				if err = testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
					b.Fatal(err)
				}
				manager := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
				manager.SetLogger(logger)
				b.Cleanup(func() { _ = manager.Close(context.Background()) })
				build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
				build.SetEgress(manager)
				build.SetLogger(logger)
				replay := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, logger)
				replay.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
				build.SetReasoningReplay(replay)
				registry := provider.NewRegistry(build)
				sticky := memory.NewStickyStore()
				concurrency := memory.NewConcurrencyLimiter()
				maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
				clients := clientkeyapp.NewService("compaction-cost", keys, memory.NewRateLimiter(), concurrency, 100000, 4, cipher)
				b.Cleanup(func() {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if err := clients.Close(ctx); err != nil {
						b.Error(err)
					}
				})
				key, err := clients.Create(ctx, clientkeyapp.CreateInput{Name: "cost", Enabled: true, RPMLimit: 100000, MaxConcurrent: 4})
				if err != nil {
					b.Fatal(err)
				}
				selector := gateway.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
				service := gateway.NewService(models, audits, maintenance, clients, registry, selector, relational.NewResponseRepository(db), 2)
				service.SetLogger(logger)
				router := gin.New()
				router.Use(middleware.RequestID(), middleware.ClientAuth(clients))
				NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
				server := httptest.NewServer(router)
				b.Cleanup(server.Close)
				// Legacy-compatible v1 envelope makes this exact fixture compile and run on
				// both revisions, with the real Gateway selecting its history controller.
				prefix := ""
				if kind != "plain" {
					blob := "foreign-private-state"
					if kind == "owned" {
						encoded, err := cipher.Encrypt(`{"version":1,"session":"old","summary":"synthetic summary"}`)
						if err != nil {
							b.Fatal(err)
						}
						blob = "g2a_compact_v1." + encoded
					}
					prefix = fmt.Sprintf(`{"type":"compaction","encrypted_content":%q},`, blob)
				}
				body := `{"model":"grok-4.5","input":[` + prefix + `{"role":"user","content":"` + strings.Repeat("synthetic text ", 2048) + `"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","const":17}}}}],"tool_choice":"none"}`
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
					if err != nil {
						b.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+key.Secret)
					req.Header.Set("X-Session-Id", fmt.Sprintf("cost-%d", i))
					res, err := server.Client().Do(req)
					if err != nil {
						b.Fatal(err)
					}
					_, readErr := io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					if readErr != nil || res.StatusCode != 200 {
						b.Fatalf("HTTP status=%d err=%v", res.StatusCode, readErr)
					}
					if kind == "foreign" && !strings.Contains(res.Header.Get("X-Grok2API-Compatibility-Warnings"), "foreign_compaction_omitted") {
						b.Fatal("missing loss fact")
					}
				}
				b.StopTimer()
				if calls.Load() != int64(b.N) {
					b.Fatalf("physical calls=%d want=%d", calls.Load(), b.N)
				}
			})
		}
	}
}
