package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestHTTPModelNamePriorityAndKeyPermissions(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	wireModels := make(chan string, 100)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		wireModels <- fmt.Sprint(body["model"])
		id := fmt.Sprintf("resp_priority_%d", calls.Add(1))
		answer := map[string]any{"id": id, "object": "response", "status": "completed", "model": body["model"], "output": []any{map[string]any{"type": "message", "role": "assistant", "id": "msg_priority", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "priority answer"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": id, "model": body["model"]}}, {"type": "response.output_text.delta", "item_id": "msg_priority", "delta": "priority answer"}, {"type": "response.completed", "response": answer}} {
				encoded, _ := json.Marshal(event)
				fmt.Fprintf(w, "data: %s\n\n", encoded)
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(answer)
		}
	}))
	defer upstream.Close()
	fx := newProviderCompletionFixture(t, upstream.URL, "grok-4.5", account.ProviderBuild, nil, nil)
	base, err := fx.models.Create(ctx, model.Route{PublicID: "Build/product", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5", Capability: model.CapabilityResponses, Enabled: true}, []uint64{fx.account.ID})
	if err != nil {
		t.Fatal(err)
	}
	requested, err := fx.models.Create(ctx, model.Route{PublicID: "Build/Build/product", Provider: account.ProviderBuild, UpstreamModel: "requested-product", Capability: model.CapabilityResponses, Enabled: true}, []uint64{fx.account.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Persisted aliases must retain the same literal priority after a rename.
	current := "Build/current-product"
	if _, err := fx.models.Patch(ctx, requested.ID, model.RoutePatch{PublicID: &current}); err != nil {
		t.Fatal(err)
	}
	key := fx.created.Key
	key.AllowedModels = []uint64{base.ID}
	if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
		t.Fatal(err)
	}
	assertRequests := func(t *testing.T, want int, upstreamModel string) {
		t.Helper()
		for _, operation := range []string{"responses", "chat/completions", "messages"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/stream-%t", operation, stream), func(t *testing.T) {
					body := map[string]any{"model": "Build/product", "stream": stream}
					if operation == "responses" {
						body["input"] = "synthetic priority"
					} else {
						body["messages"] = []map[string]string{{"role": "user", "content": "synthetic priority"}}
					}
					if operation == "messages" {
						body["max_tokens"] = 32
					}
					encoded, _ := json.Marshal(body)
					request := httptest.NewRequest(http.MethodPost, "/v1/"+operation, strings.NewReader(string(encoded)))
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Anthropic-Version", "2023-06-01")
					request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
					recorder := httptest.NewRecorder()
					before := calls.Load()
					fx.router.ServeHTTP(recorder, request)
					if recorder.Code != want {
						t.Fatalf("status=%d want=%d body=%s", recorder.Code, want, recorder.Body.String())
					}
					if want == http.StatusOK {
						if calls.Load() != before+1 {
							t.Fatalf("physical calls=%d", calls.Load()-before)
						}
						if got := <-wireModels; got != upstreamModel {
							t.Fatalf("wrong upstream=%s want=%s", got, upstreamModel)
						}
						if !strings.Contains(recorder.Body.String(), "priority answer") {
							t.Fatalf("answer not delivered: %s", recorder.Body.String())
						}
					} else if calls.Load() != before {
						t.Fatalf("rejected name reached upstream: %d", calls.Load()-before)
					}
				})
			}
		}
	}
	t.Run("base-permission-cannot-authorize-literal", func(t *testing.T) { assertRequests(t, 403, "") })
	key.AllowedModels = []uint64{requested.ID}
	if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
		t.Fatal(err)
	}
	t.Run("literal-authorized", func(t *testing.T) { assertRequests(t, 200, "requested-product") })
	if _, err := fx.models.UpdateManyEnabled(ctx, []uint64{requested.ID}, false); err != nil {
		t.Fatal(err)
	}
	t.Run("literal-disabled", func(t *testing.T) { assertRequests(t, 404, "") })
	if _, err := fx.models.UpdateManyEnabled(ctx, []uint64{requested.ID}, true); err != nil {
		t.Fatal(err)
	}
	offline, _, err := fx.accounts.UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderBuild, Name: "offline", SourceKey: "offline", EncryptedAccessToken: "fixture", AuthStatus: account.AuthStatusActive, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	patch := repository.AccountAdminPatch{}
	no := false
	patch.Enabled = &no
	if _, err := fx.accounts.UpdateAdministration(ctx, offline.ID, patch); err != nil {
		t.Fatal(err)
	}
	ids := []uint64{offline.ID}
	if _, err := fx.models.Patch(ctx, requested.ID, model.RoutePatch{AccountIDs: &ids}); err != nil {
		t.Fatal(err)
	}
	t.Run("literal-no-account", func(t *testing.T) { assertRequests(t, 503, "") })
	if err := fx.models.Delete(ctx, requested.ID); err != nil {
		t.Fatal(err)
	}
	key.AllowedModels = []uint64{base.ID}
	if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowedModels: &key.AllowedModels, AllowModelAliases: &key.AllowModelAliases}); err != nil {
		t.Fatal(err)
	}
	t.Run("deleted-literal-allows-qualified-fallback", func(t *testing.T) { assertRequests(t, 200, "grok-4.5") })
}
