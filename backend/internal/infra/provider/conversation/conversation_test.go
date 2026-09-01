package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestConvertChatRequestToResponses(t *testing.T) {
	body := []byte(`{
			"model":"public-chat","stream":true,"max_completion_tokens":512,
			"user":"client-user","presence_penalty":0,"frequency_penalty":0,
			"web_search_options":{"search_context_size":"medium"},
			"messages":[
			{"role":"system","content":"be concise"},
			{"role":"user","content":[{"type":"text","text":"describe"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"result"}
		],
		"tools":[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}}
	}`)
	converted, err := ConvertRequest(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-4.5" || payload["max_output_tokens"] != float64(512) || payload["stream"] != true || payload["safety_identifier"] != "client-user" {
		t.Fatalf("request fields = %#v", payload)
	}
	if _, exists := payload["user"]; exists {
		t.Fatalf("Chat user 不应原样转发到 Responses: %#v", payload)
	}
	input := payload["input"].([]any)
	if len(input) != 4 || input[2].(map[string]any)["type"] != "function_call" || input[3].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
	content := input[1].(map[string]any)["content"].([]any)
	if content[1].(map[string]any)["image_url"] != "data:image/png;base64,AA==" {
		t.Fatalf("image content = %#v", content)
	}
	if content[1].(map[string]any)["detail"] != "auto" {
		t.Fatalf("image detail = %#v", content[1])
	}
	tools := payload["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "lookup" || tools[0].(map[string]any)["type"] != "function" || tools[1].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestConvertChatRequestDropsReasoningContentHistory(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[
			{"role":"assistant","content":"hi","reasoning_content":"prior thought"},
			{"role":"user","content":"continue"}
		]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input = %#v", payload["input"])
	}
	for i, raw := range input {
		item, _ := raw.(map[string]any)
		if item["type"] == "reasoning" {
			t.Fatalf("chat reasoning_content leaked into input[%d]: %#v", i, item)
		}
		if item["type"] != "message" {
			t.Fatalf("input[%d] = %#v, want message", i, item)
		}
	}
	if _, exists := payload["reasoning"]; exists {
		t.Fatalf("request-level reasoning = %#v", payload["reasoning"])
	}
}

func TestConvertChatRequestKeepsReasoningEffort(t *testing.T) {
	converted, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public-chat",
		"reasoning_effort":"high",
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" {
		t.Fatalf("reasoning_effort dropped: %#v", payload["reasoning"])
	}
	if options.ReasoningEffortSet || options.ReasoningEffort != "" {
		t.Fatalf("chat options leaked Messages reasoning metadata: %#v", options)
	}
}

func TestConvertChatDropsNestedReasoningObject(t *testing.T) {
	converted, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public-chat",
		"reasoning":{"effort":"high"},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["reasoning"]; exists {
		t.Fatalf("Chat nested reasoning 不应转发到 Responses: %#v", payload["reasoning"])
	}
	if options.ReasoningEffortSet || options.ReasoningEffort != "" {
		t.Fatalf("chat options leaked Messages reasoning metadata: %#v", options)
	}
}

func TestConvertChatWebSearchOptionsInjectsDespiteToolChoiceNone(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"web_search_options":{"search_context_size":"medium"},
		"tool_choice":"none",
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["type"] != "web_search" {
		t.Fatalf("tool_choice none blocked web_search_options inject: %#v", payload["tools"])
	}
	if payload["tool_choice"] != "none" {
		t.Fatalf("tool_choice = %#v, want none", payload["tool_choice"])
	}
}

func TestConvertChatJSONSchemaFlattensIntoTextFormat(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object"}}},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["response_format"]; exists {
		t.Fatalf("Chat response_format 不应原样转发: %#v", payload)
	}
	format, _ := payload["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "answer" || format["strict"] != true || format["json_schema"] != nil {
		t.Fatalf("json_schema 未展平进 text.format: %#v", payload["text"])
	}
	schema, _ := format["schema"].(map[string]any)
	if schema["type"] != "object" {
		t.Fatalf("schema = %#v", format["schema"])
	}

	objectOnly, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"response_format":{"type":"json_object"},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(objectOnly, &payload); err != nil {
		t.Fatal(err)
	}
	format, _ = payload["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_object" || payload["response_format"] != nil {
		t.Fatalf("json_object 未写入 text.format: %#v", payload)
	}
}

func TestConvertChatPreservesWebSearchExcludedDomains(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search","filters":{"excluded_domains":["blocked.example"]}}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	tool := payload["tools"].([]any)[0].(map[string]any)
	domains := tool["filters"].(map[string]any)["excluded_domains"].([]any)
	if len(domains) != 1 || domains[0] != "blocked.example" {
		t.Fatalf("excluded_domains = %#v", tool)
	}
}

