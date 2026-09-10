package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestHistoryScopeFollowsActualPlaneAndAccount(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var mu sync.Mutex
			var paths, bodies []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				mu.Lock()
				paths = append(paths, r.URL.Path)
				bodies = append(bodies, string(data))
				mu.Unlock()
				answer := recoveryAnswer("reply", recoveryOpaque(9))
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", answer)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, answer)
				}
			}))
			t.Cleanup(upstream.Close)
			old, replay, request := newRecoveryJournalAdapter(t, upstream.URL, false)
			adapter := NewAdapter(Config{BaseURL: upstream.URL + "/v1", FallbackBaseURL: upstream.URL + "/xai/v1"}, old.cipher)
			adapter.SetReasoningReplay(replay)
			adapter.SetLegacyReplayAccounts([]uint64{1, 2, 99})
			t.Cleanup(func() { adapter.base.current.Load().CloseIdleConnections() })
			xaiKey := historydomain.ReplayScope(request.ReasoningReplayKey, 1, historydomain.ReplayPlaneXAI)
			_, prepared, err := replay.Prepare(context.Background(), request.Model, xaiKey, []byte(`{"input":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			captured, commit, discard := prepared.Capture(io.NopCloser(strings.NewReader(recoveryAnswer("xai-seed", recoveryOpaque(5)))), false)
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
			for _, tc := range []struct {
				name       string
				id         uint64
				mode       account.BuildRouteMode
				want, path string
			}{
				{"Build first", 1, account.BuildRouteBuild, recoveryOpaque(0), "/v1/responses"},
				{"Build other account", 2, account.BuildRouteBuild, recoveryOpaque(0), "/v1/responses"},
				{"XAI first", 1, account.BuildRouteXAI, recoveryOpaque(5), "/xai/v1/responses"},
				{"XAI other account", 2, account.BuildRouteXAI, "", "/xai/v1/responses"},
				{"return Build", 1, account.BuildRouteBuild, recoveryOpaque(0), "/v1/responses"},
			} {
				request.Credential.ID = tc.id
				request.Credential.BuildRouteMode = tc.mode
				request.Body = recoveryInput("responses", stream)
				request.Streaming = stream
				response, err := adapter.ForwardResponse(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.DiscardOutput != nil {
					response.DiscardOutput()
				}
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 {
					t.Fatalf("%s status=%d", tc.name, response.StatusCode)
				}
				mu.Lock()
				path, body := paths[len(paths)-1], bodies[len(bodies)-1]
				mu.Unlock()
				if path != tc.path {
					t.Fatalf("%s path=%s", tc.name, path)
				}
				for _, opaque := range []string{recoveryOpaque(0), recoveryOpaque(5)} {
					if strings.Contains(body, opaque) != (opaque == tc.want) {
						t.Fatalf("%s restored wrong plane/account history", tc.name)
					}
				}
			}
		})
	}
}

func TestIdentityMigrationAuthorizationFollowsActualPlane(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			var mu sync.Mutex
			var bodies []string
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, _ := io.ReadAll(r.Body)
				mu.Lock()
				bodies = append(bodies, string(data))
				mu.Unlock()
				answer := recoveryAnswer("migrated", recoveryOpaque(9))
				if stream {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":%s}\n\n", answer)
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, answer)
				}
			}))
			defer upstream.Close()
			original, replay, request := newRecoveryJournalAdapter(t, upstream.URL, false)
			adapter := NewAdapter(Config{BaseURL: upstream.URL + "/v1", FallbackBaseURL: upstream.URL + "/xai/v1"}, original.cipher)
			adapter.SetReasoningReplay(replay)
			t.Cleanup(func() { adapter.base.current.Load().CloseIdleConnections() })
			seedRecoveryHistory(t, replay, request.Model, historydomain.ReplayScope(request.ReasoningReplayKey, 1, historydomain.ReplayPlaneXAI))
			request.PriorReasoningReplayKey = request.ReasoningReplayKey
			request.ReasoningReplayKey = "new-typed-identity"
			for _, test := range []struct {
				name                         string
				mode                         account.BuildRouteMode
				accountID                    uint64
				carry, allow, absent, denied bool
			}{
				{name: "Build same", mode: account.BuildRouteBuild, accountID: 1, denied: true},
				{name: "Build other", mode: account.BuildRouteBuild, accountID: 2, denied: true},
				{name: "XAI same", mode: account.BuildRouteXAI, accountID: 1, denied: true},
				{name: "XAI other", mode: account.BuildRouteXAI, accountID: 2},
				{name: "Build complete", mode: account.BuildRouteBuild, accountID: 1, carry: true},
				{name: "XAI complete", mode: account.BuildRouteXAI, accountID: 1, carry: true},
				{name: "Build allowed", mode: account.BuildRouteBuild, accountID: 1, allow: true},
				{name: "XAI allowed", mode: account.BuildRouteXAI, accountID: 1, allow: true},
				{name: "missing controller", mode: account.BuildRouteBuild, accountID: 1, absent: true, denied: true},
			} {
				t.Run(test.name, func(t *testing.T) {
					request.Credential.ID, request.Credential.BuildRouteMode = test.accountID, test.mode
					request.Body, request.Streaming = recoveryInput("responses", stream), stream
					if test.carry {
						request.Body = []byte(fmt.Sprintf(`{"model":"grok-4.5","stream":%t,"input":[{"role":"user","content":"hello"},{"type":"reasoning","encrypted_content":%q,"summary":[]},{"role":"assistant","content":"answer"},{"role":"user","content":"next"}]}`, stream, recoveryOpaque(0)))
					}
					budget := inferencedomain.NewAttemptBudget(1)
					defer budget.Close()
					mode := historydomain.PreserveOpaque
					if test.allow {
						mode = historydomain.AllowLossyRecovery
					}
					request.HistoryControl = gateway.NewHistoryController(mode, budget)
					if test.absent {
						request.HistoryControl = nil
					}
					mu.Lock()
					before := len(bodies)
					mu.Unlock()
					response, err := adapter.ForwardResponse(context.Background(), request)
					if test.denied {
						if !errors.Is(err, historydomain.ErrIdentityLossNotAuthorized) || response != nil {
							t.Fatalf("denied response=%v err=%v", response, err)
						}
						mu.Lock()
						count := len(bodies)
						mu.Unlock()
						if count != before {
							t.Fatal("denied migration called upstream")
						}
						return
					}
					if err != nil {
						t.Fatal(err)
					}
					_, err = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
					if response.DiscardOutput != nil {
						response.DiscardOutput()
					}
					if err != nil || response.StatusCode != 200 {
						t.Fatalf("accepted migration status=%d err=%v", response.StatusCode, err)
					}
					mu.Lock()
					count, body := len(bodies), bodies[len(bodies)-1]
					mu.Unlock()
					if count != before+1 || strings.Contains(body, recoveryOpaque(0)) != test.carry {
						t.Fatal("migration copied old opaque or added generation")
					}
				})
			}
		})
	}
}
