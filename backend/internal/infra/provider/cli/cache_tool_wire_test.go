package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestBuildCacheToolPermissionWire(t *testing.T) {
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("synthetic-access-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"responses", "chat", "messages"} {
		for _, stream := range []bool{false, true} {
			for _, tier := range []string{"unknown", "free", "paid"} {
				for _, cache := range []bool{false, true} {
					for _, scenario := range []string{"no_tools", "none", "default", "auto", "required", "forced", "web_search", "x_search"} {
						if operation == "messages" && scenario == "x_search" {
							continue
						}
						t.Run(fmt.Sprintf("%s/stream=%t/%s/cache=%t/%s", operation, stream, tier, cache, scenario), func(t *testing.T) {
							payload, wantTools, wantChoice := cacheWirePayload(operation, scenario, stream)
							var captured map[string]any
							var calls atomic.Int32
							var planned atomic.Bool
							server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
								calls.Add(1)
								if !planned.Load() {
									t.Error("wire request preceded plan review")
								}
								if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
									t.Error(err)
								}
								answer := map[string]any{"id": "resp_cache_permission", "object": "response", "status": "completed", "model": "grok-4.6", "tools": captured["tools"], "output": []any{map[string]any{"type": "message", "id": "msg_1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "permission preserved"}}}}, "usage": map[string]any{"input_tokens": 3, "output_tokens": 2}}
								if stream {
									w.Header().Set("Content-Type", "text/event-stream")
									fmt.Fprintf(w, "event: response.completed\ndata: %s\n\n", mustJSON(map[string]any{"type": "response.completed", "response": answer}))
								} else {
									w.Header().Set("Content-Type", "application/json")
									_ = json.NewEncoder(w).Encode(answer)
								}
							}))
							defer server.Close()
							adapter := NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
							defer adapter.http.CloseIdleConnections()
							request := provider.ResponseResourceRequest{Credential: account.Credential{ID: 7, Provider: account.ProviderBuild, EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Body: mustJSON(payload), Model: "grok-4.6", Operation: operation, NormalizeBody: true, Streaming: stream, ToolCompatibilityPolicy: inferencedomain.AllowDisabledCacheTools}
							if tier == "free" {
								request.Credential.ObservedModel = "grok-4.6-build-free"
							}
							if tier == "paid" {
								request.Credential.BuildSuperEntitled = true
							}
							if cache {
								request.PromptCacheKey = "synthetic-cache-session"
							}
							request.OnNormalized = func(metadata provider.NormalizedRequestMetadata) error {
								if metadata.ToolCompatibility == nil {
									return errors.New("missing tool compatibility plan")
								}
								plan := *metadata.ToolCompatibility
								if err := plan.Validate(request.ToolCompatibilityPolicy); err != nil {
									return err
								}
								wantAdded := cache && scenario == "no_tools"
								if (len(plan.AddedCacheTools) > 0) != wantAdded || (wantAdded && !plan.ExecutionDisabled) {
									return fmt.Errorf("unexpected tool plan: %+v", plan)
								}
								planned.Store(true)
								return nil
							}
							response, err := adapter.ForwardResponse(context.Background(), request)
							if err != nil {
								t.Fatal(err)
							}
							defer response.Body.Close()
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
							if response.StatusCode != http.StatusOK || calls.Load() != 1 || !strings.Contains(string(output), "permission preserved") {
								t.Fatalf("status=%d calls=%d output=%s", response.StatusCode, calls.Load(), output)
							}
							if scenario == "no_tools" && cache {
								wantTools = []string{"web_search", "x_search"}
								wantChoice = "none"
							}
							var toolTypes []string
							if tools, ok := captured["tools"].([]any); ok {
								for _, raw := range tools {
									toolTypes = append(toolTypes, stringField(raw.(map[string]any), "type"))
								}
							}
							if !reflect.DeepEqual(toolTypes, wantTools) || !reflect.DeepEqual(captured["tool_choice"], wantChoice) {
								t.Fatalf("wire tool grant: tools=%v choice=%v; want %v %v", toolTypes, captured["tool_choice"], wantTools, wantChoice)
							}
							if scenario == "web_search" {
								tool := captured["tools"].([]any)[0].(map[string]any)
								if !reflect.DeepEqual(tool["filters"], map[string]any{"allowed_domains": []any{"example.com"}}) {
									t.Fatalf("search restriction lost: %+v", tool)
								}
							}
							if scenario == "no_tools" && (strings.Contains(string(output), `"type":"x_search"`) || strings.Contains(string(output), `"type":"web_search"`)) {
								t.Fatalf("cache-only declarations leaked: %s", output)
							}
						})
					}
				}
			}
		}
	}
}

func cacheWirePayload(operation, scenario string, stream bool) (map[string]any, []string, any) {
	payload := map[string]any{"model": "grok-4.6", "stream": stream}
	if operation == "responses" {
		payload["input"] = "hello"
	} else {
		payload["messages"] = []any{map[string]any{"role": "user", "content": "hello"}}
	}
	if operation == "messages" {
		payload["max_tokens"] = 128
	}
	if scenario == "no_tools" {
		return payload, nil, nil
	}
	tool := map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}
	if operation == "chat" {
		tool = map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "parameters": map[string]any{"type": "object"}}}
	}
	if operation == "messages" {
		tool = map[string]any{"name": "lookup", "input_schema": map[string]any{"type": "object"}}
	}
	wantTools := []string{"function"}
	if scenario == "web_search" {
		tool = map[string]any{"type": "web_search", "filters": map[string]any{"allowed_domains": []any{"example.com"}}}
		if operation == "messages" {
			tool = map[string]any{"type": "web_search_20250305", "name": "web_search", "allowed_domains": []any{"example.com"}}
		}
		wantTools = []string{"web_search"}
	}
	if scenario == "x_search" {
		tool = map[string]any{"type": "x_search"}
		wantTools = []string{"x_search"}
	}
	payload["tools"] = []any{tool}
	var wantChoice any
	switch scenario {
	case "none", "auto", "required":
		wantChoice = scenario
		payload["tool_choice"] = scenario
		if operation == "messages" {
			kind := scenario
			if kind == "required" {
				kind = "any"
			}
			payload["tool_choice"] = map[string]any{"type": kind}
		}
	case "forced":
		wantChoice = map[string]any{"type": "function", "name": "lookup"}
		payload["tool_choice"] = wantChoice
		if operation == "chat" {
			payload["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup"}}
		}
		if operation == "messages" {
			payload["tool_choice"] = map[string]any{"type": "tool", "name": "lookup"}
		}
	}
	return payload, wantTools, wantChoice
}

func TestBuildCacheToolPlanCanAbortBeforeNetwork(t *testing.T) {
	cipher, _ := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	encrypted, _ := cipher.Encrypt("synthetic-token")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer server.Close()
	adapter := NewAdapter(Config{BaseURL: server.URL + "/v1"}, cipher)
	defer adapter.http.CloseIdleConnections()
	rejected := errors.New("request owner rejected plan")
	_, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: account.Credential{EncryptedAccessToken: encrypted}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.6", Body: []byte(`{"input":"hello"}`), NormalizeBody: true, PromptCacheKey: "cache", ToolCompatibilityPolicy: inferencedomain.AllowDisabledCacheTools, OnNormalized: func(metadata provider.NormalizedRequestMetadata) error { return rejected }})
	if !errors.Is(err, rejected) || calls.Load() != 0 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}