func TestConvertChatValidatesWebSearchDomainFilters(t *testing.T) {
	for _, test := range []struct {
		name    string
		tool    string
		wantErr bool
	}{
		{name: "identical nested and top level", tool: `{"type":"web_search","filters":{"allowed_domains":["same.example"]},"allowed_domains":["same.example"]}`},
		{name: "conflicting duplicate", tool: `{"type":"web_search","filters":{"allowed_domains":["nested.example"]},"allowed_domains":["top.example"]}`, wantErr: true},
		{name: "allow and exclude", tool: `{"type":"web_search","filters":{"allowed_domains":["allow.example"],"excluded_domains":["deny.example"]}}`, wantErr: true},
		{name: "six domains", tool: `{"type":"web_search","excluded_domains":["a.example","b.example","c.example","d.example","e.example","f.example"]}`, wantErr: true},
		{name: "invalid filters type", tool: `{"type":"web_search","filters":"invalid"}`, wantErr: true},
		{name: "empty filters omitted", tool: `{"type":"web_search","filters":{"allowed_domains":null,"excluded_domains":[]}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			converted, err := ConvertRequest([]byte(`{
				"model":"public-chat","messages":[{"role":"user","content":"search"}],
				"tools":[`+test.tool+`]
			}`), "grok-4.6", OperationChat)
			if (err != nil) != test.wantErr {
				t.Fatalf("conversion error = %v, wantErr %v", err, test.wantErr)
			}
			if err != nil {
				return
			}
			var payload map[string]any
			if err := json.Unmarshal(converted, &payload); err != nil {
				t.Fatal(err)
			}
			tool := payload["tools"].([]any)[0].(map[string]any)
			if test.name == "empty filters omitted" && len(tool) != 1 {
				t.Fatalf("empty filters were not omitted: %#v", tool)
			}
		})
	}
}

func TestConvertChatToolImageResultToMultimodalFunctionOutput(t *testing.T) {
	body := []byte(`{
		"model":"public-chat",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":[
				{"type":"text","text":"Read image file"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,AA==","detail":"high"}}
			]}
		]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input := payload["input"].([]any)
	output := input[1].(map[string]any)["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("tool output = %#v", output)
	}
	textBlock := output[0].(map[string]any)
	imageBlock := output[1].(map[string]any)
	if textBlock["type"] != "input_text" || textBlock["text"] != "Read image file" ||
		imageBlock["type"] != "input_image" || imageBlock["detail"] != "high" ||
		imageBlock["image_url"] != "data:image/png;base64,AA==" {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestConvertChatKeepsNonMultimodalToolJSONAsText(t *testing.T) {
	body := []byte(`{
		"model":"public-chat",
		"messages":[{"role":"tool","tool_call_id":"call_1","content":[{"name":"value","value":1}]}]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	output := payload["input"].([]any)[0].(map[string]any)["output"]
	if output != `[{"name":"value","value":1}]` {
		t.Fatalf("tool output = %#v", output)
	}
}

func TestConvertChatKeepsParallelToolCalls(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","parallel_tool_calls":false,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls dropped: %#v", payload)
	}
}

func TestConvertChatKeepsServiceTier(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","service_tier":"priority",
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["service_tier"] != "priority" {
		t.Fatalf("service_tier dropped: %#v", payload)
	}
}

func TestConvertChatKeepsStore(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","store":true,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["store"] != true {
		t.Fatalf("store dropped: %#v", payload)
	}
}

func TestConvertChatMaxTokensFallback(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","max_tokens":256,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_output_tokens"] != float64(256) {
		t.Fatalf("max_tokens fallback dropped: %#v", payload)
	}
	if _, exists := payload["max_tokens"]; exists {
		t.Fatalf("max_tokens 不应原样转发: %#v", payload)
	}
}

func TestConvertChatMaxCompletionTokensWinsOverMaxTokens(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","max_completion_tokens":128,"max_tokens":256,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["max_output_tokens"] != float64(128) {
		t.Fatalf("max_completion_tokens 未优先: %#v", payload)
	}
	if _, exists := payload["max_tokens"]; exists {
		t.Fatalf("max_tokens 不应原样转发: %#v", payload)
	}
	if _, exists := payload["max_completion_tokens"]; exists {
		t.Fatalf("max_completion_tokens 不应原样转发: %#v", payload)
	}
}

func TestConvertChatKeepsTemperature(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","temperature":0.2,"top_p":0.8,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["temperature"] != 0.2 || payload["top_p"] != 0.8 {
		t.Fatalf("Chat temperature/top_p dropped: %#v", payload)
	}
}

func TestConvertChatToolChoiceFunctionFlattens(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object"}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}}
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	choice, _ := payload["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice 未展平: %#v", payload["tool_choice"])
	}
	if _, exists := choice["function"]; exists {
		t.Fatalf("nested function 不应保留: %#v", choice)
	}
}

func TestConvertChatToolChoiceAutoPassthrough(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","tool_choice":"auto",
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["tool_choice"] != "auto" {
		t.Fatalf("tool_choice auto dropped: %#v", payload)
	}
}

func TestConvertChatToolChoiceRequiredPassthrough(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","tool_choice":"required",
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["tool_choice"] != "required" {
		t.Fatalf("tool_choice required dropped: %#v", payload)
	}
}

func TestConvertChatFunctionToolsLiftName(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"function","function":{"name":"lookup","description":"lookup","parameters":{"type":"object"}}}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "function" || tool["name"] != "lookup" || tool["description"] != "lookup" {
		t.Fatalf("function tool 未展平: %#v", tool)
	}
	if _, exists := tool["function"]; exists {
		t.Fatalf("nested function 不应保留: %#v", tool)
	}
}

func TestConvertChatWebSearchPreviewNormalizesType(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"web_search_preview"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	if tool["type"] != "web_search" {
		t.Fatalf("web_search_preview 未归一: %#v", tool)
	}
}

func TestConvertChatUnknownToolTypePassthrough(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"type":"file_search","vector_store_ids":["vs_1"]}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	ids, _ := tool["vector_store_ids"].([]any)
	if tool["type"] != "file_search" || len(ids) != 1 || ids[0] != "vs_1" {
		t.Fatalf("unknown tool type 未透传: %#v", tool)
	}
}

func TestConvertChatDropsMessageName(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[
			{"role":"user","name":"alice","content":"hello"},
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","name":"lookup","tool_call_id":"call_1","content":"ok"}
		]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", payload["input"])
	}
	user, _ := input[0].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" || user["content"] != "hello" {
		t.Fatalf("user = %#v", user)
	}
	if _, exists := user["name"]; exists {
		t.Fatalf("user message.name 不应写入 Responses input: %#v", user)
	}
	call, _ := input[1].(map[string]any)
	if call["type"] != "function_call" || call["name"] != "lookup" {
		t.Fatalf("function_call = %#v", call)
	}
	output, _ := input[2].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "call_1" || output["output"] != "ok" {
		t.Fatalf("tool output = %#v", output)
	}
	if _, exists := output["name"]; exists {
		t.Fatalf("tool message.name 不应写入 function_call_output: %#v", output)
	}
}

func TestConvertChatSkipsEmptySystemContent(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"public-chat","messages":[{"role":"system","content":null},{"role":"user","content":"hello"}]}`),
		[]byte(`{"model":"public-chat","messages":[{"role":"system"},{"role":"user","content":"hello"}]}`),
	} {
		converted, err := ConvertRequest(body, "grok-4.6", OperationChat)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(converted, &payload); err != nil {
			t.Fatal(err)
		}
		input, _ := payload["input"].([]any)
		if len(input) != 1 {
			t.Fatalf("input = %#v", payload["input"])
		}
		msg, _ := input[0].(map[string]any)
		if msg["type"] != "message" || msg["role"] != "user" || msg["content"] != "hello" {
			t.Fatalf("expected only user message, got %#v", msg)
		}
	}
	_, err := ConvertRequest([]byte(`{"model":"public-chat","messages":[{"role":"system","content":null}]}`), "grok-4.6", OperationChat)
	if err == nil {
		t.Fatal("expected empty system-only messages to be rejected")
	}
	if !strings.Contains(err.Error(), "messages 中没有可发送内容") {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertChatKeepsEmptyStringContent(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"system","content":""},{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("empty-string content 不应被当 empty 跳过: %#v", payload["input"])
	}
	system, _ := input[0].(map[string]any)
	if system["type"] != "message" || system["role"] != "system" || system["content"] != "" {
		t.Fatalf("system = %#v", system)
	}
	user, _ := input[1].(map[string]any)
	if user["type"] != "message" || user["role"] != "user" || user["content"] != "hello" {
		t.Fatalf("user = %#v", user)
	}
}

func TestConvertChatEmptyToolCallArgumentsBecomeObject(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"public-chat","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup"}}]}]}`),
		[]byte(`{"model":"public-chat","messages":[{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}]}`),
	} {
		converted, err := ConvertRequest(body, "grok-4.6", OperationChat)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(converted, &payload); err != nil {
			t.Fatal(err)
		}
		input, _ := payload["input"].([]any)
		if len(input) != 1 {
			t.Fatalf("input = %#v", payload["input"])
		}
		call, _ := input[0].(map[string]any)
		if call["type"] != "function_call" || call["call_id"] != "call_1" || call["name"] != "lookup" || call["arguments"] != "{}" {
			t.Fatalf("empty arguments 未写成 {}: %#v", call)
		}
	}
}

func TestConvertChatRejectsUnknownMessageRole(t *testing.T) {
	_, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"function","name":"lookup","content":"legacy"}]
	}`), "grok-4.6", OperationChat)
	if err == nil {
		t.Fatal("expected unknown role to be rejected")
	}
	if !strings.Contains(err.Error(), `不支持 messages.role="function"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertChatRejectsToolMessageWithoutCallID(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"public-chat","messages":[{"role":"user","content":"hello"},{"role":"tool","content":"ok"}]}`),
		[]byte(`{"model":"public-chat","messages":[{"role":"user","content":"hello"},{"role":"tool","tool_call_id":"","content":"ok"}]}`),
	} {
		_, err := ConvertRequest(body, "grok-4.6", OperationChat)
		if err == nil {
			t.Fatal("expected missing tool_call_id to be rejected")
		}
		if !strings.Contains(err.Error(), "tool 消息缺少 tool_call_id") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestConvertChatRejectsInputFileContent(t *testing.T) {
	_, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":[{"type":"input_file","file_id":"file_1"}]}]
	}`), "grok-4.6", OperationChat)
	if err == nil {
		t.Fatal("expected input_file to be rejected")
	}
	if !strings.Contains(err.Error(), `不支持 content.type="input_file"`) {
		t.Fatalf("err = %v", err)
	}
}

func TestConvertChatNormalizesInputAndOutputTextParts(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":[
			{"type":"input_text","text":"ask"},
			{"type":"output_text","text":"prior"}
		]}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v", payload["input"])
	}
	content, _ := input[0].(map[string]any)["content"].([]any)
	if len(content) != 2 {
		t.Fatalf("content = %#v", content)
	}
	first, _ := content[0].(map[string]any)
	second, _ := content[1].(map[string]any)
	if first["type"] != "input_text" || first["text"] != "ask" || second["type"] != "input_text" || second["text"] != "prior" {
		t.Fatalf("input_text/output_text 未归一成 input_text: %#v", content)
	}
}

func TestConvertChatKeepsDeveloperRole(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"developer","content":"dev rules"},{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["instructions"]; exists {
		t.Fatalf("Chat developer 不应折进 instructions: %#v", payload)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input = %#v", payload["input"])
	}
	first, _ := input[0].(map[string]any)
	if first["type"] != "message" || first["role"] != "developer" {
		t.Fatalf("developer 未保留: %#v", first)
	}
	if first["content"] != "dev rules" {
		t.Fatalf("developer content = %#v", first)
	}
}

func TestConvertChatInputImageURLFallback(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat",
		"messages":[{"role":"user","content":[{"type":"input_image","url":"data:image/png;base64,AA=="}]}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	input, _ := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %#v", payload["input"])
	}
	content, _ := input[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content = %#v", content)
	}
	image, _ := content[0].(map[string]any)
	if image["type"] != "input_image" || image["image_url"] != "data:image/png;base64,AA==" || image["detail"] != "auto" {
		t.Fatalf("input_image url fallback dropped: %#v", image)
	}
}

func TestConvertChatKeepsMetadata(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","metadata":{"session":"lab"},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	meta, _ := payload["metadata"].(map[string]any)
	if meta["session"] != "lab" {
		t.Fatalf("metadata dropped: %#v", payload)
	}
}

func TestConvertChatUserAndMetadataAreOrthogonal(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","user":"lab-user","metadata":{"session":"lab"},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["safety_identifier"] != "lab-user" {
		t.Fatalf("user 未写入 safety_identifier: %#v", payload)
	}
	if _, exists := payload["user"]; exists {
		t.Fatalf("Chat user 不应原样转发: %#v", payload)
	}
	meta, _ := payload["metadata"].(map[string]any)
	if meta["session"] != "lab" {
		t.Fatalf("metadata 被 user 覆盖: %#v", payload)
	}
}

func TestConvertChatRejectsEmptyOrWhitespaceUser(t *testing.T) {
	for _, body := range [][]byte{
		[]byte(`{"model":"public-chat","user":"","messages":[{"role":"user","content":"hello"}]}`),
		[]byte(`{"model":"public-chat","user":"   ","messages":[{"role":"user","content":"hello"}]}`),
	} {
		_, err := ConvertRequest(body, "grok-4.6", OperationChat)
		if err == nil {
			t.Fatal("expected empty user to be rejected")
		}
		if !strings.Contains(err.Error(), "user 必须是非空字符串") {
			t.Fatalf("err = %v", err)
		}
	}
}

func TestConvertChatDropsN(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","n":2,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["n"]; exists {
		t.Fatalf("Chat n 不应转发到 Responses: %#v", payload)
	}
}

func TestConvertChatDropsPreviousResponseID(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public-chat","previous_response_id":"resp_1","store":true,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["previous_response_id"]; exists {
		t.Fatalf("Chat previous_response_id 不应转发到 Responses: %#v", payload)
	}
	if payload["store"] != true {
		t.Fatalf("store 应仍保留: %#v", payload["store"])
	}
}

func TestConvertChatIgnoresSamplingFieldsWithoutResponsesEquivalent(t *testing.T) {
	body := []byte(`{
		"model":"public","messages":[{"role":"user","content":"hi"}],
		"presence_penalty":0.5,"frequency_penalty":-0.5,"seed":42
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"presence_penalty", "frequency_penalty", "seed"} {
		if _, exists := payload[field]; exists {
			t.Fatalf("不受 Responses 支持的 %s 不应转发给上游: %#v", field, payload)
		}
	}
}

