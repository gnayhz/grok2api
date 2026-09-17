package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInferenceProtocolModeMatrix(t *testing.T) {
	for _, operation := range []string{conversation.OperationResponses, conversation.OperationChat, conversation.OperationMessages} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", operation, streaming), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer matrix-token" {
						t.Error("missing Build authorization")
					}
					if r.URL.Path != "/v1/responses" {
						t.Errorf("unexpected path %s", r.URL.Path)
					}
					var input map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
					}
					if !strings.Contains(string(input["input"]), "matrix prompt") {
						t.Errorf("lost prompt: %s", input["input"])
					}
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, data := range []string{
							`{"type":"response.created","response":{"id":"resp_matrix","model":"grok-4.3","created_at":123}}`,
							`{"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"matrix thought"}`,
							`{"type":"response.output_text.delta","item_id":"msg_1","delta":"matrix answer"}`,
							`{"type":"response.completed","response":{"id":"resp_matrix","model":"grok-4.3","status":"completed","usage":{"input_tokens":9,"output_tokens":7,"total_tokens":16}}}`,
						} {
							_, _ = io.WriteString(w, "data: "+data+"\n\n")
							w.(http.Flusher).Flush()
						}
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"id":"resp_matrix","model":"grok-4.3","status":"completed","output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"matrix thought"}]},{"type":"message","content":[{"type":"output_text","text":"matrix answer"}]}],"usage":{"input_tokens":9,"output_tokens":7,"total_tokens":16}}`)
					}
				}))
				defer server.Close()
				cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
				if err != nil {
					t.Fatal(err)
				}
				token, err := cipher.Encrypt("matrix-token")
				if err != nil {
					t.Fatal(err)
				}
				adapter := NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
				defer adapter.http.CloseIdleConnections()
				credential := account.Credential{ID: 1, Provider: account.ProviderBuild, AuthType: account.AuthTypeOAuth, EncryptedAccessToken: token}
				body := map[string]any{"model": "grok-4.3", "stream": streaming}
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
				response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: operation, NormalizeBody: true, Streaming: streaming, Body: raw})
				if err != nil {
					t.Fatal(err)
				}
				source := response.Body
				if streaming && response.ConvertStream != nil {
					source = response.ConvertStream(source)
				}
				defer source.Close()
				output, err := io.ReadAll(source)
				if err != nil {
					t.Fatal(err)
				}
				if !streaming && response.ConvertJSON != nil {
					output, err = response.ConvertJSON(output)
					if err != nil {
						t.Fatal(err)
					}
				}
				if response.StatusCode != 200 || !strings.Contains(string(output), "matrix answer") || !strings.Contains(string(output), "matrix thought") {
					t.Fatalf("status=%d body=%s", response.StatusCode, output)
				}
				if streaming {
					terminal := map[string]string{conversation.OperationResponses: `"type":"response.completed"`, conversation.OperationChat: "data: [DONE]", conversation.OperationMessages: `"type":"message_stop"`}[operation]
					if !strings.Contains(string(output), terminal) {
						t.Fatalf("missing terminal %s: %s", terminal, output)
					}
					if !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
						t.Fatal("wrong SSE content type")
					}
				} else if !json.Valid(output) {
					t.Fatalf("invalid JSON: %s", output)
				}
			})
		}
	}
}
