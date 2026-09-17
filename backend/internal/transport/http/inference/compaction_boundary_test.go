package inference

import (
	"context"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	providerimpl "github.com/chenyme/grok2api/backend/internal/infra/provider"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"io"
	"net/http"
	"net/http/httptest"
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

func TestHTTPCompactionPreparationPreservesFactsAcrossFailureAndRestart(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, stream := range []bool{false, true} {
			for _, scenario := range []string{"foreign_failover", "foreign_failure", "owned_restart"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", dialect, stream, scenario), func(t *testing.T) {
					var calls atomic.Int64
					var mu sync.Mutex
					var sent, auth []string
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						data, _ := io.ReadAll(r.Body)
						mu.Lock()
						sent = append(sent, string(data))
						auth = append(auth, r.Header.Get("Authorization"))
						mu.Unlock()
						index := calls.Add(1)
						if scenario == "foreign_failover" && index == 1 {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(429)
							_, _ = io.WriteString(w, `{"error":"rate limited"}`)
							return
						}
						if scenario == "foreign_failure" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(400)
							_, _ = io.WriteString(w, `{"error":"invalid synthetic input"}`)
							return
						}
						answer := fmt.Sprintf(`{"id":"resp_compaction_%d","object":"response","status":"completed","model":"grok-4.5","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"synthetic answer"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, index)
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", answer)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, answer)
						}
					}))
					t.Cleanup(upstream.Close)
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
					for _, name := range []string{"one", "two"} {
						token, err := cipher.Encrypt("synthetic-" + name)
						if err != nil {
							t.Fatal(err)
						}
						c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: name, SourceKey: name, EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2})
						if err != nil {
							t.Fatal(err)
						}
						if err = testsupport.Capabilities(ctx, models, accounts, c.ID, []string{"grok-4.5"}, time.Now()); err != nil {
							t.Fatal(err)
						}
					}
					if err = testsupport.Discover(ctx, models, account.ProviderBuild, []string{"grok-4.5"}); err != nil {
						t.Fatal(err)
					}
					egress := infraegress.NewManagerWithLimits(relational.NewEgressRepository(db), cipher, netbudget.Limits{})
					t.Cleanup(func() { _ = egress.Close(context.Background()) })
					makeBuild := func() *cli.Adapter {
						b := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
						replay := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, nil)
						replay.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
						b.SetReasoningReplay(replay)
						b.SetEgress(egress)
						return b
					}
					sticky := memory.NewStickyStore()
					concurrency := memory.NewConcurrencyLimiter()
					clientService := clientkeyapp.NewService("compaction-owner", keys, memory.NewRateLimiter(), concurrency, 120, 4, cipher, security.RandomTokenSource{})
					t.Cleanup(func() { closeClientKeyService(t, clientService) })
					created, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "compaction", Enabled: true, RPMLimit: 120, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					makeGateway := func() (*gateway.Service, *gin.Engine) {
						registry := providerimpl.NewRegistry(makeBuild())
						maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, security.RandomTokenSource{}, nil, nil, nil)
						selector := selector.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
						service := gateway.NewService(models, audits, maintenance, clientService, registry, selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 2)
						router := gin.New()
						router.Use(middleware.RequestID(nil), middleware.ClientAuth(clientService))
						NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
						return service, router
					}
					service, router := makeGateway()
					var activeRouter atomic.Pointer[gin.Engine]
					activeRouter.Store(router)
					handled := make(chan struct{}, 2)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						activeRouter.Load().ServeHTTP(w, r)
						handled <- struct{}{}
					}))
					t.Cleanup(server.Close)
					blob := "foreign-private-state"
					rounds := 1
					if scenario == "owned_restart" {
						blob, err = historydomain.NewCompactionCodec(cipher).Encode("old-cache-hint", "SYNTHETIC_SUMMARY_PRESERVED")
						if err != nil {
							t.Fatal(err)
						}
						rounds = 2
					}
					body := fmt.Sprintf(`{"model":"grok-4.5","stream":%t,"input":[{"type":"\u0063ompaction","encrypted_content":%q},{"role":"user","content":"continue"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","const":9007199254740993},"fraction":{"type":"number","minimum":0.1234567890123456789}}}}],"tool_choice":"none"}`, stream, blob)
					// An explicit strict M04 input must reject before making any physical call,
					// even though ordinary HTTP keeps the existing AllowLossyRecovery default.
					if strings.HasPrefix(scenario, "foreign") {
						mode := historydomain.PreserveOpaque
						result, err := service.CreateResponse(ctx, gateway.Input{RequestID: "strict-compaction", ClientKey: created.Key, PublicModel: "grok-4.5", PromptCacheKey: "strict", HistoryRecoveryPolicy: &mode, Body: []byte(body), Streaming: stream})
						if result != nil {
							_ = result.Body.Close()
						}
						if err == nil || calls.Load() != 0 {
							t.Fatalf("strict preparation made upstream call: count=%d result=%v err=%v", calls.Load(), result, err)
						}
					}
					for round := 0; round < rounds; round++ {
						if round == 1 {
							_, next := makeGateway()
							activeRouter.Store(next)
						} // Previous handler joined; reconstruct Gateway/Provider/history over the same SQL.
						req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
						if err != nil {
							t.Fatal(err)
						}
						req.Header.Set("Authorization", "Bearer "+created.Secret)
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("X-Session-Id", fmt.Sprintf("compaction-client-%d", round))
						res, err := server.Client().Do(req)
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
							t.Fatal("HTTP handler did not drain")
						}
						want := 200
						if scenario == "foreign_failure" {
							want = 400
						}
						if res.StatusCode != want {
							t.Fatalf("status=%d want=%d body=%s", res.StatusCode, want, data)
						}
						warnings := res.Header.Get("X-Grok2API-Compatibility-Warnings")
						if strings.HasPrefix(scenario, "foreign") && !strings.Contains(warnings, "foreign_compaction_omitted") {
							t.Fatalf("lost warning after %s: %q", scenario, warnings)
						}
						if scenario == "owned_restart" && (!strings.Contains(warnings, "compaction_session_drifted") || strings.Contains(warnings, "foreign_compaction_omitted")) {
							t.Fatalf("owned summary warning=%q", warnings)
						}
					}
					mu.Lock()
					defer mu.Unlock()
					expected := 1
					if scenario != "foreign_failure" {
						expected = 2
					}
					if len(sent) != expected {
						t.Fatalf("physical calls=%d want=%d", len(sent), expected)
					}
					if scenario == "foreign_failover" && auth[0] == auth[1] {
						t.Fatal("429 did not switch selected account")
					}
					for _, data := range sent {
						for _, number := range []string{"9007199254740993", "0.1234567890123456789"} {
							if !strings.Contains(data, number) {
								t.Fatalf("wire lost number %s: %s", number, data)
							}
						}
						if strings.Contains(data, blob) {
							t.Fatalf("compaction blob leaked to upstream: %s", data)
						}
						if scenario == "owned_restart" && !strings.Contains(data, "SYNTHETIC_SUMMARY_PRESERVED") {
							t.Fatal("restart lost owned summary")
						}
					}
					if state := egress.RuntimeStats(); state.Network.Requests != 0 {
						t.Fatalf("request leases remain: %+v", state)
					}
				})
			}
		}
	}
}