func TestConvertChatStopSequencesLocally(t *testing.T) {
	request := []byte(`{
		"model":"public-chat","stop":["STOP","END"],
		"messages":[{"role":"user","content":"continue"}]
	}`)
	converted, options, err := ConvertRequestWithOptions(request, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["stop"]; exists {
		t.Fatalf("Responses 请求不应包含不受支持的 stop 字段: %#v", payload)
	}
	if len(options.StopSequences) != 2 || options.StopSequences[0] != "STOP" || options.StopSequences[1] != "END" {
		t.Fatalf("stop options = %#v", options.StopSequences)
	}

	body := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]}]}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationChat, options)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["content"] != "ABC" {
		t.Fatalf("chat response = %#v", response)
	}
}

func TestConvertChatRejectsEmptyOrOverlongStop(t *testing.T) {
	tests := []struct {
		stop string
		want string
	}{
		{stop: `""`, want: "stop 不能为空"},
		{stop: `["A","B","C","D","E"]`, want: "stop 最多包含 4 个序列"},
	}
	for _, test := range tests {
		_, options, err := ConvertRequestWithOptions([]byte(`{
			"model":"public-chat","stop":`+test.stop+`,
			"messages":[{"role":"user","content":"hello"}]
		}`), "grok-4.6", OperationChat)
		if err == nil {
			t.Fatalf("stop=%s expected error, options=%#v", test.stop, options)
		}
		if !strings.Contains(err.Error(), test.want) {
			t.Fatalf("stop=%s err=%v want %q", test.stop, err, test.want)
		}
		if len(options.StopSequences) != 0 {
			t.Fatalf("stop=%s leaked into options: %#v", test.stop, options.StopSequences)
		}
	}
}

