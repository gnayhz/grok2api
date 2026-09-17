package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/streampipe"
)

func TestPrepareBuildPromptCacheRouteToolFree(t *testing.T) {
	body, route, err := prepareBuildPromptCacheRoute([]byte(`{"model":"grok-4.5","input":"hello"}`), "responses", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 2 || stringField(tools[0].(map[string]any), "type") != "web_search" || stringField(tools[1].(map[string]any), "type") != "x_search" {
		t.Fatalf("tools = %#v", tools)
	}
	if payload["tool_choice"] != "none" || !route.filterXSearch || len(route.injectedToolTypes) != 2 {
		t.Fatalf("route = %#v payload = %#v", route, payload)
	}
}

func TestPrepareBuildPromptCacheRoutePreservesToolBearingRequest(t *testing.T) {
	body, route, err := prepareBuildPromptCacheRoute([]byte(`{
		"model":"grok-4.5","input":"hello","tool_choice":"auto",
		"tools":[{"type":"function","name":"Read","parameters":{"type":"object"}}]
	}`), "messages", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 || payload["tool_choice"] != "auto" {
		t.Fatalf("payload = %#v", payload)
	}
	if _, ok := route.clientDeclaredTools["Read"]; !ok || len(route.injectedToolTypes) != 0 || route.filterXSearch {
		t.Fatalf("route = %#v", route)
	}
}

func TestPrepareBuildPromptCacheRoutePreservesLargeIntegers(t *testing.T) {
	body, _, err := prepareBuildPromptCacheRoute([]byte(`{"input":"hello","metadata":{"sequence":9007199254740993}}`), "responses", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"sequence":9007199254740993`) {
		t.Fatalf("large integer changed during cache route rewrite: %s", body)
	}
}

func TestPrepareBuildPromptCacheRouteDoesNotBroadenUntrustedFunctions(t *testing.T) {
	body, route, err := prepareBuildPromptCacheRoute([]byte(`{
		"input":"hello","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]
	}`), "responses", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"x_search"`) || route.filterXSearch || len(route.injectedToolTypes) != 0 {
		t.Fatalf("untrusted function request was broadened: body=%s route=%#v", body, route)
	}
}

func TestPrepareBuildPromptCacheRoutePreservesExistingSearch(t *testing.T) {
	body, route, err := prepareBuildPromptCacheRoute([]byte(`{"input":"hello","tools":[{"type":"web_search"}]}`), "responses", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"x_search"`) || route.filterXSearch {
		t.Fatalf("existing search permissions changed: body=%s route=%#v", body, route)
	}
}

func TestPrepareBuildPromptCacheRoutePreservesAllowedTools(t *testing.T) {
	body, _, err := prepareBuildPromptCacheRoute([]byte(`{
		"input":"hello",
		"tools":[{"type":"function","name":"Read"}],
		"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"Read"}]}
	}`), "responses", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	choice := payload["tool_choice"].(map[string]any)
	allowed := choice["tools"].([]any)
	if len(allowed) != 1 || stringField(allowed[0].(map[string]any), "type") != "function" {
		t.Fatalf("allowed tools = %#v", allowed)
	}
}

func TestPrepareBuildPromptCacheRoutePreservesExplicitXSearch(t *testing.T) {
	body, route, err := prepareBuildPromptCacheRoute([]byte(`{
		"input":"hello","tools":[{"type":"x_search"}],"tool_choice":"auto"
	}`), "responses", "grok-4.5", "cache-key", inferencedomain.AllowDisabledCacheTools)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 1 || stringField(tools[0].(map[string]any), "type") != "x_search" || payload["tool_choice"] != "auto" {
		t.Fatalf("explicit x_search changed: %#v", payload)
	}
	if !route.filterXSearch || len(route.injectedToolTypes) != 0 {
		t.Fatalf("explicit x_search route = %#v", route)
	}
}

func TestPrepareBuildPromptCacheRouteRespectsBoundaries(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		operation string
		model     string
		key       string
	}{
		{name: "no cache identity", body: `{"input":"hello"}`, operation: "responses", model: "grok-4.5"},
		{name: "compaction", body: `{"input":"hello"}`, operation: "compaction", model: "grok-4.5", key: "key"},
		{name: "image operation", body: `{"input":"hello"}`, operation: "image", model: "grok-4.5", key: "key"},
		{name: "image model", body: `{"input":"hello"}`, operation: "responses", model: "grok-imagine-1.0", key: "key"},
		{name: "image tool", body: `{"input":"hello","tools":[{"type":"image_generation"}]}`, operation: "responses", model: "grok-4.5", key: "key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, route, err := prepareBuildPromptCacheRoute([]byte(test.body), test.operation, test.model, test.key, inferencedomain.AllowDisabledCacheTools)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(body), `"x_search"`) || route.filterXSearch || len(route.injectedToolTypes) != 0 {
				t.Fatalf("unexpected cache route: body=%s route=%#v", body, route)
			}
		})
	}
}

func TestFilterBuildPromptCacheResponseJSON(t *testing.T) {
	route := buildPromptCacheRoute{
		filterXSearch:       true,
		injectedToolTypes:   map[string]struct{}{"x_search": {}},
		clientDeclaredTools: map[string]struct{}{"x_keyword_search": {}},
	}
	response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{
		"id":"resp_1",
		"usage":{"cost_in_usd_ticks":9007199254740993},
		"tools":[{"type":"function","name":"x_keyword_search"},{"type":"x_search"}],
		"output":[
			{"type":"custom_tool_call","id":"internal","call_id":"xs_call-1","name":"x_keyword_search"},
			{"type":"custom_tool_call","id":"client_custom","call_id":"call_custom_1","name":"x_keyword_search","input":"{}"},
			{"type":"function_call","id":"client","call_id":"call_1","name":"x_keyword_search","arguments":"{}"},
			{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"done"}]}
		]
	}`))}
	if err := filterBuildPromptCacheResponse(response, false, route); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, `"id":"internal"`) || strings.Contains(text, `"type":"x_search"`) || !strings.Contains(text, `"id":"client_custom"`) || !strings.Contains(text, `"id":"client"`) || !strings.Contains(text, `"id":"msg_1"`) || !strings.Contains(text, `"cost_in_usd_ticks":9007199254740993`) {
		t.Fatalf("filtered response = %s", text)
	}
	if response.ContentLength != int64(len(data)) || response.Header.Get("Content-Length") != strconv.Itoa(len(data)) {
		t.Fatalf("content length = %d header=%q want=%d", response.ContentLength, response.Header.Get("Content-Length"), len(data))
	}
}

