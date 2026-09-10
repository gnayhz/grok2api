package inference

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
)

// The only grant disappears through the real M05 lifecycle. All subsequent
// requests authenticate from SQL, so the assertion also covers a fresh process.
func deleteOnlyClientModelGrant(t *testing.T, fx voiceCompletionFixture) {
	t.Helper()
	ctx := context.Background()
	grant, err := fx.models.Create(ctx, model.Route{Provider: fx.account.Provider, PublicID: "removed-only-grant", UpstreamModel: "removed-upstream", Capability: model.CapabilityResponses, Enabled: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ids := []uint64{grant.ID}
	if _, err := fx.clients.Patch(ctx, fx.created.Key.ID, clientkey.ManagementPatch{AllowedModels: &ids}); err != nil {
		t.Fatal(err)
	}
	if err := fx.models.Delete(ctx, grant.ID); err != nil {
		t.Fatal(err)
	}
	key, err := fx.clients.Get(ctx, fx.created.Key.ID)
	if err != nil || key.ModelScope != clientkey.ModelScopeRestricted || len(key.AllowedModels) != 0 {
		t.Fatalf("deleted grant state: %+v %v", key, err)
	}
}

func TestDeletedLastGrantDeniesEveryTextProtocol(t *testing.T) {
	for _, provider := range []account.Provider{account.ProviderBuild, account.ProviderWeb, account.ProviderConsole} {
		t.Run(string(provider), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer upstream.Close()
			upstreamModel := map[account.Provider]string{account.ProviderBuild: "grok-4.5", account.ProviderConsole: "grok-4.3", account.ProviderWeb: "grok-chat-fast"}[provider]
			fx := newProviderCompletionFixture(t, upstream.URL, upstreamModel, provider, nil, nil)
			deleteOnlyClientModelGrant(t, fx)
			for _, protocol := range []string{"responses", "chat/completions", "messages"} {
				for _, stream := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/stream=%v", protocol, stream), func(t *testing.T) {
						payload := map[string]any{"model": fx.publicModel, "stream": stream}
						if protocol == "responses" {
							payload["input"] = "synthetic"
						} else {
							payload["messages"] = []map[string]string{{"role": "user", "content": "synthetic"}}
							payload["max_tokens"] = 16
						}
						body, _ := json.Marshal(payload)
						request := httptest.NewRequest("POST", "/v1/"+protocol, strings.NewReader(string(body)))
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						if protocol == "messages" {
							request.Header.Set("anthropic-version", "2023-06-01")
						}
						response := httptest.NewRecorder()
						fx.router.ServeHTTP(response, request)
						if response.Code != 403 || calls.Load() != 0 {
							t.Fatalf("scope bypass: %d %s upstream=%d", response.Code, response.Body.String(), calls.Load())
						}
					})
				}
			}
			request := httptest.NewRequest("GET", "/v1/models", nil)
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			response := httptest.NewRecorder()
			fx.router.ServeHTTP(response, request)
			var list struct {
				Data []json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &list); err != nil || response.Code != 200 || len(list.Data) != 0 {
				t.Fatalf("restricted listing: %d %s %v", response.Code, response.Body.String(), err)
			}
		})
	}
}

func TestDeletedLastGrantDeniesMediaAndAudio(t *testing.T) {
	for _, entry := range []struct {
		path, upstreamModel, payload string
		provider                     account.Provider
	}{
		{"images/generations", "grok-imagine-image", `{"prompt":"synthetic","response_format":"b64_json"}`, account.ProviderConsole},
		{"images/edits", "grok-imagine-image", `{"prompt":"synthetic","image":{"url":"data:image/png;base64,` + completionImagePNG + `"}}`, account.ProviderConsole},
		{"videos/generations", "grok-imagine-video", `{"prompt":"synthetic","duration":6}`, account.ProviderWeb},
		{"tts", "grok-voice-latest", `{"text":"synthetic","language":"en"}`, account.ProviderConsole},
		{"audio/speech", "grok-voice-latest", `{"input":"synthetic","voice":"alloy"}`, account.ProviderConsole},
		{"audio/tasks", "grok-voice-latest", `{"input":"synthetic","voice":"alloy"}`, account.ProviderConsole},
		{"stt", "grok-stt", `{"url":"https://example.invalid/synthetic.wav"}`, account.ProviderConsole},
		{"audio/transcriptions", "grok-stt", `{"url":"https://example.invalid/synthetic.wav"}`, account.ProviderConsole},
	} {
		t.Run(entry.path, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer upstream.Close()
			fx := newProviderCompletionFixture(t, upstream.URL, entry.upstreamModel, entry.provider, nil, nil)
			if entry.path == "videos/generations" {
				fx.service.ConfigureMedia(fx.jobs, 1)
			}
			deleteOnlyClientModelGrant(t, fx)
			var payload map[string]any
			if err := json.Unmarshal([]byte(entry.payload), &payload); err != nil {
				t.Fatal(err)
			}
			payload["model"] = fx.publicModel
			body, _ := json.Marshal(payload)
			request := httptest.NewRequest("POST", "/v1/"+entry.path, strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			response := httptest.NewRecorder()
			fx.router.ServeHTTP(response, request)
			if response.Code != 403 || calls.Load() != 0 {
				t.Fatalf("scope bypass: %d %s upstream=%d", response.Code, response.Body.String(), calls.Load())
			}
		})
	}
}

func TestDeletedLastGrantDeniesVoiceWebSocketBeforeUpgrade(t *testing.T) {
	for _, entry := range []struct{ path, model string }{{"realtime", "grok-voice-latest"}, {"stt", "grok-stt"}} {
		t.Run(entry.path, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(500) }))
			defer upstream.Close()
			fx := newVoiceCompletionFixture(t, upstream.URL, entry.model)
			deleteOnlyClientModelGrant(t, fx)
			request := httptest.NewRequest("GET", "/v1/"+entry.path+"?model="+fx.publicModel, nil)
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			request.Header.Set("Connection", "Upgrade")
			request.Header.Set("Upgrade", "websocket")
			request.Header.Set("Sec-WebSocket-Version", "13")
			request.Header.Set("Sec-WebSocket-Key", "c3ludGhldGljZml4dHVyZQ==")
			response := httptest.NewRecorder()
			fx.router.ServeHTTP(response, request)
			if response.Code != 403 || calls.Load() != 0 {
				t.Fatalf("socket scope bypass: %d %s upstream=%d", response.Code, response.Body.String(), calls.Load())
			}
		})
	}
}