func TestConvertChatPreservesOpaqueToolArgumentsHistory(t *testing.T) {
	body := []byte(`{
		"model":"public","messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{partial"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"failed"}
		]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	call := payload["input"].([]any)[0].(map[string]any)
	if call["arguments"] != "{partial" {
		t.Fatalf("function call = %#v", call)
	}
}

func TestConvertChatStopSequencesStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"ABCST"}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"OPXYZ"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(
		io.NopCloser(strings.NewReader(stream)), OperationChat, ResponseOptions{StopSequences: []string{"STOP"}},
	))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "STOP") || strings.Contains(text, "XYZ") || !strings.Contains(text, `"content":"ABC"`) || !strings.Contains(text, `"finish_reason":"stop"`) {
		t.Fatalf("chat stream = %s", text)
	}
}

func TestConvertAnthropicMessagesRequestToResponses(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":1024,"stream":true,
		"system":[{"type":"text","text":"You are precise."}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"url","url":"https://example.com/a.png"}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"lookup","input":{"q":"x"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]}
		],
		"tools":[{"name":"lookup","description":"lookup","input_schema":{"type":"object","properties":{"q":{"type":"string"}}}}],
		"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true}
	}`)
	converted, err := ConvertRequest(body, "grok-chat-fast", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["model"] != "grok-chat-fast" || payload["instructions"] != "You are precise." || payload["parallel_tool_calls"] != false {
		t.Fatalf("request = %#v", payload)
	}
	input := payload["input"].([]any)
	if len(input) != 3 || input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
}

func TestConvertAnthropicMessagesDropsClaudeCodeBillingAttribution(t *testing.T) {
	body := []byte(`{
		"model":"claude","max_tokens":128,
		"system":[
			{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.170.abc; cc_entrypoint=sdk-cli; cch=12345;"},
			{"type":"text","text":"stable system instruction","cache_control":{"type":"ephemeral"}}
		],
		"messages":[{"role":"user","content":"hello"}]
	}`)
	converted, err := ConvertRequest(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] != "stable system instruction" {
		t.Fatalf("instructions = %#v", payload["instructions"])
	}
	if strings.Contains(string(converted), anthropicBillingHeaderPrefix) {
		t.Fatalf("billing attribution leaked into Build request: %s", converted)
	}
}

func TestConvertAnthropicMessagesInlineSystemRole(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":1024,
		"system":"Top-level rules.",
		"messages":[
			{"role":"system","content":"Inline directive."},
			{"role":"system","content":[{"type":"text","text":"Inline block."}]},
			{"role":"user","content":"hi"}
		]
	}`)
	converted, err := ConvertRequest(body, "grok-chat-fast", OperationMessages)
	if err != nil {
		t.Fatalf("inline system role should not fail: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if instructions, _ := payload["instructions"].(string); instructions != "Top-level rules.\n\nInline directive.\n\nInline block." {
		t.Fatalf("instructions = %q", instructions)
	}
	input := payload["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input should only contain the user message, got %#v", input)
	}
	if role := input[0].(map[string]any)["role"]; role != "user" {
		t.Fatalf("remaining input role = %v", role)
	}
}

func TestConvertAnthropicMessagesInlineSystemOnly(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":256,
		"messages":[
			{"role":"system","content":"Only inline."},
			{"role":"user","content":"go"}
		]
	}`)
	converted, err := ConvertRequest(body, "grok-chat-fast", OperationMessages)
	if err != nil {
		t.Fatalf("inline-only system should not fail: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] != "Only inline." {
		t.Fatalf("instructions = %#v", payload["instructions"])
	}
}

func TestConvertAnthropicMessagesRejectsUnknownRole(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":256,
		"messages":[{"role":"tool","content":"nope"}]
	}`)
	if _, err := ConvertRequest(body, "grok-chat-fast", OperationMessages); err == nil {
		t.Fatal("unknown role should be rejected")
	}
}

func TestConvertAnthropicRedactedThinkingIncludesEmptySummary(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":256,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":[{"type":"redacted_thinking","data":"opaque-reasoning"}]},
			{"role":"user","content":"continue"}
		]
	}`)
	converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(converted, &payload) != nil || len(payload.Input) != 2 {
		t.Fatalf("converted = %s", converted)
	}
	reasoning := payload.Input[0]
	summary, ok := reasoning["summary"].([]any)
	if reasoning["type"] != "reasoning" || reasoning["encrypted_content"] != "opaque-reasoning" || !ok || len(summary) != 0 {
		t.Fatalf("reasoning = %#v", reasoning)
	}
}

func TestConvertAnthropicEmptyThinkingKeepsSignature(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public-chat","max_tokens":256,
		"thinking":{"type":"enabled","budget_tokens":1024},
		"messages":[
			{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"encrypted-reasoning"}]},
			{"role":"user","content":"continue"}
		]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Input []map[string]any `json:"input"`
	}
	if json.Unmarshal(converted, &payload) != nil || len(payload.Input) != 2 {
		t.Fatalf("converted = %s", converted)
	}
	reasoning := payload.Input[0]
	summary, ok := reasoning["summary"].([]any)
	if !ok || len(summary) != 1 {
		t.Fatalf("empty thinking dropped: %#v", reasoning)
	}
	part, _ := summary[0].(map[string]any)
	if reasoning["type"] != "reasoning" || reasoning["encrypted_content"] != "encrypted-reasoning" || part["type"] != "summary_text" || part["text"] != "" {
		t.Fatalf("empty thinking = %#v", reasoning)
	}
}

func TestConvertAnthropicClaudeCodeRequestToResponses(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":4096,"stream":true,
		"system":[{"type":"text","text":"top-level system","cache_control":{"type":"ephemeral"}}],
		"messages":[
			{"role":"system","content":"legacy system"},
			{"role":"developer","content":[{"type":"text","text":"developer context","cache_control":{"type":"ephemeral"}}]},
			{"role":"assistant","content":[
				{"type":"thinking","thinking":"prior thought","signature":"encrypted-reasoning"},
				{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"README.md"}}
			]},
			{"role":"user","content":[
				{"type":"tool_result","tool_use_id":"toolu_1","is_error":true,"content":[
					{"type":"text","text":"failed"},
					{"type":"tool_reference","tool_name":"Read"},
					{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}}
				]},
				{"type":"document","title":"notes.txt","source":{"type":"text","data":"document text"}},
				{"type":"text","text":"continue"}
			]}
		],
		"metadata":{"user_id":"cc-user"},
		"thinking":{"type":"enabled","budget_tokens":12000},
		"tools":[{"name":"Read","description":"Read file","input_schema":{"type":"object"},"strict":true,"cache_control":{"type":"ephemeral"}}],
		"mcp_servers":[{"name":"github","url":"https://example.com/mcp","authorization_token":"token"}]
	}`)
	converted, options, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	if !options.AnthropicThinking || !options.ReasoningEffortSet || options.ReasoningEffort != "high" {
		t.Fatal("thinking option 未保留")
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["instructions"] != "top-level system\n\nlegacy system\n\ndeveloper context" || payload["safety_identifier"] != "cc-user" || payload["store"] != false {
		t.Fatalf("request metadata = %#v", payload)
	}
	reasoning := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || payload["include"].([]any)[0] != "reasoning.encrypted_content" {
		t.Fatalf("reasoning = %#v, include = %#v", reasoning, payload["include"])
	}
	input := payload["input"].([]any)
	if len(input) != 4 || input[0].(map[string]any)["type"] != "reasoning" || input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("input = %#v", input)
	}
	reasoningItem := input[0].(map[string]any)
	summary, _ := reasoningItem["summary"].([]any)
	if len(summary) != 1 || summary[0].(map[string]any)["type"] != "summary_text" || summary[0].(map[string]any)["text"] != "prior thought" || reasoningItem["encrypted_content"] != "encrypted-reasoning" {
		t.Fatalf("thinking history dropped: %#v", reasoningItem)
	}
	output := input[2].(map[string]any)["output"].([]any)
	if len(output) != 4 || !strings.Contains(output[0].(map[string]any)["text"].(string), "failed") ||
		!strings.Contains(output[2].(map[string]any)["text"].(string), `"Read"`) || output[3].(map[string]any)["type"] != "input_image" || output[3].(map[string]any)["detail"] != "auto" {
		t.Fatalf("tool result = %#v", output)
	}
	tools := payload["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["type"] != "function" || tools[0].(map[string]any)["strict"] != true || tools[1].(map[string]any)["type"] != "mcp" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestConvertAnthropicDisabledThinkingReportsNoneMetadata(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"disabled"}
	}`)
	_, options, err := ConvertRequestWithOptions(body, "grok-4.3", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	if options.AnthropicThinking || !options.ReasoningEffortSet || options.ReasoningEffort != "none" {
		t.Fatalf("options = %#v", options)
	}
}

