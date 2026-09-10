package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

func TestCompactionRetainsExplicitToolChoice(t *testing.T) {
	functions := `[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"save","parameters":{"type":"object"}}]`
	for _, tc := range []struct {
		name, tools, choice, want string
		count                     int
		invalid                   bool
	}{
		{"none", functions, `"none"`, `"none"`, 2, false},
		{"required", functions, `"required"`, `"required"`, 2, false},
		{"function", functions, `{"type":"function","name":"lookup"}`, `{"type":"function","name":"lookup"}`, 2, false},
		{"auto", functions, `"auto"`, `"auto"`, 2, false},
		{"unspecified", functions, ``, `"auto"`, 2, false},
		{"null", functions, `null`, `"auto"`, 2, false},
		{"hosted", `[{"type":"web_search"},{"type":"function","name":"lookup","parameters":{"type":"object"}}]`, `{"type":"web_search"}`, `"required"`, 1, false},
		{"mcp", `[{"type":"mcp","server_label":"docs","server_url":"https://example.test/mcp","allowed_tools":["lookup"],"require_approval":"never"},{"type":"function","name":"save","parameters":{"type":"object"}}]`, `{"type":"mcp","server_label":"docs"}`, `"required"`, 1, false},
		{"no_tools_none", `[]`, `"none"`, `null`, 0, false},
		{"required_no_tools", `[]`, `"required"`, `null`, 0, true},
		{"missing_function", functions, `{"type":"function","name":"undeclared"}`, `null`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := make(chan map[string]json.RawMessage, 1)
			var calls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				data, _ := io.ReadAll(r.Body)
				var payload map[string]json.RawMessage
				_ = json.Unmarshal(data, &payload)
				wire <- payload
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, compactionSampleSSE("resp_compaction_tools", healthyCompactionSummary()))
			}))
			defer server.Close()
			adapter, encrypted := newCompactionTestAdapter(t)
			cfg := adapter.config()
			cfg.BaseURL = server.URL
			cfg.FallbackBaseURL = ""
			adapter.UpdateConfig(cfg)
			choice := ""
			if tc.choice != "" {
				choice = `,"tool_choice":` + tc.choice
			}
			request := provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5", Operation: "responses", NormalizeBody: true, Body: []byte(`{"input":[{"role":"user","content":"summarize"},{"type":"compaction_trigger"}],"tools":` + tc.tools + choice + `}`)}
			response, err := adapter.ForwardResponse(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, _ = io.Copy(io.Discard, response.Body)
			if tc.invalid {
				if calls.Load() != 0 || response.RequestValidation == nil || response.StatusCode != 400 {
					t.Fatalf("invalid input calls=%d status=%d", calls.Load(), response.StatusCode)
				}
				return
			}
			if calls.Load() != 1 || response.StatusCode != 200 {
				t.Fatalf("calls=%d status=%d", calls.Load(), response.StatusCode)
			}
			payload := <-wire
			var got, want any
			var tools []any
			if raw := payload["tool_choice"]; len(raw) > 0 {
				if err = json.Unmarshal(raw, &got); err != nil {
					t.Fatal(err)
				}
			}
			if err = json.Unmarshal([]byte(tc.want), &want); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("summary generation changed tool permission: got=%v want=%v", got, want)
			}
			if raw := payload["tools"]; len(raw) > 0 {
				if err = json.Unmarshal(raw, &tools); err != nil {
					t.Fatal(err)
				}
			}
			if len(tools) != tc.count {
				t.Fatalf("tools=%d want=%d", len(tools), tc.count)
			}
		})
	}
}
