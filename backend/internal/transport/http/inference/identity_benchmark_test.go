package inference

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func BenchmarkIdentityMigrationPipeline(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []string{"raw", "claude_empty", "claude_old", "claude_current"} {
			b.Run(dialect+"/"+kind, func(b *testing.B) {
				logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
				ctx := context.Background()
				db := compactionDatabase(b, dialect)
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					b.Fatal(err)
				}
				var calls, restoredCalls, warningCalls atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					wire, _ := io.ReadAll(r.Body)
					if strings.Contains(string(wire), `"encrypted_content":`) {
						restoredCalls.Add(1)
					}
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
				manager := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
				manager.SetLogger(logger)
				b.Cleanup(func() { _ = manager.Close(context.Background()) })
				build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
				build.SetEgress(manager)
				build.SetLogger(logger)
				replay := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, logger)
				replay.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
				build.SetReasoningReplay(replay)
				registry := providerimpl.NewRegistry(build)
				sticky := memory.NewStickyStore()
				concurrency := memory.NewConcurrencyLimiter()
				maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
				clients := clientkeyapp.NewService("compaction-cost", keys, memory.NewRateLimiter(), concurrency, 100000, 4, cipher, security.RandomTokenSource{})
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
				selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
				service := gateway.NewService(models, audits, maintenance, clients, registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
				service.SetLogger(logger)
				router := gin.New()
				router.Use(middleware.RequestID(nil), middleware.ClientAuth(clients))
				NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
				server := httptest.NewServer(router)
				b.Cleanup(server.Close)

				opening := "one"
				if kind == "claude_old" || kind == "claude_current" {
					digest := sha256.Sum256([]byte(fmt.Sprintf("grok2api:build-replay:v5:%d:grok_build:grok-4.5:claude:cost:agent:main", key.Key.ID)))
					seed := hex.EncodeToString(digest[:])
					if kind == "claude_current" {
						var resolver historyapp.IdentityResolver
						identity := resolver.Resolve(historyapp.NewIdentityRequest(key.Key.ID, historydomain.ClientSignals{ClaudeSession: "cost"}, "", "cost", "cost", nil), historyapp.IdentityTarget{Provider: string(account.ProviderBuild), Model: "grok-4.5"}, historyapp.Identity{})
						seed = identity.ReplayKey
					}
					scope := historydomain.ReplayScope(seed, c.ID, historydomain.ReplayPlaneBuild)
					_, prepared, err := replay.Prepare(ctx, "grok-4.5", scope, []byte(`{"input":[{"role":"user","content":"one"}]}`))
					if err != nil {
						b.Fatal(err)
					}
					opaqueBytes := make([]byte, 256)
					for i := range opaqueBytes {
						opaqueBytes[i] = byte(i)
					}
					opaque := base64.RawStdEncoding.EncodeToString(opaqueBytes)
					answer := fmt.Sprintf(`{"id":"seed","status":"completed","output":[{"type":"reasoning","encrypted_content":%q,"summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic"}]}]}`, opaque)
					captured, commit, discard := prepared.Capture(io.NopCloser(strings.NewReader(answer)), false)
					if _, err := io.Copy(io.Discard, captured); err != nil {
						b.Fatal(err)
					}
					_ = captured.Close()
					if err := commit(); err != nil {
						b.Fatal(err)
					}
					discard()
				}
				body := `{"model":"grok-4.5","input":[{"role":"user","content":"` + opening + `"},{"role":"assistant","content":"synthetic"},{"role":"user","content":"` + strings.Repeat("synthetic text ", 2048) + `"}],"tool_choice":"none"}`

				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
					if err != nil {
						b.Fatal(err)
					}
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("Authorization", "Bearer "+key.Secret)
					if kind == "raw" {
						req.Header.Set("X-Session-Id", fmt.Sprintf("cost-%d", i))
					} else if kind == "claude_empty" {
						req.Header.Set("X-Claude-Code-Session-Id", fmt.Sprintf("cost-%d", i))
					} else {
						req.Header.Set("X-Claude-Code-Session-Id", "cost")
					}
					res, err := server.Client().Do(req)
					if err != nil {
						b.Fatal(err)
					}
					_, readErr := io.Copy(io.Discard, res.Body)
					_ = res.Body.Close()
					if readErr != nil || res.StatusCode != 200 {
						b.Fatalf("HTTP status=%d err=%v", res.StatusCode, readErr)
					}
					if strings.Contains(res.Header.Get("X-Grok2API-Compatibility-Warnings"), "history_identity_context_unavailable") {
						warningCalls.Add(1)
					}

				}
				b.StopTimer()
				b.ReportMetric(float64(restoredCalls.Load())/float64(b.N), "opaque/call")
				b.ReportMetric(float64(warningCalls.Load())/float64(b.N), "warnings/call")
				if calls.Load() != int64(b.N) {
					b.Fatalf("physical calls=%d want=%d", calls.Load(), b.N)
				}
			})
		}
	}
}