func TestConvertAnthropicMessagesPreservesExtendedReasoningEffort(t *testing.T) {
	for _, effort := range []string{"xhigh", "max"} {
		t.Run(effort, func(t *testing.T) {
			body := []byte(`{
				"model":"public-chat","max_tokens":1024,
				"messages":[{"role":"user","content":"hello"}],
				"thinking":{"type":"enabled","budget_tokens":1024},
				"output_config":{"effort":"` + effort + `"}
			}`)
			converted, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]any
			if err := json.Unmarshal(converted, &payload); err != nil {
				t.Fatal(err)
			}
			reasoning, ok := payload["reasoning"].(map[string]any)
			if !ok || reasoning["effort"] != effort {
				t.Fatalf("reasoning = %#v", payload["reasoning"])
			}
		})
	}
}

func TestConvertAnthropicMessagesUsesThinkingEffortBeforeBudget(t *testing.T) {
	body := []byte(`{
		"model":"public-chat","max_tokens":1024,
		"messages":[{"role":"user","content":"hello"}],
		"thinking":{"type":"enabled","effort":"high","budget_tokens":1024}
	}`)
	converted, options, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if json.Unmarshal(converted, &payload) != nil {
		t.Fatalf("converted = %s", converted)
	}
	reasoning, _ := payload["reasoning"].(map[string]any)
	if reasoning["effort"] != "high" || !options.ReasoningEffortSet || options.ReasoningEffort != "high" {
		t.Fatalf("reasoning = %#v, options = %#v", reasoning, options)
	}
}

func TestConvertAnthropicToolReferenceValidatesDeclaredTool(t *testing.T) {
	body := []byte(`{
		"model":"public","max_tokens":64,
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"SearchTools","input":{}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"tool_reference","tool_name":"Missing"}]}]}
		],
		"tools":[{"name":"SearchTools","input_schema":{"type":"object"}}]
	}`)
	_, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
	if err == nil || !strings.Contains(err.Error(), `未声明的工具 "Missing"`) {
		t.Fatalf("error = %v", err)
	}
}

