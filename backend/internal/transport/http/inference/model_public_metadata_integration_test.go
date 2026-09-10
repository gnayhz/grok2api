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

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

func TestPublicMetadataFollowsActualProduct(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("unexpected upstream request") }))
	defer upstream.Close()
	fx := newProviderCompletionFixture(t, upstream.URL, "grok-4.5", account.ProviderBuild, nil, nil)
	service := modelapp.NewService(fx.models, fx.accounts, nil, fx.registry)
	base, err := fx.models.GetByPublicID(context.Background(), "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	name := "team-coding"
	if _, err = service.Update(context.Background(), base.ID, modelapp.UpdateInput{PublicID: &name}); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "/v1/models?client_version=1", nil)
	request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	fx.router.ServeHTTP(response, request)
	var catalog codexModelCatalog
	if err = json.Unmarshal(response.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 1 {
		t.Fatalf("catalog = %#v", catalog)
	}
	entry := catalog.Models[0]
	if entry.Slug != name || entry.ContextWindow != 500000 || len(entry.SupportedReasoningLevels) != 3 || entry.DefaultReasoningLevel != "medium" {
		t.Fatalf("renamed grok-4.5 lost product metadata: slug=%s context=%d reasoning=%v default=%s", entry.Slug, entry.ContextWindow, entry.SupportedReasoningLevels, entry.DefaultReasoningLevel)
	}
}

func TestPublicModelIdentityReachesActualWireAcrossProtocols(t *testing.T) {
	ctx := context.Background()
	var calls atomic.Int32
	wire := make(chan map[string]any, 32)
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
		wire <- body
		id := fmt.Sprintf("resp_public_%d", calls.Add(1))
		answer := map[string]any{"id": id, "object": "response", "status": "completed", "model": body["model"], "output": []any{map[string]any{"type": "message", "role": "assistant", "id": "msg_public", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "published product answer"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2, "total_tokens": 5}}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, event := range []map[string]any{{"type": "response.created", "response": map[string]any{"id": id, "model": body["model"]}}, {"type": "response.output_text.delta", "item_id": "msg_public", "delta": "published product answer"}, {"type": "response.completed", "response": answer}} {
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
	key := fx.created.Key
	key.AllowModelAliases = true
	if _, err := fx.clients.Patch(ctx, key.ID, clientkey.ManagementPatch{AllowModelAliases: &key.AllowModelAliases}); err != nil {
		t.Fatal(err)
	}
	for _, spec := range []struct{ name, upstream string }{{"team-coding", "grok-4.5"}, {"grok-4.6-low", "grok-build-0.1"}, {"grok-4.3", "future-text"}, {"Build/grok-4.6", "grok-4.6"}} {
		// Manual public names include a literal Provider prefix and an effort-looking name.
		public, _ := modeldomain.NormalizeExternalPublicID(account.ProviderBuild, spec.name)
		if _, err := fx.models.Create(ctx, modeldomain.Route{PublicID: public, Provider: account.ProviderBuild, UpstreamModel: spec.upstream, Capability: modeldomain.CapabilityResponses, Enabled: true}, []uint64{fx.account.ID}); err != nil {
			t.Fatal(err)
		}
	}
	catalogRequest := httptest.NewRequest("GET", "/v1/models?client_version=1", nil)
	catalogRequest.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	listed := httptest.NewRecorder()
	fx.router.ServeHTTP(listed, catalogRequest)
	if listed.Code != 200 {
		t.Fatal(listed.Code, listed.Body.String())
	}
	var catalog codexModelCatalog
	if err := json.Unmarshal(listed.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	products := map[string]codexModelEntry{}
	for _, entry := range catalog.Models {
		products[entry.Slug] = entry
	}
	for _, tc := range []struct {
		name, upstream, effort string
		status                 int
	}{
		{"grok-4.5-low", "grok-4.5", "low", 200},
		{"team-coding", "grok-4.5", "", 200},
		{"grok-4.6-low", "grok-build-0.1", "", 200},
		{"grok-4.3", "future-text", "", 200},
		{"Build/grok-4.6-xhigh", "grok-4.6", "xhigh", 200},
		{"grok-4.3-low", "", "", 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry, exists := products[tc.name]
			if exists != (tc.status == 200) {
				t.Fatalf("list disagrees with request eligibility: %s listed=%t", tc.name, exists)
			}
			if tc.name == "grok-4.6-low" && (entry.DefaultReasoningLevel != "none" || entry.ContextWindow != 256000) {
				t.Fatalf("configured effort-looking name changed metadata: %+v", entry)
			}
			if tc.effort != "" && (entry.DefaultReasoningLevel != tc.effort || len(entry.SupportedReasoningLevels) != 1) {
				t.Fatalf("alias metadata: %+v", entry)
			}
			for _, operation := range []string{"responses", "chat/completions", "messages"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream-%t", operation, stream), func(t *testing.T) {
						body := map[string]any{"model": tc.name, "stream": stream}
						if operation == "responses" {
							body["input"] = "synthetic publication"
						} else {
							body["messages"] = []map[string]string{{"role": "user", "content": "synthetic publication"}}
						}
						if operation == "messages" {
							body["max_tokens"] = 32
						}
						encoded, _ := json.Marshal(body)
						request := httptest.NewRequest("POST", "/v1/"+operation, strings.NewReader(string(encoded)))
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						request.Header.Set("Anthropic-Version", "2023-06-01")
						result := httptest.NewRecorder()
						before := calls.Load()
						fx.router.ServeHTTP(result, request)
						if result.Code != tc.status {
							t.Fatalf("status=%d want=%d body=%s", result.Code, tc.status, result.Body.String())
						}
						if tc.status != 200 {
							if calls.Load() != before {
								t.Fatal("rejected alias reached upstream")
							}
							return
						}
						if calls.Load() != before+1 {
							t.Fatalf("physical calls=%d", calls.Load()-before)
						}
						sent := <-wire
						if sent["model"] != tc.upstream {
							t.Fatalf("wire model=%v want=%s", sent["model"], tc.upstream)
						}
						effort := ""
						if reasoning, ok := sent["reasoning"].(map[string]any); ok {
							effort, _ = reasoning["effort"].(string)
						}
						if effort != tc.effort {
							t.Fatalf("wire effort=%q want=%q", effort, tc.effort)
						}
						if !strings.Contains(result.Body.String(), "published product answer") {
							t.Fatal(result.Body.String())
						}
					})
				}
			}
		})
	}
	// Same content receives 304; a persisted name change invalidates the catalog ETag.
	etag := listed.Header().Get("ETag")
	if etag == "" {
		t.Fatal("missing ETag")
	}
	repeat := catalogRequest.Clone(ctx)
	repeat.Header.Set("If-None-Match", etag)
	cached := httptest.NewRecorder()
	fx.router.ServeHTTP(cached, repeat)
	if cached.Code != 304 || cached.Body.Len() != 0 {
		t.Fatal(cached.Code, cached.Body.String())
	}
	base, err := fx.models.GetByPublicID(ctx, "team-coding")
	if err != nil {
		t.Fatal(err)
	}
	renamed := "Build/team-renamed"
	if _, err := fx.models.Patch(ctx, base.ID, modeldomain.RoutePatch{PublicID: &renamed}); err != nil {
		t.Fatal(err)
	}
	changed := httptest.NewRecorder()
	fx.router.ServeHTTP(changed, repeat)
	if changed.Code != 200 || changed.Header().Get("ETag") == etag || !strings.Contains(changed.Body.String(), `"slug":"team-renamed"`) {
		t.Fatalf("stale catalog: %d %s", changed.Code, changed.Body.String())
	}
}
