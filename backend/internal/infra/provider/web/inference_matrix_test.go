package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestInferenceProtocolModeMatrix(t *testing.T) {
	for _, operation := range []string{conversation.OperationResponses, conversation.OperationChat, conversation.OperationMessages} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", operation, streaming), func(t *testing.T) {
				server := fhttptest.NewServer(fhttp.HandlerFunc(func(writer fhttp.ResponseWriter, request *fhttp.Request) {
					if request.URL.Path != "/ws/mgw/" {
						fhttp.NotFound(writer, request)
						return
					}
					connection, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(writer, request, nil)
					if err != nil {
						t.Errorf("upgrade: %v", err)
						return
					}
					defer connection.Close()
					var initial map[string]any
					if err := connection.ReadJSON(&initial); err != nil {
						t.Errorf("read session.create: %v", err)
						return
					}
					initialID := initial["event"].(map[string]any)["event_id"].(string)
					_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "session.created", "client_event_id": initialID}})
					_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "conv_1"}}})
					var item map[string]any
					if err := connection.ReadJSON(&item); err != nil {
						t.Errorf("read conversation.item.create: %v", err)
						return
					}
					itemValue := item["event"].(map[string]any)["item"].(map[string]any)
					chunks := itemValue["x_grok"].(map[string]any)["input_chunks"].([]any)
					upstreamMessage, _ := chunks[len(chunks)-1].(map[string]any)["text"].(map[string]any)["text"].(string)
					if !strings.Contains(upstreamMessage, "matrix prompt") {
						t.Errorf("lost prompt: %s", upstreamMessage)
					}
					var create map[string]any
					if err := connection.ReadJSON(&create); err != nil {
						t.Errorf("read response.create: %v", err)
						return
					}
					for _, chunk := range []map[string]any{
						{"text": "matrix thought", "channel": "CHANNEL_ASSISTANT_ANALYSIS"},
						{"text": "matrix answer", "channel": "CHANNEL_ASSISTANT_RESPONSE"},
					} {
						_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.chunk", "chunk": map[string]any{"text": chunk}}})
					}
					_ = connection.WriteJSON(map[string]any{"session_id": "conv_1", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "parent_1", "status": "completed"}}})
				}))
				defer server.Close()
				cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
				if err != nil {
					t.Fatal(err)
				}
				token, err := cipher.Encrypt("test-sso")
				if err != nil {
					t.Fatal(err)
				}
				manager := infraegress.NewManagerWithLimits(egressRepositoryStub{}, cipher, netbudget.Limits{})
				defer manager.Close(context.Background())
				adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", ChatTimeout: 5 * time.Second}, manager, cipher, nil, nil)
				body := map[string]any{"model": "grok-chat-fast", "stream": streaming}
				if operation == conversation.OperationResponses {
					body["input"] = "matrix prompt"
				} else {
					body["messages"] = []any{map[string]any{"role": "user", "content": "matrix prompt"}}
				}
				if operation == conversation.OperationMessages {
					body["max_tokens"] = 2048
					body["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
				}
				raw, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: token}, Method: http.MethodPost, Path: "/responses", Model: "grok-chat-fast", Operation: operation, Streaming: streaming, Body: raw})
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				output, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 || !strings.Contains(string(output), "matrix answer") || !strings.Contains(string(output), "matrix thought") {
					t.Fatalf("status=%d body=%s", response.StatusCode, output)
				}
				if streaming {
					terminal := map[string]string{conversation.OperationResponses: `"type":"response.completed"`, conversation.OperationChat: "data: [DONE]", conversation.OperationMessages: `"type":"message_stop"`}[operation]
					if !strings.Contains(string(output), terminal) {
						t.Fatalf("missing terminal %s: %s", terminal, output)
					}
				} else if !json.Valid(output) {
					t.Fatalf("invalid JSON: %s", output)
				}
			})
		}
	}
}