func TestConvertAnthropicMessagesValidatesToolRelationships(t *testing.T) {
	tests := []struct {
		name     string
		messages string
		want     string
	}{
		{name: "orphan result", messages: `[{"role":"user","content":[{"type":"tool_result","tool_use_id":"missing","content":"x"}]}]`, want: "未匹配"},
		{name: "missing result", messages: `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}]`, want: "提供 tool_result"},
		{name: "result after text", messages: `[{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]},{"role":"user","content":[{"type":"text","text":"late"},{"type":"tool_result","tool_use_id":"toolu_1","content":"x"}]}]`, want: "必须位于"},
		{name: "user tool use", messages: `[{"role":"user","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]}]`, want: "只允许"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := []byte(`{"model":"public","max_tokens":64,"messages":` + test.messages + `}`)
			_, _, err := ConvertRequestWithOptions(body, "grok-4.5", OperationMessages)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestConvertAnthropicMCPServersMapsAuthorization(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public","max_tokens":64,
		"messages":[{"role":"user","content":"hello"}],
		"mcp_servers":[{"name":"docs","url":"https://example.com/mcp","authorization_token":"opaque"}]
	}`), "grok-4.6", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["mcp_servers"]; exists {
		t.Fatalf("mcp_servers 不应原样转发: %#v", payload)
	}
	tools, _ := payload["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", payload["tools"])
	}
	tool := tools[0].(map[string]any)
	if tool["type"] != "mcp" || tool["server_label"] != "docs" || tool["server_url"] != "https://example.com/mcp" || tool["authorization"] != "opaque" {
		t.Fatalf("mcp_servers authorization dropped: %#v", tool)
	}
}

func TestConvertAnthropicMessagesJSONSchemaOutputConfig(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public","max_tokens":64,
		"output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["output_config"]; exists {
		t.Fatalf("Messages output_config 不应原样转发: %#v", payload)
	}
	format, _ := payload["text"].(map[string]any)["format"].(map[string]any)
	schema, _ := format["schema"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "anthropic_output" || schema["type"] != "object" {
		t.Fatalf("output_config.format 未写入 text.format: %#v", payload["text"])
	}
}

func TestConvertAnthropicMessagesKeepsTemperature(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public","max_tokens":64,"temperature":0.2,"top_p":0.8,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["temperature"] != 0.2 || payload["top_p"] != 0.8 {
		t.Fatalf("Messages temperature/top_p dropped: %#v", payload)
	}
}

func TestConvertAnthropicMessagesKeepsStopSequencesLocally(t *testing.T) {
	converted, options, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"stop_sequences":["STOP","END"],
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["stop_sequences"]; exists {
		t.Fatalf("stop_sequences 不应转发到 Responses: %#v", payload)
	}
	if _, exists := payload["stop"]; exists {
		t.Fatalf("stop 不应出现在 Responses: %#v", payload)
	}
	if len(options.StopSequences) != 2 || options.StopSequences[0] != "STOP" || options.StopSequences[1] != "END" {
		t.Fatalf("stop options = %#v", options.StopSequences)
	}
}

func TestConvertAnthropicMessagesDropsNonUserMetadata(t *testing.T) {
	converted, err := ConvertRequest([]byte(`{
		"model":"public","max_tokens":64,
		"metadata":{"user_id":"lab-user","session":"lab"},
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.6", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["safety_identifier"] != "lab-user" {
		t.Fatalf("user_id 未写入 safety_identifier: %#v", payload)
	}
	if _, exists := payload["metadata"]; exists {
		t.Fatalf("Messages metadata 不应原样转发: %#v", payload)
	}
	if _, exists := payload["session"]; exists {
		t.Fatalf("metadata.session 不应提升到 Responses: %#v", payload)
	}
}

func TestConvertAnthropicMessagesIgnoresUnrepresentableTopK(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"top_k":10,
		"messages":[{"role":"user","content":"hello"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(converted, &payload); err != nil {
		t.Fatal(err)
	}
	if _, exists := payload["top_k"]; exists {
		t.Fatalf("top_k 不应转发到 Responses: %#v", payload)
	}
}

func TestConvertAnthropicWebSearchControls(t *testing.T) {
	converted, _, err := ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":3,"allowed_domains":["example.com"],"user_location":{"type":"approximate","country":"US"}}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	_ = json.Unmarshal(converted, &payload)
	tool := payload["tools"].([]any)[0].(map[string]any)
	domains := tool["filters"].(map[string]any)["allowed_domains"].([]any)
	if tool["type"] != "web_search" || len(domains) != 1 || domains[0] != "example.com" || len(tool) != 2 {
		t.Fatalf("tool = %#v", tool)
	}

	converted, _, err = ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","search_context_size":"high"}]
	}`), "grok-4.5", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	_ = json.Unmarshal(converted, &payload)
	tool = payload["tools"].([]any)[0].(map[string]any)
	if len(tool) != 1 || tool["type"] != "web_search" {
		t.Fatalf("downgraded tool = %#v", tool)
	}

	converted, _, err = ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","blocked_domains":["blocked.example"]}]
	}`), "grok-4.6", OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	payload = nil
	_ = json.Unmarshal(converted, &payload)
	tool = payload["tools"].([]any)[0].(map[string]any)
	domains = tool["filters"].(map[string]any)["excluded_domains"].([]any)
	if len(domains) != 1 || domains[0] != "blocked.example" {
		t.Fatalf("blocked_domains conversion = %#v", tool)
	}

	if _, _, err = ConvertRequestWithOptions([]byte(`{
		"model":"public","max_tokens":64,"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20250305","name":"web_search","allowed_domains":["allow.example"],"blocked_domains":["blocked.example"]}]
	}`), "grok-4.6", OperationMessages); err == nil {
		t.Fatal("allowed_domains + blocked_domains must be rejected")
	}
}

func TestConvertResponsesJSONToChatAndMessages(t *testing.T) {
	body := []byte(`{
		"id":"resp_1","object":"response","created_at":123,"model":"grok-4.5","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"reason"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello","annotations":[{"type":"url_citation","url":"https://example.com","title":"Example"}]}]},
			{"id":"fc_1","type":"function_call","call_id":"call_1","name":"lookup","arguments":"{\"q\":\"x\"}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":2},"output_tokens_details":{"reasoning_tokens":1},"cost_in_usd_ticks":158500,"num_sources_used":3,"num_server_side_tools_used":2,"context_details":{"input_tokens":9,"output_tokens":4}}
	}`)
	chatData, err := ConvertResponseJSON(body, OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	_ = json.Unmarshal(chatData, &chat)
	choice := chat["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	if chat["object"] != "chat.completion" || choice["finish_reason"] != "tool_calls" || message["reasoning_content"] != "reason" {
		t.Fatalf("chat = %#v", chat)
	}
	if annotations := message["annotations"].([]any); len(annotations) != 1 || annotations[0].(map[string]any)["url"] != "https://example.com" {
		t.Fatalf("chat annotations = %#v", message)
	}
	chatUsage := chat["usage"].(map[string]any)
	if chatUsage["cost_in_usd_ticks"] != float64(158500) || chatUsage["num_sources_used"] != float64(3) || chatUsage["context_details"].(map[string]any)["input_tokens"] != float64(9) {
		t.Fatalf("chat usage = %#v", chatUsage)
	}

	messagesData, err := ConvertResponseJSON(body, OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var messages map[string]any
	_ = json.Unmarshal(messagesData, &messages)
	content := messages["content"].([]any)
	if messages["type"] != "message" || messages["stop_reason"] != "tool_use" || content[1].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("messages = %#v", messages)
	}
	messagesUsage := messages["usage"].(map[string]any)
	if messagesUsage["cost_in_usd_ticks"] != float64(158500) || messagesUsage["num_server_side_tools_used"] != float64(2) || messagesUsage["context_details"].(map[string]any)["output_tokens"] != float64(4) {
		t.Fatalf("messages usage = %#v", messagesUsage)
	}
	if messagesUsage["input_tokens"] != float64(8) || messagesUsage["cache_read_input_tokens"] != float64(2) {
		t.Fatalf("messages cache usage = %#v", messagesUsage)
	}
	outputDetails, ok := messagesUsage["output_tokens_details"].(map[string]any)
	if !ok || outputDetails["thinking_tokens"] != float64(1) {
		t.Fatalf("messages thinking usage = %#v", messagesUsage)
	}
}

func TestAnthropicUsageClampsCacheReadToTotalInput(t *testing.T) {
	usage := responseUsage{InputTokens: 10, OutputTokens: 2}
	usage.InputTokensDetails.CachedTokens = 12

	converted := anthropicUsage(usage, 0)
	if converted["input_tokens"] != int64(0) || converted["cache_read_input_tokens"] != int64(10) {
		t.Fatalf("clamped messages usage = %#v", converted)
	}
	outputDetails, ok := converted["output_tokens_details"].(map[string]any)
	if !ok || outputDetails["thinking_tokens"] != int64(0) {
		t.Fatalf("non-reasoning messages usage = %#v", converted)
	}
}

func TestAnthropicUsageClampsThinkingTokensToOutputTokens(t *testing.T) {
	usage := responseUsage{OutputTokens: 5}
	usage.OutputTokensDetails.ReasoningTokens = 8
	converted := anthropicUsage(usage, 0)
	outputDetails := converted["output_tokens_details"].(map[string]any)
	if outputDetails["thinking_tokens"] != int64(5) {
		t.Fatalf("clamped thinking usage = %#v", converted)
	}
}

func TestConvertResponsesJSONToMessagesThinkingAndStop(t *testing.T) {
	body := []byte(`{
		"id":"response-1","model":"grok-4.5","status":"completed",
		"output":[
			{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}],"encrypted_content":"signature"},
			{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]},
			{"type":"function_call","call_id":"call_1","name":"Read","arguments":"{}"}
		],
		"usage":{"input_tokens":10,"output_tokens":5}
	}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{AnthropicThinking: true, StopSequences: []string{"STOP"}})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	content := response["content"].([]any)
	if response["id"] != "msg_response-1" || response["stop_reason"] != "tool_use" || len(content) != 3 {
		t.Fatalf("response = %#v", response)
	}
	thinking := content[0].(map[string]any)
	tool := content[2].(map[string]any)
	if thinking["type"] != "thinking" || thinking["signature"] != "signature" || content[1].(map[string]any)["text"] != "ABC" || tool["id"] != "toolu_call_1" {
		t.Fatalf("content = %#v", content)
	}
}

func TestConvertResponsesJSONUsesRawReasoningContentBeforeSummary(t *testing.T) {
	body := []byte(`{
		"id":"resp_reasoning","model":"grok-4.5","status":"completed",
		"output":[{"type":"reasoning","content":[{"type":"reasoning_text","text":"raw thought"}],"summary":[{"type":"summary_text","text":"summary"}]}]
	}`)
	data, err := ConvertResponseJSON(body, OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(data, &response); err != nil {
		t.Fatal(err)
	}
	message := response["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if message["reasoning_content"] != "raw thought" {
		t.Fatalf("message = %#v", message)
	}
}

func TestConvertResponsesRefusalAcrossChatAndMessages(t *testing.T) {
	body := []byte(`{"id":"resp_refusal","status":"completed","output":[{"type":"message","content":[{"type":"refusal","refusal":"Cannot comply"}]}]}`)
	chatData, err := ConvertResponseJSON(body, OperationChat)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	_ = json.Unmarshal(chatData, &chat)
	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "content_filter" || choice["message"].(map[string]any)["refusal"] != "Cannot comply" {
		t.Fatalf("chat refusal = %#v", chat)
	}
	messagesData, err := ConvertResponseJSON(body, OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	var messages map[string]any
	_ = json.Unmarshal(messagesData, &messages)
	if messages["stop_reason"] != "refusal" {
		t.Fatalf("messages refusal = %#v", messages)
	}
}

func TestConvertResponsesStreamRefusalAcrossChatAndMessages(t *testing.T) {
	source := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_refusal","model":"grok-4.5"}}`, "",
		`event: response.refusal.delta`,
		`data: {"type":"response.refusal.delta","delta":"Cannot comply"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	chat, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(source)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(chat); !strings.Contains(text, `"refusal":"Cannot comply"`) || !strings.Contains(text, `"finish_reason":"content_filter"`) {
		t.Fatalf("chat refusal stream = %s", text)
	}
	messages, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(source)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(messages); !strings.Contains(text, `"text":"Cannot comply"`) || !strings.Contains(text, `"stop_reason":"refusal"`) {
		t.Fatalf("messages refusal stream = %s", text)
	}
}

func TestConvertResponsesJSONToMessagesStopSequence(t *testing.T) {
	body := []byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ABCSTOPXYZ"}]}]}`)
	data, err := ConvertResponseJSONWithOptions(body, OperationMessages, ResponseOptions{StopSequences: []string{"STOP"}})
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	_ = json.Unmarshal(data, &response)
	if response["stop_reason"] != "stop_sequence" || response["stop_sequence"] != "STOP" || response["content"].([]any)[0].(map[string]any)["text"] != "ABC" {
		t.Fatalf("response = %#v", response)
	}
}

func TestConvertResponsesJSONToMessagesNormalizesErrorType(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		want        string
		wantMessage string
	}{
		{name: "preserve anthropic type", body: `{"error":{"message":"auth","type":"authentication_error"}}`, want: "authentication_error"},
		{name: "map openai type", body: `{"error":{"message":"invalid","type":"unsupported_parameter"}}`, want: "invalid_request_error"},
		{name: "map upstream code", body: `{"error":{"message":"limited","code":"rate_limit_exceeded"}}`, want: "rate_limit_error"},
		{name: "hide private type", body: `{"error":{"message":"failed","type":"private_internal"}}`, want: "api_error"},
		{name: "preserve string message", body: `{"error":"plain upstream failure"}`, want: "api_error", wantMessage: "plain upstream failure"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := ConvertResponseJSON([]byte(test.body), OperationMessages)
			if err != nil {
				t.Fatal(err)
			}
			var response map[string]any
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			errorObject := response["error"].(map[string]any)
			if errorObject["type"] != test.want {
				t.Fatalf("error = %#v", response)
			}
			if test.wantMessage != "" && errorObject["message"] != test.wantMessage {
				t.Fatalf("error message = %#v", response)
			}
		})
	}
}

func TestConvertResponsesStream(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hi"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","usage":{"input_tokens":3,"output_tokens":1}}}`, "", "",
	}, "\n")
	for _, operation := range []string{OperationChat, OperationMessages} {
		converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), operation))
		if err != nil {
			t.Fatal(err)
		}
		value := string(converted)
		if operation == OperationChat && (!strings.Contains(value, `"object":"chat.completion.chunk"`) || !strings.Contains(value, "data: [DONE]")) {
			t.Fatalf("chat stream = %s", value)
		}
		if operation == OperationMessages && (!strings.Contains(value, "event: message_start") || !strings.Contains(value, "event: content_block_delta") || !strings.Contains(value, "event: message_stop")) {
			t.Fatalf("messages stream = %s", value)
		}
	}
}

func TestConvertResponsesStreamChatErrorIsTerminal(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"upstream failed"}}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"late delta"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	value := string(converted)
	if !strings.Contains(value, `"error":{"message":"upstream failed","type":"api_error"}`) {
		t.Fatalf("missing normalized upstream failure: %s", value)
	}
	if strings.Contains(value, `"finish_reason":"stop"`) {
		t.Fatalf("error stream must not end successfully: %s", value)
	}
	if strings.Contains(value, "late delta") {
		t.Fatalf("events after an error must be ignored: %s", value)
	}
	if strings.Count(value, "data: [DONE]") != 1 {
		t.Fatalf("error stream must send one terminator: %s", value)
	}
}
func TestConvertResponsesStreamChatPrefersRawReasoningOverSummary(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6"}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`, "",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"raw "}`, "",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"reasoning"}`, "",
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"raw reasoning"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Count(text, `"reasoning_content"`) != 1 || strings.Count(text, "raw reasoning") != 1 {
		t.Fatalf("identical summary/raw reasoning should be emitted exactly once: %s", text)
	}
}

