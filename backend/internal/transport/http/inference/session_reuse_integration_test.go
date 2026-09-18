package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
)

type sessionReuseEligibility struct {
	accountID atomic.Uint64
	heldNode  atomic.Uint64
}

func (s *sessionReuseEligibility) AccountSchedulable(id uint64) bool {
	return id == s.accountID.Load()
}
func (s *sessionReuseEligibility) ExitSchedulable(id uint64) bool {
	return id != s.heldNode.Load()
}
func (s *sessionReuseEligibility) CheckExitAdmission(_ context.Context, id uint64) (bool, error) {
	return s.ExitSchedulable(id), nil
}

// Real HTTP/auth/identity/account selection/Build/pool/SQL cooperation. Only
// upstream generation and the quality eligibility projection are synthetic.
func TestHTTPSessionReuseSurvivesAccountSwitchAndExitRemand(t *testing.T) {
	for _, operation := range []string{"responses", "chat/completions", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", operation, stream), func(t *testing.T) {
				ctx := context.Background()
				db := compactionDatabase(t, "sqlite")
				cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
				if err != nil {
					t.Fatal(err)
				}
				repo := relational.NewEgressRepository(db)
				network := infraegress.NewManagerWithLimits(repo, cipher, netbudget.Limits{})
				t.Cleanup(func() { _ = network.Close(ctx) })
				gate := &sessionReuseEligibility{}
				network.SetExitEligibility(gate)
				pool, err := repo.CreateEgressPool(ctx, domain.Pool{Name: "synthetic-session-pool", Enabled: true, Strategy: domain.PoolStrategySessionReuse})
				if err != nil {
					t.Fatal(err)
				}
				type wire struct {
					node                         uint64
					authorization, session, body string
				}
				wires := make(chan wire, 16)
				var generated, connections atomic.Int32
				upstream := completionHTTPUpstreamWithResponse(t, "grok-4.6", &generated, func(call int32, answer map[string]any) {
					answer["id"] = fmt.Sprintf("resp_synthetic_session_%d", call)
				})
				t.Cleanup(upstream.Close)
				var members []uint64
				for i := range 2 {
					var nodeID uint64
					proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, err := io.ReadAll(r.Body)
						if err != nil {
							t.Error(err)
							return
						}
						wires <- wire{nodeID, r.Header.Get("Authorization"), r.Header.Get("x-grok-conv-id"), string(body)}
						r.Body = io.NopCloser(bytes.NewReader(body))
						upstream.Config.Handler.ServeHTTP(w, r)
					}))
					proxy.Config.ConnState = func(_ net.Conn, state http.ConnState) {
						if state == http.StateNew {
							connections.Add(1)
						}
					}
					proxy.Start()
					t.Cleanup(proxy.Close)
					encrypted, err := cipher.Encrypt(proxy.URL)
					if err != nil {
						t.Fatal(err)
					}
					node, err := repo.CreateEgressNode(ctx, domain.Node{Name: fmt.Sprintf("synthetic-exit-%d", i), Enabled: true, Health: 1, EncryptedProxyURL: encrypted})
					if err != nil {
						t.Fatal(err)
					}
					nodeID = node.ID
					members = append(members, node.ID)
				}
				if err := repo.SetEgressPoolMembers(ctx, pool.ID, members); err != nil {
					t.Fatal(err)
				}
				config := domain.DefaultOperationsConfig()
				config.DefaultTarget = domain.RoutingTarget{Mode: domain.RoutingTargetPool, PoolID: pool.ID}
				if _, err := repo.SaveEgressOperationsConfig(ctx, config, func(domain.Node) error { return nil }); err != nil {
					t.Fatal(err)
				}
				fx := newProviderCompletionFixtureOnDatabase(t, "http://synthetic-upstream.invalid", "grok-4.6", account.ProviderBuild, nil, func(a provider.Adapter) provider.Adapter {
					a.(*cli.Adapter).SetEgress(network)
					return a
				}, nil, nil, db, "")
				second := fx.account
				second.ID, second.Name, second.SourceKey = 0, "synthetic-second", "synthetic-second"
				second.UserID = "597f19f8-49d4-458a-bee4-43ec3dcaf8ca"
				second.EncryptedAccessToken, err = cipher.Encrypt("synthetic-second-token")
				if err != nil {
					t.Fatal(err)
				}
				second, _, err = fx.accounts.UpsertByIdentity(ctx, second)
				if err != nil {
					t.Fatal(err)
				}
				if second.ID == fx.account.ID {
					t.Fatal("fixture did not create an independent account")
				}
				if err := testsupport.Capabilities(ctx, fx.models, fx.accounts, second.ID, []string{"grok-4.6"}, time.Now()); err != nil {
					t.Fatal(err)
				}
				fx.selector.SetQualityEligibility(gate)
				gate.accountID.Store(fx.account.ID)
				server := httptest.NewServer(fx.router)
				t.Cleanup(server.Close)
				request := func(session, header string, continued bool) wire {
					t.Helper()
					messages := []any{map[string]string{"role": "user", "content": "synthetic first turn"}}
					if continued {
						messages = append(messages, map[string]string{"role": "assistant", "content": "completion answer"}, map[string]string{"role": "user", "content": "synthetic next turn"})
					}
					payload := map[string]any{"model": fx.publicModel, "stream": stream, "prompt_cache_key": session, "max_tokens": 2048}
					if operation == "responses" {
						payload["input"] = messages
					} else {
						payload["messages"] = messages
					}
					encoded, _ := json.Marshal(payload)
					req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/"+operation, bytes.NewReader(encoded))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Authorization", "Bearer "+fx.created.Secret)
					req.Header.Set("Content-Type", "application/json")
					req.Header.Set("anthropic-version", "2023-06-01")
					req.Header.Set("X-Session-Id", header)
					response, err := server.Client().Do(req)
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(response.Body)
					_ = response.Body.Close()
					if err != nil || response.StatusCode != 200 || !bytes.Contains(body, []byte("completion answer")) {
						t.Fatalf("delivery: status=%d err=%v body=%s", response.StatusCode, err, body)
					}
					if stream {
						terminal := map[string]string{"responses": "response.completed", "chat/completions": "[DONE]", "messages": "message_stop"}[operation]
						if !bytes.Contains(body, []byte(terminal)) || bytes.Contains(body, []byte(`"error"`)) {
							t.Fatalf("stream did not complete successfully: %s", body)
						}
					}
					select {
					case value := <-wires:
						return value
					default:
						t.Fatal("no upstream call")
						return wire{}
					}
				}
				first := request("synthetic-session", "header-1", false)
				gate.accountID.Store(second.ID)
				next := request("synthetic-session", "header-2", true)
				if first.node != next.node || first.session == "" || first.session != next.session || first.authorization == next.authorization || connections.Load() != 1 {
					t.Fatalf("account switch lost identity/connection: first=%+v next=%+v connections=%d", first, next, connections.Load())
				}
				gate.heldNode.Store(first.node)
				moved := request("synthetic-session", "header-3", true)
				if moved.node == first.node || moved.session != first.session || !strings.Contains(moved.body, "synthetic first turn") || !strings.Contains(moved.body, "synthetic next turn") {
					t.Fatalf("remand lost route/history: %+v", moved)
				}
				gate.heldNode.Store(0)
				if released := request("synthetic-session", "header-4", true); released.node != moved.node || connections.Load() != 2 {
					t.Fatalf("release moved session again: %+v connections=%d", released, connections.Load())
				}
				fresh := request("synthetic-new-session", "header-5", false)
				if fresh.node != first.node || fresh.session == first.session || generated.Load() != 5 {
					t.Fatalf("new session did not distribute: %+v calls=%d", fresh, generated.Load())
				}
			})
		}
	}
}
