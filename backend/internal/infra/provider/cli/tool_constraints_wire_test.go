package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestToolConstraintsWire(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("synthetic-test-token")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, operation, fragment string
		rejected                  bool
		count                     int
		field                     string
		want                      any
	}{
		{"domain", "responses", `"tools":[{"type":"web_search_preview","filters":{"allowed_domains":["example.com"]}}]`, false, 1, "filters", map[string]any{"allowed_domains": []any{"example.com"}}},
		{"chat_options", "chat", `"web_search_options":{"allowed_domains":["example.com"]}`, false, 1, "filters", map[string]any{"allowed_domains": []any{"example.com"}}},
		{"chat_image_off", "chat", `"tools":[{"type":"web_search","enable_image_search":false,"enable_image_understanding":false}]`, false, 1, "enable_image_search", false},
		{"x_bounds", "responses", `"tools":[{"type":"x_search","from_date":"2026-01-01","to_date":"2026-02-01","excluded_x_handles":["blocked"]}]`, false, 1, "excluded_x_handles", []any{"blocked"}},
		{"forced_hosted", "responses", `"tools":[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"web_search"}`, false, 1, "type", "web_search"},
		{"messages_forced_hosted", "messages", `"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"web_search"}`, false, 1, "type", "web_search"},
		{"required_hosted", "responses", `"tools":[{"type":"web_search"}],"tool_choice":"required"`, false, 1, "type", "web_search"},
		{"mcp_whitelist", "responses", `"tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.com/mcp","allowed_tools":["lookup"],"require_approval":"never"}]`, false, 1, "allowed_tools", []any{"lookup"}},
		{"mcp_messages_whitelist", "messages", `"mcp_servers":[{"name":"docs","url":"https://example.com/mcp","tool_configuration":{"allowed_tools":["lookup"]}}]`, false, 1, "allowed_tools", []any{"lookup"}},
		{"mcp_disabled", "messages", `"mcp_servers":[{"name":"docs","url":"https://example.com/mcp","tool_configuration":{"enabled":false}}]`, false, 0, "", nil},
		{"mcp_empty", "messages", `"mcp_servers":[{"name":"docs","url":"https://example.com/mcp","tool_configuration":{"allowed_tools":[]}}]`, false, 0, "", nil},
		{"mcp_native_empty", "responses", `"tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.com/mcp","allowed_tools":[]}]`, false, 0, "", nil},
		{"mcp_forced_server", "responses", `"tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.com/mcp","allowed_tools":["lookup"]},{"type":"mcp","server_label":"other","server_url":"https://other.example/mcp"}],"tool_choice":{"type":"mcp","server_label":"docs"}`, false, 1, "server_label", "docs"},
		{"no_external", "responses", `"tools":[{"type":"web_search","external_web_access":false}]`, true, 0, "external_web_access", nil},
		{"no_external_chat", "chat", `"web_search_options":{"external_web_access":false}`, true, 0, "external_web_access", nil},
		{"unknown_filter", "responses", `"tools":[{"type":"web_search","filters":{"blocked_domains":["example.com"]}}]`, true, 0, "filters", nil},
		{"unknown_filter_chat", "chat", `"tools":[{"type":"web_search","filters":{"blocked_domains":["example.com"]}}]`, true, 0, "filters", nil},
		{"search_limit", "responses", `"tools":[{"type":"web_search","max_search_results":1}]`, true, 0, "max_search_results", nil},
		{"search_max_uses", "messages", `"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}]`, true, 0, "max_uses", nil},
		{"messages_unknown_control", "messages", `"tools":[{"type":"web_search_20250305","name":"web_search","safe_search":true}]`, true, 0, "safe_search", nil},
		{"inverted_date", "responses", `"tools":[{"type":"x_search","from_date":"2026-02-01","to_date":"2026-01-01"}]`, true, 0, "from_date", nil},
		{"invalid_date_chat", "chat", `"tools":[{"type":"x_search","from_date":"yesterday"}]`, true, 0, "from_date", nil},
		{"required_no_tools", "responses", `"tool_choice":"required"`, true, 0, "tool_choice", nil},
		{"required_no_tools_chat", "chat", `"tool_choice":"required"`, true, 0, "tool_choice", nil},
		{"required_no_tools_messages", "messages", `"tool_choice":{"type":"any"}`, true, 0, "tool_choice", nil},
		{"forced_missing", "responses", `"tools":[{"type":"web_search"}],"tool_choice":{"type":"function","name":"missing"}`, true, 0, "tool_choice", nil},
		{"mcp_approval", "responses", `"tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.com/mcp","require_approval":"always"}]`, true, 0, "require_approval", nil},
		{"mcp_approval_chat", "chat", `"tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.com/mcp","require_approval":"always"}]`, true, 0, "require_approval", nil},
		{"mcp_filter_object", "responses", `"tools":[{"type":"mcp","server_label":"docs","server_url":"https://example.com/mcp","allowed_tools":{"read_only":true}}]`, true, 0, "allowed_tools", nil},
		{"mcp_disabled_required", "messages", `"mcp_servers":[{"name":"docs","url":"https://example.com/mcp","tool_configuration":{"enabled":false}}],"tool_choice":{"type":"any"}`, true, 0, "tool_choice", nil},
		{"mcp_unknown_configuration", "messages", `"mcp_servers":[{"name":"docs","url":"https://example.com/mcp","tool_configuration":{"read_only":true}}]`, true, 0, "tool_configuration", nil},
		{"mcp_new_toolset", "messages", `"tools":[{"type":"mcp_toolset","mcp_server_name":"docs","default_config":{"enabled":false}}]`, true, 0, "mcp_toolset", nil},
		{"duplicate_hosted", "responses", `"tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}},{"type":"web_search"}]`, true, 0, "tools", nil},
		{"search_defaults", "responses", `"tools":[{"type":"web_search"}]`, false, 1, "enable_image_understanding", nil},
		{"x_defaults", "responses", `"tools":[{"type":"x_search"}]`, false, 1, "enable_video_understanding", nil},
		{"cross_image_conflict", "responses", `"tools":[{"type":"web_search","enable_image_understanding":true},{"type":"x_search","enable_image_understanding":false}]`, true, 0, "tools", nil},
	}

	for _, test := range cases {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%t", test.name, stream), func(t *testing.T) {
				var captured map[string]any
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)

					if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
						t.Error(err)
					}
					answer := map[string]any{"id": "resp_constraints", "object": "response", "status": "completed", "model": "grok-4.5", "output": []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "constraint response"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}}
					if stream {
						w.Header().Set("Content-Type", "text/event-stream")
						data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": answer})
						fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", data)
					} else {
						w.Header().Set("Content-Type", "application/json")
						_ = json.NewEncoder(w).Encode(answer)
					}
				}))
				defer server.Close()
				adapter := NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
				defer adapter.http.CloseIdleConnections()
				credential := account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}
				var payload map[string]any
				if err := json.Unmarshal([]byte(`{`+test.fragment+`}`), &payload); err != nil {
					t.Fatal(err)
				}
				payload["stream"] = stream
				payload["model"] = "grok-4.5"
				if test.operation == "responses" {
					payload["input"] = "hello"
				} else {
					payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
				}
				if test.operation == "messages" {
					payload["max_tokens"] = 128
				}
				body, _ := json.Marshal(payload)
				response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Operation: test.operation, Model: "grok-4.5", Body: body, NormalizeBody: true, Streaming: stream})
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if test.rejected {
					output, _ := io.ReadAll(response.Body)
					if response.StatusCode != 400 || calls.Load() != 0 || !strings.Contains(string(output), test.field) {
						t.Fatalf("status=%d calls=%d body=%s", response.StatusCode, calls.Load(), output)
					}
					return
				}
				source := response.Body
				if stream && response.ConvertStream != nil {
					source = response.ConvertStream(source)
					defer source.Close()
				}
				output, err := io.ReadAll(source)
				if err == nil && !stream && response.ConvertJSON != nil {
					output, err = response.ConvertJSON(output)
				}
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != 200 || captured == nil || !strings.Contains(string(output), "constraint response") {
					t.Fatalf("status=%d body=%s", response.StatusCode, output)
				}
				tools, _ := captured["tools"].([]any)
				if len(tools) != test.count {
					t.Fatalf("wire tools=%v", tools)
				}
				if test.count > 0 && !reflect.DeepEqual(tools[0].(map[string]any)[test.field], test.want) {
					t.Fatalf("wire constraint lost: %+v; %s want %v", tools, test.field, test.want)
				}
				if strings.Contains(test.name, "forced") || test.name == "required_hosted" {
					if captured["tool_choice"] != "required" {
						t.Fatalf("mandatory choice lost: %v", captured["tool_choice"])
					}
				}
			})
		}
	}
}