func TestConvertResponsesStreamChatFlushesSummaryAtEOF(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"summary only"}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Count(text, `"reasoning_content":"summary only"`) != 1 || !strings.Contains(text, "data: [DONE]") {
		t.Fatalf("summary fallback was not finalized at EOF: %s", text)
	}
}

func TestConvertResponsesStreamChatFlushesManySummaryDeltasAsOneChunk(t *testing.T) {
	chunks := make([]string, 0, 27)
	for i := 0; i < 20; i++ {
		chunks = append(chunks, "aaaa")
	}
	for i := 0; i < 7; i++ {
		chunks = append(chunks, "aaaaa")
	}
	joined := strings.Join(chunks, "")
	if len(joined) != 115 {
		t.Fatalf("fixture bytes = %d want 115", len(joined))
	}
	var body strings.Builder
	body.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.6\"}}\n\n")
	body.WriteString("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
	for _, chunk := range chunks {
		body.WriteString("event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"")
		body.WriteString(chunk)
		body.WriteString("\"}\n\n")
	}
	body.WriteString("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
	body.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(body.String())), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Count(text, `"reasoning_content"`) != 1 {
		t.Fatalf("summary deltas must flush as one chatDelta: %s", text)
	}
	if !strings.Contains(text, `"reasoning_content":"`+joined+`"`) {
		t.Fatalf("concatenated summary missing: %s", text)
	}
}

func TestConvertResponsesStreamChatAdoptsLateReasoningItemID(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","delta":"summary"}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`, "",
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"raw"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, `"reasoning_content":"summary"`) || strings.Count(text, `"reasoning_content":"raw"`) != 1 {
		t.Fatalf("late item_id created a second reasoning source: %s", text)
	}
}

func TestConvertResponsesStreamMessagesPrefersRawReasoningBeforeSignature(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.6"}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`, "",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"duplicated summary"}`, "",
		`event: response.reasoning_text.delta`,
		`data: {"type":"response.reasoning_text.delta","item_id":"rs_1","delta":"raw reasoning"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"signature"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(
		io.NopCloser(strings.NewReader(stream)), OperationMessages, ResponseOptions{AnthropicThinking: true},
	))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, "duplicated summary") {
		t.Fatalf("summary leaked after raw reasoning was selected: %s", text)
	}
	reasoningAt := strings.Index(text, `"thinking":"raw reasoning"`)
	signatureAt := strings.Index(text, `"signature":"signature"`)
	stopAt := strings.Index(text, `"type":"content_block_stop"`)
	if reasoningAt < 0 || signatureAt < reasoningAt || stopAt < signatureAt {
		t.Fatalf("thinking/signature/block-stop order is invalid: %s", text)
	}
}

func TestConvertResponsesStreamMessagesNormalizesTerminalError(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.failed`,
		`data: {"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"message":"quota denied","code":"forbidden"}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, `event: error`) || !strings.Contains(text, `"type":"permission_error"`) || !strings.Contains(text, `"message":"quota denied"`) {
		t.Fatalf("messages error stream = %s", text)
	}
	if strings.Contains(text, "message_stop") {
		t.Fatalf("failed stream must not emit message_stop: %s", text)
	}
}

func TestConvertResponsesStreamToMessagesThinkingToolsAndStop(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"response-1","model":"grok-4.5","usage":{"input_tokens":3}}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"reasoning-1","type":"reasoning"}}`, "",
		`event: response.reasoning_summary_text.delta`,
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"reasoning-1","delta":"thought"}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","item":{"id":"reasoning-1","type":"reasoning","encrypted_content":"signature"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"ABCST"}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"OPXYZ"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":2}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(stream)), OperationMessages, ResponseOptions{AnthropicThinking: true, StopSequences: []string{"STOP"}}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	ordered := []string{"message_start", "thinking_delta", "signature_delta", "text_delta", "message_delta", "message_stop"}
	position := 0
	for _, expected := range ordered {
		index := strings.Index(text[position:], expected)
		if index < 0 {
			t.Fatalf("%q 缺失或乱序:\n%s", expected, text)
		}
		position += index + len(expected)
	}
	if strings.Contains(text, "XYZ") || !strings.Contains(text, `"text":"ABC"`) || !strings.Contains(text, `"stop_reason":"stop_sequence"`) || !strings.Contains(text, `"stop_sequence":"STOP"`) {
		t.Fatalf("stream = %s", text)
	}
}

