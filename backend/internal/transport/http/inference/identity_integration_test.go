package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type identityWire struct{ body, hint, authorization, responseID, opaque string }

// Exercise real HTTP/auth/Gateway/account/Build/journal cooperation. PostgreSQL
// also uses shared Redis affinity and capacity, with a separate namespace.
func TestHTTPSessionIdentityHistoryAndHints(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%t", dialect, operation, stream), func(t *testing.T) {
					ctx := context.Background()
					db := compactionDatabase(t, dialect)
					cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
					if err != nil {
						t.Fatal(err)
					}
					logger := slog.New(slog.NewTextHandler(io.Discard, nil))
					var mu sync.Mutex
					var wires []identityWire
					var rejectNext atomic.Bool
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						data, readErr := io.ReadAll(r.Body)
						if readErr != nil {
							t.Error(readErr)
							return
						}
						var payload map[string]any
						if err := json.Unmarshal(data, &payload); err != nil {
							t.Error(err)
							return
						}
						mu.Lock()
						n := len(wires) + 1
						raw := make([]byte, 256)
						for i := range raw {
							raw[i] = byte(i + n)
						}
						opaque := base64.RawStdEncoding.EncodeToString(raw)
						id := fmt.Sprintf("resp_identity_%d", n)
						hint, _ := payload["prompt_cache_key"].(string)
						wires = append(wires, identityWire{body: string(data), hint: hint, authorization: r.Header.Get("Authorization"), responseID: id, opaque: opaque})
						mu.Unlock()
						if hint == "" || r.Header.Get("x-grok-conv-id") != hint {
							t.Error("body/header session identity disagrees")
						}
						if rejectNext.Swap(false) {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(429)
							_, _ = io.WriteString(w, `{"error":"rate limited"}`)
							return
						}
						answer := fmt.Sprintf(`{"id":%q,"object":"response","status":"completed","model":%q,"output":[{"type":"reasoning","encrypted_content":%q,"summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}`, id, payload["model"], opaque)
						if payload["stream"] == true {
							w.Header().Set("Content-Type", "text/event-stream")
							fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", answer)
						} else {
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, answer)
						}
					}))
					t.Cleanup(upstream.Close)
					accounts := relational.NewAccountRepository(db)
					models := relational.NewModelRepository(db)
					audits := relational.NewAuditRepository(db)
					keys := relational.NewClientKeyRepository(db)
					modelNames := []string{"grok-4.5", modeldomain.GrokComposer25Fast}
					var createdAccounts []account.Credential
					for _, name := range []string{"one", "two", "three"} {
						token, err := cipher.Encrypt("synthetic-" + name)
						if err != nil {
							t.Fatal(err)
						}
						c, _, err := accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, Name: name, SourceKey: name, EncryptedAccessToken: token, ExpiresAt: time.Now().Add(time.Hour), Enabled: true, AuthStatus: account.AuthStatusActive, MaxConcurrent: 2})
						if err != nil {
							t.Fatal(err)
						}
						createdAccounts = append(createdAccounts, c)
						if err = testsupport.Capabilities(ctx, models, accounts, c.ID, modelNames, time.Now()); err != nil {
							t.Fatal(err)
						}
					}
					if err = testsupport.Discover(ctx, models, account.ProviderBuild, modelNames); err != nil {
						t.Fatal(err)
					}
					egress := infraegress.NewManager(relational.NewEgressRepository(db), cipher)
					t.Cleanup(func() { _ = egress.Close(context.Background()) })
					var sticky repository.StickySessionRepository = memory.NewStickyStore()
					var concurrency repository.ConcurrencyLimiter = memory.NewConcurrencyLimiter()
					if dialect == "postgres" {
						address := os.Getenv("TEST_REDIS_ADDRESS")
						if address == "" {
							t.Skip("isolated TEST_REDIS_ADDRESS required")
						}
						store, err := redisruntime.Open(ctx, redisruntime.Config{Address: address, KeyPrefix: fmt.Sprintf("g31-%d:", time.Now().UnixNano()), ConcurrencyLease: time.Minute})
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { _ = store.Close() })
						sticky = store
						concurrency = redisruntime.NewConcurrencyLimiter(store)
					}
					clientService := clientkeyapp.NewService("identity-owner", keys, memory.NewRateLimiter(), concurrency, 240, 4, cipher)
					t.Cleanup(func() { closeClientKeyService(t, clientService) })
					firstKey, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "one", Enabled: true, RPMLimit: 240, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					otherKey, err := clientService.Create(ctx, clientkeyapp.CreateInput{Name: "two", Enabled: true, RPMLimit: 240, MaxConcurrent: 4})
					if err != nil {
						t.Fatal(err)
					}
					makeHistory := func() *historyapp.ReasoningReplay {
						replay := historyapp.New(memory.NewReasoningReplayStore(32), historyapp.Config{Enabled: true, TTL: time.Hour}, logger)
						replay.UseJournal(relational.NewConversationJournal(db, cipher, 8<<20), time.Hour, time.Hour)
						return replay
					}
					var activeService *gateway.Service
					makeRouter := func() *gin.Engine {
						build := cli.NewAdapter(cli.Config{BaseURL: upstream.URL + "/v1"}, cipher)
						build.SetReasoningReplay(makeHistory())
						build.SetLegacyReplayAccounts([]uint64{99})
						build.SetEgress(egress)
						registry := provider.NewRegistry(build)
						maintenance := accountapp.NewService(accounts, audits, memory.NewDeviceSessionStore(), sticky, registry, cipher, nil)
						selector := gateway.NewSelector(accounts, concurrency, sticky, registry, time.Hour, time.Second, time.Minute)
						service := gateway.NewService(models, audits, maintenance, clientService, registry, selector, relational.NewResponseRepository(db), 2)
						activeService = service
						router := gin.New()
						router.Use(middleware.RequestID(), middleware.ClientAuth(clientService))
						NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
						return router
					}
					var active atomic.Pointer[gin.Engine]
					active.Store(makeRouter())
					handled := make(chan struct{}, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { active.Load().ServeHTTP(w, r); handled <- struct{}{} }))
					t.Cleanup(server.Close)
					input := func(opening string, continued bool) map[string]any {
						messages := []any{map[string]any{"role": "user", "content": opening}}
						if continued {
							messages = append(messages, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next"})
						}
						p := map[string]any{"model": "grok-4.5", "stream": stream}
						if operation == "responses" {
							p["input"] = messages
						} else {
							p["messages"] = messages
						}
						if operation == "messages" {
							p["max_tokens"] = 128
						}
						return p
					}
					lastWarnings := ""
					send := func(secret string, payload map[string]any, headers http.Header, want int) []identityWire {
						t.Helper()
						mu.Lock()
						before := len(wires)
						mu.Unlock()
						body, err := json.Marshal(payload)
						if err != nil {
							t.Fatal(err)
						}
						req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/"+operation, strings.NewReader(string(body)))
						if err != nil {
							t.Fatal(err)
						}
						req.Header = headers.Clone()
						if req.Header == nil {
							req.Header = make(http.Header)
						}
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Authorization", "Bearer "+secret)
						req.Header.Set("anthropic-version", "2023-06-01")
						res, err := server.Client().Do(req)
						if err != nil {
							t.Fatal(err)
						}
						data, err := io.ReadAll(res.Body)
						_ = res.Body.Close()
						if err != nil {
							t.Fatal(err)
						}
						select {
						case <-handled:
						case <-time.After(3 * time.Second):
							t.Fatal("handler did not finish")
						}
						lastWarnings = res.Header.Get("X-Grok2API-Compatibility-Warnings")
						if res.StatusCode != want {
							t.Fatalf("status=%d want=%d response=%s", res.StatusCode, want, data)
						}
						mu.Lock()
						defer mu.Unlock()
						return append([]identityWire(nil), wires[before:]...)
					}
					one := func(got []identityWire) identityWire {
						t.Helper()
						if len(got) != 1 {
							t.Fatalf("physical calls=%d want=1", len(got))
						}
						return got[0]
					}
					// G32: colliding old encodings are distinct at the real wire and
					// journal boundaries, including raw keys impersonating namespaces.
					{
						groups := []struct {
							name                string
							first, second       http.Header
							firstKey, secondKey string
							title               bool
						}{
							{name: "claude-fields", first: http.Header{"X-Claude-Code-Session-Id": {"a:agent:b"}, "X-Claude-Code-Agent-Id": {"c"}}, second: http.Header{"X-Claude-Code-Session-Id": {"a"}, "X-Claude-Code-Agent-Id": {"b:agent:c"}}},
							{name: "claude-raw", first: http.Header{"X-Claude-Code-Session-Id": {"raw-main"}}, secondKey: "claude:raw-main:agent:main"},
							{name: "window-raw", first: http.Header{"X-Codex-Window-Id": {"window-main"}}, secondKey: "codex:window:window-main"},
							{name: "title-raw", first: http.Header{"X-Claude-Code-Session-Id": {"title-main"}}, firstKey: "title-branch", secondKey: "aux:claude-title:title-branch", title: true},
							{name: "new-prefix-raw", first: http.Header{"X-Codex-Window-Id": {"x"}}, secondKey: "g2a:identity:1:window:1:x"},
						}
						for _, group := range groups {
							a := input(group.name, false)
							if group.firstKey != "" {
								a["prompt_cache_key"] = group.firstKey
							}
							if group.title {
								a["system"] = "Generate a concise title of this coding session"
							}
							first := one(send(firstKey.Secret, a, group.first, 200))
							b := input(group.name, true)
							if group.secondKey != "" {
								b["prompt_cache_key"] = group.secondKey
							}
							second := one(send(firstKey.Secret, b, group.second, 200))
							if first.hint == second.hint || strings.Contains(second.body, first.opaque) {
								t.Fatalf("%s collides or restores another identity", group.name)
							}
							active.Store(makeRouter())
							a = input(group.name, true)
							if group.firstKey != "" {
								a["prompt_cache_key"] = group.firstKey
							}
							if group.title {
								a["system"] = "Generate a concise title of this coding session"
							}
							restored := one(send(firstKey.Secret, a, group.first, 200))
							if restored.hint != first.hint || !strings.Contains(restored.body, first.opaque) || strings.Contains(restored.body, second.opaque) {
								t.Fatalf("%s lost its own isolated history after restart", group.name)
							}
						}
						// Seed exactly the old v5 scope. Its immutable records survive
						// strict rejection and an explicitly allowed new branch.
						var resolver historyapp.IdentityResolver
						signals := historydomain.ClientSignals{ClaudeSession: "migration-main"}
						identity := resolver.Resolve(historyapp.NewIdentityRequest(firstKey.Key.ID, signals, "", "migration", "migration", nil), historyapp.IdentityTarget{Provider: string(account.ProviderBuild), Model: "grok-4.5"}, historyapp.Identity{})
						if identity.PriorReplayKey == "" || identity.PriorReplayKey == identity.ReplayKey {
							t.Fatal("migration identity has no distinct prior scope")
						}
						oldKey := historydomain.ReplayScope(identity.PriorReplayKey, 1, historydomain.ReplayPlaneBuild)
						oldHistory := makeHistory()
						_, prepared, err := oldHistory.Prepare(ctx, "grok-4.5", oldKey, []byte(`{"input":[{"role":"user","content":"migration"}]}`))
						if err != nil {
							t.Fatal(err)
						}
						raw := make([]byte, 256)
						for i := range raw {
							raw[i] = byte(255 - i)
						}
						oldOpaque := base64.RawStdEncoding.EncodeToString(raw)
						response := fmt.Sprintf(`{"id":"resp_g32_old","status":"completed","output":[{"type":"reasoning","encrypted_content":%q,"summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, oldOpaque)
						captured, commit, discard := prepared.Capture(io.NopCloser(strings.NewReader(response)), false)
						if _, err := io.Copy(io.Discard, captured); err != nil {
							t.Fatal(err)
						}
						_ = captured.Close()
						if err := commit(); err != nil {
							t.Fatal(err)
						}
						discard()
						oldScope := repository.JournalScope{Key: oldKey, Model: "grok-4.5", Normalizer: 1}
						journal := relational.NewConversationJournal(db, cipher, 8<<20)
						before, err := journal.Inspect(ctx, []repository.JournalScope{oldScope}, time.Now())
						if err != nil {
							t.Fatal(err)
						}
						if len(before) != 1 || len(before[0].Turns) != 1 {
							t.Fatal("old scope fixture missing")
						}
						strictBody := []byte(fmt.Sprintf(`{"model":"grok-4.5","stream":%t,"input":[{"role":"user","content":"migration"},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`, stream))
						mode := historydomain.PreserveOpaque
						mu.Lock()
						count := len(wires)
						mu.Unlock()
						result, err := activeService.CreateResponse(ctx, gateway.Input{RequestID: "g32-strict", ClientKey: firstKey.Key, PublicModel: "grok-4.5", SessionSignals: signals, HistoryRecoveryPolicy: &mode, Body: strictBody, Streaming: stream})
						if result != nil {
							_ = result.Body.Close()
						}
						var failure *gateway.UpstreamFailure
						if !errors.As(err, &failure) || failure.Code != "history_identity_context_unavailable" {
							t.Fatalf("strict migration failure=%v", err)
						}
						mu.Lock()
						afterStrict := len(wires)
						mu.Unlock()
						if afterStrict != count {
							t.Fatal("strict migration used physical attempt")
						}
						rejectNext.Store(true)
						migrated := send(firstKey.Secret, input("migration", true), http.Header{"X-Claude-Code-Session-Id": {"migration-main"}}, 200)
						if len(migrated) != 2 || !strings.Contains(lastWarnings, "history_identity_context_unavailable") {
							t.Fatalf("migration calls=%d warning=%q", len(migrated), lastWarnings)
						}
						for _, wire := range migrated {
							if strings.Contains(wire.body, oldOpaque) {
								t.Fatal("ambiguous old opaque reached upstream")
							}
						}
						after, err := journal.Inspect(ctx, []repository.JournalScope{oldScope}, time.Now())
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(before, after) {
							t.Fatal("old journal changed during isolation")
						}
						active.Store(makeRouter())
						follow := input("migration", true)
						field := "messages"
						if operation == "responses" {
							field = "input"
						}
						follow[field] = append(follow[field].([]any), map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "after restart"})
						resumed := one(send(firstKey.Secret, follow, http.Header{"X-Claude-Code-Session-Id": {"migration-main"}}, 200))
						if !strings.Contains(resumed.body, migrated[1].opaque) || strings.Contains(resumed.body, oldOpaque) || strings.Contains(lastWarnings, "history_identity_context_unavailable") {
							t.Fatal("accepted new branch did not become authoritative")
						}
						if operation == "responses" {
							complete := input("migration", true)
							complete["input"] = []any{map[string]any{"role": "user", "content": "migration"}, map[string]any{"type": "reasoning", "encrypted_content": oldOpaque, "summary": []any{}}, map[string]any{"role": "assistant", "content": "answer"}, map[string]any{"role": "user", "content": "next"}}
							carried := one(send(firstKey.Secret, complete, http.Header{"X-Claude-Code-Session-Id": {"migration-main"}}, 200))
							if !strings.Contains(carried.body, oldOpaque) || strings.Contains(lastWarnings, "history_identity_context_unavailable") {
								t.Fatal("complete client native history was discarded or called lossy")
							}
							route, err := models.GetByProviderUpstream(ctx, account.ProviderBuild, "grok-4.5")
							if err != nil {
								t.Fatal(err)
							}
							var ownerID uint64
							for _, credential := range createdAccounts {
								if migrated[1].authorization == "Bearer synthetic-"+credential.Name {
									ownerID = credential.ID
								}
							}
							if ownerID == 0 {
								t.Fatal("missing accepted account")
							}
							now := time.Now().UTC()
							saved := inferencedomain.ResponseOwnership{ResponseID: "resp_g32_old", AccountID: ownerID, ClientKeyID: firstKey.Key.ID, ModelRouteID: route.ID, Provider: account.ProviderBuild, PromptCacheKey: "99999999-8888-7777-6666-555555555555", ReasoningReplayKey: identity.PriorReplayKey, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}
							if err := relational.NewResponseRepository(db).Save(ctx, saved); err != nil {
								t.Fatal(err)
							}
							previous := input("verified previous", false)
							previous["previous_response_id"] = saved.ResponseID
							previous["prompt_cache_key"] = "codex:window:conflicting-new-identity"
							inherited := one(send(firstKey.Secret, previous, http.Header{"X-Claude-Code-Session-Id": {"another-session"}}, 200))
							if inherited.hint != saved.PromptCacheKey || !strings.Contains(inherited.body, saved.ResponseID) || strings.Contains(lastWarnings, "history_identity_context_unavailable") {
								t.Fatal("verified old previous response did not keep its identity")
							}
							if got := send(otherKey.Secret, previous, nil, 404); len(got) != 0 {
								t.Fatal("foreign client inherited old response identity")
							}
						}

					}

					// A body branch key moves to Codex metadata while Gateway/Provider/history
					// restart. A real 429 switches accounts without changing or losing history.
					p := input("main", false)
					p["prompt_cache_key"] = "identity-main"
					first := one(send(firstKey.Secret, p, http.Header{"X-Codex-Window-Id": {"old-window"}}, 200))
					active.Store(makeRouter())
					rejectNext.Store(true)
					continued := send(firstKey.Secret, input("main", true), http.Header{"X-Codex-Turn-Metadata": {`{"prompt_cache_key":"identity-main","window_id":"new-window"}`}}, 200)
					if len(continued) != 2 || continued[0].authorization == continued[1].authorization {
						t.Fatalf("account failover calls=%d", len(continued))
					}
					for _, wire := range continued {
						if wire.hint != first.hint || !strings.Contains(wire.body, first.opaque) {
							t.Fatal("restart/account switch changed explicit history")
						}
					}
					// The same input/branch on another client key cannot restore the first key.
					p = input("main", true)
					p["prompt_cache_key"] = "identity-main"
					foreign := one(send(otherKey.Secret, p, nil, 200))
					if foreign.hint == first.hint || strings.Contains(foreign.body, first.opaque) {
						t.Fatal("tenant identity not isolated")
					}
					titleHeader := http.Header{"X-Claude-Code-Session-Id": {"claude-main"}}
					p = input("main", true)
					p["prompt_cache_key"] = "identity-main"
					p["system"] = "Generate a concise title of this coding session"
					title := one(send(firstKey.Secret, p, titleHeader, 200))
					if title.hint == first.hint || strings.Contains(title.body, first.opaque) {
						t.Fatal("title shared main replay")
					}
					if operation == "messages" {
						p = input("main", true)
						p["prompt_cache_key"] = "identity-main"
						p["tools"] = []any{map[string]any{"type": "web_search_20250305", "name": "web_search"}}
						searched := one(send(firstKey.Secret, p, nil, 200))
						if searched.hint != first.hint || strings.Contains(searched.body, first.opaque) {
							t.Fatal("translated search inherited native opaque lineage")
						}
					}
					// Soft hints remain stable, yet never restore the opaque output they observed.
					soft := one(send(firstKey.Secret, input("soft", false), nil, 200))
					softNext := one(send(firstKey.Secret, input("soft", true), nil, 200))
					if soft.hint != softNext.hint || strings.Contains(softNext.body, soft.opaque) {
						t.Fatal("soft hint enabled opaque replay or rotated")
					}
					p = input("composer", false)
					p["model"] = modeldomain.GrokComposer25Fast
					c1 := one(send(firstKey.Secret, p, nil, 200))
					c2 := one(send(firstKey.Secret, p, nil, 200))
					if c1.hint == c2.hint || strings.Contains(c2.body, c1.opaque) {
						t.Fatal("Composer stateless requests share identity")
					}
					p = input("main", true)
					p["model"] = modeldomain.GrokComposer25Fast
					p["prompt_cache_key"] = "identity-main"
					otherModel := one(send(firstKey.Secret, p, nil, 200))
					if otherModel.hint == first.hint || strings.Contains(otherModel.body, first.opaque) {
						t.Fatal("model scope collapsed")
					}
					// Seed a retired account's v2 journal through the real history owner. The
					// actual Build adapter must migrate it to v3 on the next accepted request.
					var resolver historyapp.IdentityResolver
					identity := resolver.Resolve(historyapp.NewIdentityRequest(firstKey.Key.ID, historydomain.ClientSignals{PromptCacheKey: "legacy-branch"}, "", "seed", "seed", nil), historyapp.IdentityTarget{Provider: string(account.ProviderBuild), Model: "grok-4.5"}, historyapp.Identity{})
					legacyKey := historydomain.LegacyReplayScopes(identity.ReplayKey, 99, historydomain.ReplayPlaneBuild, nil)[0]
					replay := makeHistory()
					_, prepared, err := replay.Prepare(ctx, "grok-4.5", legacyKey, []byte(`{"input":[{"role":"user","content":"legacy"}]}`))
					if err != nil {
						t.Fatal(err)
					}
					legacyOpaque := first.opaque
					answer := fmt.Sprintf(`{"id":"resp_retired","status":"completed","output":[{"type":"reasoning","encrypted_content":%q,"summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`, legacyOpaque)
					captured, commit, discard := prepared.Capture(io.NopCloser(strings.NewReader(answer)), false)
					_, err = io.Copy(io.Discard, captured)
					_ = captured.Close()
					if err != nil {
						discard()
						t.Fatal(err)
					}
					if err = commit(); err != nil {
						discard()
						t.Fatal(err)
					}
					discard()
					p = input("legacy", true)
					p["prompt_cache_key"] = "legacy-branch"
					migrated := one(send(firstKey.Secret, p, nil, 200))
					if !strings.Contains(migrated.body, legacyOpaque) {
						t.Fatal("retired v2 account history not migrated")
					}
					if operation == "responses" {
						p = input("incremental", false)
						p["previous_response_id"] = continued[1].responseID
						p["prompt_cache_key"] = "conflicting-branch"
						inherited := one(send(firstKey.Secret, p, nil, 200))
						if inherited.hint != first.hint {
							t.Fatal("previous response did not retain root identity")
						}
						if got := send(otherKey.Secret, p, nil, 404); len(got) != 0 {
							t.Fatal("foreign response ownership reached upstream")
						}
					}
				})
			}
		}
	}
}
