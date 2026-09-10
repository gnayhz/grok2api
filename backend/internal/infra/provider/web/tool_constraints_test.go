package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestWebSearchConstraintsRejectedBeforeNetwork(t *testing.T) {
	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	encrypted, _ := cipher.Encrypt("test-sso")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(500) }))
	defer server.Close()
	manager := infraegress.NewManager(egressRepositoryStub{}, cipher)
	defer manager.Close(context.Background())
	adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual"}, manager, cipher, nil, nil)
	for _, operation := range []string{"responses", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, field := range []string{"allowed_domains", "max_uses", "enable_image_search", "external_web_access"} {
				t.Run(operation+"/"+field+"/"+map[bool]string{true: "SSE", false: "JSON"}[stream], func(t *testing.T) {
					tool := map[string]any{"type": "web_search"}
					switch field {
					case "allowed_domains":
						tool[field] = []any{"example.com"}
					case "max_uses":
						tool[field] = 1
					default:
						tool[field] = false
					}
					payload := map[string]any{"model": "grok-chat-fast", "stream": stream, "tools": []any{tool}}
					if operation == "responses" {
						payload["input"] = "hello"
					} else {
						payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
					}
					if operation == "messages" {
						payload["max_tokens"] = 128
						tool["type"] = "web_search_20250305"
						tool["name"] = "web_search"
					}
					if operation == "chat" {
						delete(payload, "tools")
						delete(tool, "type")
						payload["web_search_options"] = tool
					}
					body, _ := json.Marshal(payload)
					response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Operation: operation, Model: "grok-chat-fast", Body: body, NormalizeBody: true, Streaming: stream})
					if err != nil {
						t.Fatal(err)
					}
					defer response.Body.Close()
					output, _ := io.ReadAll(response.Body)
					expectedField := field
					if operation == "messages" && field == "allowed_domains" {
						expectedField = "filters"
					}
					if response.StatusCode != 400 || calls.Load() != 0 || !strings.Contains(string(output), expectedField) {
						t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), output)
					}
				})
			}
		}
	}
}

func TestWebForcedSearchRemovesClientToolPrompt(t *testing.T) {
	configuration, err := parseToolConfiguration(json.RawMessage(`[{"type":"web_search"},{"type":"function","name":"charge","parameters":{"type":"object"}}]`), json.RawMessage(`{"type":"web_search"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Functions) != 0 || configuration.Choice != "required" || !configuration.HostedWebSearch || strings.Contains(injectToolPrompt("hello", configuration), "charge") {
		t.Fatalf("forced search broadened: %+v", configuration)
	}
	if _, err := parseToolConfiguration(json.RawMessage(`[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]`), json.RawMessage(`"auto"`)); err == nil {
		t.Fatal("web browser cannot enforce Responses domain filters")
	}
	if _, err := parseToolConfiguration(json.RawMessage(`[{"type":"web_search"}]`), json.RawMessage(`"none"`)); err == nil {
		t.Fatal("web browser cannot disable hosted search")
	}
}