func TestConvertResponsesStreamMessagesFlushesManySummaryDeltasAsOneThinkingDelta(t *testing.T) {
	chunks := make([]string, 0, 27)
	for i := 0; i < 20; i++ {
		chunks = append(chunks, "aaaa")
	}
	for i := 0; i < 7; i++ {
		chunks = append(chunks, "aaaaa")
	}
	joined := strings.Join(chunks, "")
	if len(joined) != 115 {
		t.Fatalf("fixture bytes = %d want 115", len(joined))
	}
	var body strings.Builder
	body.WriteString("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"grok-4.6\"}}\n\n")
	body.WriteString("event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
	for _, chunk := range chunks {
		body.WriteString("event: response.reasoning_summary_text.delta\ndata: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"rs_1\",\"delta\":\"")
		body.WriteString(chunk)
		body.WriteString("\"}\n\n")
	}
	body.WriteString("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\"}}\n\n")
	body.WriteString("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(
		io.NopCloser(strings.NewReader(body.String())), OperationMessages, ResponseOptions{AnthropicThinking: true},
	))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Count(text, `"type":"thinking_delta"`) != 1 {
		t.Fatalf("summary deltas must flush as one thinking_delta: %s", text)
	}
	if !strings.Contains(text, `"thinking":"`+joined+`"`) {
		t.Fatalf("concatenated summary missing: %s", text)
	}
}

func TestConvertResponsesStreamEmitsDoneOnlyToolArguments(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"Read","arguments":""}}`, "",
		`event: response.function_call_arguments.done`,
		`data: {"type":"response.function_call_arguments.done","item_id":"item_1","arguments":"{\"path\":\"README.md\"}"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(stream)), OperationMessages, ResponseOptions{}))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, `"id":"toolu_call_1"`) || !strings.Contains(text, `"partial_json":"{\"path\":\"README.md\"}"`) || !strings.Contains(text, `"stop_reason":"tool_use"`) {
		t.Fatalf("stream = %s", text)
	}
	if strings.Count(text, `"type":"content_block_stop"`) != 1 {
		t.Fatalf("tool block closed multiple times: %s", text)
	}
}

func TestConvertResponsesStreamChatUsesContiguousToolIndexes(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":0,"item":{"id":"reasoning_1","type":"reasoning"}}`, "",
		`event: response.output_item.added`,
		`data: {"type":"response.output_item.added","output_index":2,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{}"}}`, "",
		`event: response.output_item.done`,
		`data: {"type":"response.output_item.done","output_index":2,"item":{"id":"item_1","type":"function_call","call_id":"call_1","name":"Read","arguments":"{}"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if !strings.Contains(text, `"tool_calls":[{"function":{"arguments":"","name":"Read"},"id":"call_1","index":0`) || strings.Contains(text, `"index":2`) {
		t.Fatalf("chat tool stream = %s", text)
	}
}

func TestConvertResponsesStreamChatPreservesAnnotations(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5"}}`, "",
		`event: response.output_text.annotation.added`,
		`data: {"type":"response.output_text.annotation.added","annotation":{"type":"url_citation","url":"https://example.com","title":"Example"}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"status":"completed"}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationChat))
	if err != nil {
		t.Fatal(err)
	}
	if text := string(converted); !strings.Contains(text, `"annotations":[{"title":"Example","type":"url_citation","url":"https://example.com"}]`) {
		t.Fatalf("chat annotation stream = %s", text)
	}
}

func TestConvertResponsesStreamMessagesInputTokens(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_1","model":"grok-4.5","status":"in_progress"}}`, "",
		`event: response.output_text.delta`,
		`data: {"type":"response.output_text.delta","delta":"hello"}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_1","model":"grok-4.5","status":"completed","usage":{"input_tokens":194,"output_tokens":7,"cost_in_usd_ticks":9000,"context_details":{"input_tokens":180,"output_tokens":6}}}}`, "", "",
	}, "\n")
	converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), OperationMessages))
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)

	if !strings.Contains(text, `"input_tokens":194`) {
		t.Fatalf("message_delta should contain input_tokens from response.completed usage:\n%s", text)
	}
	if !strings.Contains(text, `"cost_in_usd_ticks":9000`) || !strings.Contains(text, `"input_tokens":180`) {
		t.Fatalf("message_delta should retain upstream usage extensions:\n%s", text)
	}
}

func TestConvertResponsesStreamMergesPartialUsageFrames(t *testing.T) {
	stream := strings.Join([]string{
		`event: response.created`,
		`data: {"type":"response.created","response":{"id":"resp_usage","model":"grok-4.5","status":"in_progress","usage":{"input_tokens":120,"output_tokens":30,"total_tokens":150,"cost_in_usd_ticks":9000,"output_tokens_details":{"reasoning_tokens":12},"context_details":{"input_tokens":110,"output_tokens":25}}}}`, "",
		`event: response.in_progress`,
		`data: {"type":"response.in_progress","response":{"usage":{"input_tokens_details":{"cached_tokens":80}}}}`, "",
		`event: response.completed`,
		`data: {"type":"response.completed","response":{"id":"resp_usage","model":"grok-4.5","status":"completed"}}`, "", "",
	}, "\n")

	tests := []struct {
		operation string
		want      []string
	}{
		{operation: OperationChat, want: []string{`"prompt_tokens":120`, `"completion_tokens":30`, `"cached_tokens":80`, `"reasoning_tokens":12`, `"cost_in_usd_ticks":9000`}},
		{operation: OperationMessages, want: []string{`"input_tokens":40`, `"output_tokens":30`, `"cache_read_input_tokens":80`, `"thinking_tokens":12`, `"cost_in_usd_ticks":9000`}},
	}
	for _, test := range tests {
		converted, err := io.ReadAll(ConvertResponseStream(io.NopCloser(strings.NewReader(stream)), test.operation))
		if err != nil {
			t.Fatalf("%s conversion: %v", test.operation, err)
		}
		text := string(converted)
		for _, want := range test.want {
			if !strings.Contains(text, want) {
				t.Fatalf("%s partial usage lost %s:\n%s", test.operation, want, text)
			}
		}
		if test.operation == OperationMessages {
			assertAnthropicThinkingUsageEventPlacement(t, converted, 12)
		}
	}
}

func assertAnthropicThinkingUsageEventPlacement(t *testing.T, stream []byte, want int64) {
	t.Helper()
	seenStart := false
	seenDelta := false
	err := consumeSSE(bytes.NewReader(stream), func(event string, data []byte) error {
		if event != "message_start" && event != "message_delta" {
			return nil
		}
		var payload struct {
			Message struct {
				Usage map[string]json.RawMessage `json:"usage"`
			} `json:"message"`
			Usage map[string]json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatalf("decode %s event: %v", event, err)
		}
		if event == "message_start" {
			seenStart = true
			if _, exists := payload.Message.Usage["output_tokens_details"]; exists {
				t.Fatalf("message_start unexpectedly contains output token details: %s", data)
			}
			return nil
		}
		seenDelta = true
		var details struct {
			ThinkingTokens int64 `json:"thinking_tokens"`
		}
		if err := json.Unmarshal(payload.Usage["output_tokens_details"], &details); err != nil {
			t.Fatalf("decode message_delta output token details: %v", err)
		}
		if details.ThinkingTokens != want {
			t.Fatalf("message_delta thinking_tokens = %d, want %d", details.ThinkingTokens, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !seenStart || !seenDelta {
		t.Fatalf("missing Anthropic usage events: message_start=%t message_delta=%t", seenStart, seenDelta)
	}
}