func TestFilterBuildPromptCacheResponseDropsInjectedWebSearch(t *testing.T) {
	route := buildPromptCacheRoute{
		injectedToolTypes:   map[string]struct{}{"web_search": {}},
		clientDeclaredTools: map[string]struct{}{},
	}
	response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{
		"output":[{"type":"web_search_call","id":"ws_1"},{"type":"message","id":"msg_1"}]
	}`))}
	if err := filterBuildPromptCacheResponse(response, false, route); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "web_search_call") || !strings.Contains(string(data), `"id":"msg_1"`) {
		t.Fatalf("injected web search trace leaked: %s", data)
	}
}

func TestFilterBuildPromptCacheResponseStream(t *testing.T) {
	route := buildPromptCacheRoute{
		filterXSearch:       true,
		injectedToolTypes:   map[string]struct{}{"x_search": {}},
		clientDeclaredTools: map[string]struct{}{},
	}
	source := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"xs_call-1","name":"x_semantic_search"}}`, "",
		`event: response.custom_tool_call_input.delta`,
		`data: {"type":"response.custom_tool_call_input.delta","output_index":1,"item_id":"ctc_1","delta":"{}"}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"message","id":"msg_1"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"tools":[{"type":"x_search"}],"output":[{"type":"custom_tool_call","id":"ctc_1","call_id":"xs_call-1","name":"x_semantic_search"},{"type":"message","id":"msg_1"}]}}`, "", "",
	}, "\n")
	response := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(source))}
	if err := filterBuildPromptCacheResponse(response, true, route); err != nil {
		t.Fatal(err)
	}
	if response.ContentLength != -1 || response.Header.Get("Content-Length") != "" {
		t.Fatalf("stream content length = %d header=%q", response.ContentLength, response.Header.Get("Content-Length"))
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "x_semantic_search") || strings.Contains(text, "ctc_1") || strings.Contains(text, `"type":"x_search"`) {
		t.Fatalf("internal x_search trace leaked:\n%s", text)
	}
	if !strings.Contains(text, `"output_index":1`) || !strings.Contains(text, `"id":"msg_1"`) {
		t.Fatalf("remaining output was not compacted:\n%s", text)
	}
}

// maxCompatibleResponseBytes 与 filterBuildPromptCacheResponse 是测试缝:
// 生产路径在 response_conversion.go 内联消费 buildXSearchResponseFilter
// (流式走 filterEvent、非流式走 filterJSON);这里保留独立 http.Response
// 级别的编排入口,供过滤行为的 JSON/流式两种断言直接复用。
const maxCompatibleResponseBytes = 128 << 20

func filterBuildPromptCacheResponse(response *http.Response, streaming bool, route buildPromptCacheRoute) error {
	if response == nil || response.Body == nil || (!route.filterXSearch && len(route.injectedToolTypes) == 0) {
		return nil
	}
	filter := newBuildXSearchResponseFilter(route)
	if streaming {
		response.Body = filter.stream(response.Body)
		response.Header.Del("Content-Length")
		response.ContentLength = -1
		return nil
	}
	source := response.Body
	data, err := io.ReadAll(io.LimitReader(source, maxCompatibleResponseBytes+1))
	_ = source.Close()
	if err != nil {
		return err
	}
	if len(data) > maxCompatibleResponseBytes {
		return fmt.Errorf("Grok Build Responses 响应超过 %d MiB", maxCompatibleResponseBytes>>20)
	}
	filtered, err := filter.filterJSON(data)
	if err != nil {
		return err
	}
	response.Body = io.NopCloser(bytes.NewReader(filtered))
	response.Header.Set("Content-Length", strconv.Itoa(len(filtered)))
	response.ContentLength = int64(len(filtered))
	return nil
}

func (f *buildXSearchResponseFilter) stream(source io.ReadCloser) io.ReadCloser {
	return streampipe.Transform(source, func(input io.Reader, writer io.Writer) error {
		return consumeCompatibleSSE(input, func(event compatibleSSEEvent) error {
			if !event.HasData() {
				return event.writeTo(writer)
			}
			data := event.Data()
			if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
				return event.writeTo(writer)
			}
			filtered, keep, filterErr := f.filterEvent(data)
			if filterErr != nil {
				return filterErr
			}
			if !keep {
				return nil
			}
			event.SetData(filtered)
			return event.writeTo(writer)
		})
	})
}
