package app

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonvalue"
)

type liveToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

func liveString(value any) string { s, _ := value.(string); return s }

func (r *liveInferenceRow) tool(key, id, name string) *liveToolCall {
	if r.toolIndexes == nil {
		r.toolIndexes = make(map[string]int)
	}
	i, ok := r.toolIndexes[key]
	if !ok {
		i = len(r.Tools)
		r.toolIndexes[key] = i
		r.Tools = append(r.Tools, liveToolCall{})
	}
	call := &r.Tools[i]
	if id != "" {
		call.ID = id
	}
	if name != "" {
		call.Name = name
	}
	return call
}

func (r *liveInferenceRow) observeTools(event map[string]any) {
	switch r.Operation {
	case "chat":
		choices, _ := event["choices"].([]any)
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			message, stream := choice["delta"].(map[string]any)
			if !stream {
				message, _ = choice["message"].(map[string]any)
			}
			calls, _ := message["tool_calls"].([]any)
			for index, rawCall := range calls {
				item, _ := rawCall.(map[string]any)
				key := fmt.Sprint(index)
				if value, ok := item["index"]; ok {
					key = fmt.Sprint(value)
				}
				function, _ := item["function"].(map[string]any)
				call := r.tool(key, liveString(item["id"]), liveString(function["name"]))
				if stream {
					call.Arguments += liveString(function["arguments"])
				} else {
					call.Arguments = liveString(function["arguments"])
				}
			}
		}
	case "responses":
		note := func(item map[string]any) {
			if item["type"] != "function_call" {
				return
			}
			call := r.tool(liveString(item["id"]), liveString(item["call_id"]), liveString(item["name"]))
			if args := liveString(item["arguments"]); args != "" {
				call.Arguments = args
			}
		}
		if item, ok := event["item"].(map[string]any); ok {
			note(item)
		}
		if event["type"] == "response.function_call_arguments.delta" {
			r.tool(liveString(event["item_id"]), "", "").Arguments += liveString(event["delta"])
		}
		if event["type"] == "response.function_call_arguments.done" {
			r.tool(liveString(event["item_id"]), "", "").Arguments = liveString(event["arguments"])
		}
		root := event
		if nested, ok := event["response"].(map[string]any); ok {
			root = nested
		}
		output, _ := root["output"].([]any)
		for _, raw := range output {
			if item, ok := raw.(map[string]any); ok {
				note(item)
			}
		}
	case "messages":
		note := func(key string, item map[string]any) {
			if item["type"] != "tool_use" {
				return
			}
			call := r.tool(key, liveString(item["id"]), liveString(item["name"]))
			if input, ok := item["input"].(map[string]any); ok && len(input) > 0 {
				raw, _ := json.Marshal(input)
				call.Arguments = string(raw)
			}
		}
		if block, ok := event["content_block"].(map[string]any); ok {
			note(fmt.Sprint(event["index"]), block)
		}
		if delta, ok := event["delta"].(map[string]any); ok && delta["type"] == "input_json_delta" {
			r.tool(fmt.Sprint(event["index"]), "", "").Arguments += liveString(delta["partial_json"])
		}
		content, _ := event["content"].([]any)
		for index, raw := range content {
			if item, ok := raw.(map[string]any); ok {
				note(fmt.Sprint(index), item)
			}
		}
	}
}

func liveBody(model, operation, prompt string, stream bool) map[string]any {
	body := map[string]any{"model": model, "stream": stream}
	if operation == "responses" {
		body["input"] = prompt
		body["max_output_tokens"] = 4096
	} else {
		body["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		if operation == "messages" {
			body["max_tokens"] = 4096
		} else {
			body["max_completion_tokens"] = 4096
		}
	}
	return body
}

func livePath(operation string) string {
	return map[string]string{"chat": "/v1/chat/completions", "responses": "/v1/responses", "messages": "/v1/messages"}[operation]
}

func runLiveWorkloads(t *testing.T, client *http.Client, base, admin, key, provider, model string, rows *[]liveInferenceRow) {
	t.Helper()
	for _, operation := range []string{"chat", "responses", "messages"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("tool_roundtrip/%s/stream=%v", operation, stream), func(t *testing.T) {
				const prompt = "Call lookup_test_value once using the required value. After receiving the tool result, reply with its marker exactly."
				body := liveBody(model, operation, prompt, stream)
				schema := map[string]any{"type": "object", "properties": map[string]any{"value": map[string]any{"type": "integer", "enum": []any{json.Number("9007199254740993")}}}, "required": []string{"value"}, "additionalProperties": false}
				function := map[string]any{"name": "lookup_test_value", "description": "Read a test marker; no external effects.", "parameters": schema}
				switch operation {
				case "chat":
					body["tools"] = []any{map[string]any{"type": "function", "function": function}}
					body["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": "lookup_test_value"}}
				case "responses":
					function["type"] = "function"
					body["tools"] = []any{function}
					body["tool_choice"] = map[string]any{"type": "function", "name": "lookup_test_value"}
				case "messages":
					body["tools"] = []any{map[string]any{"name": "lookup_test_value", "description": "Read a test marker; no external effects.", "input_schema": schema}}
					body["tool_choice"] = map[string]any{"type": "tool", "name": "lookup_test_value"}
				}
				row := liveInferenceRow{Provider: provider, Model: model, Operation: operation, Streaming: stream, Scenario: "tool_call"}
				err := readLiveInference(client, base+livePath(operation), key, body, &row)
				*rows = append(*rows, row)
				if err != nil {
					t.Fatal(err)
				}
				if !row.Terminal || len(row.Tools) != 1 {
					t.Fatalf("terminal=%v tools=%+v", row.Terminal, row.Tools)
				}
				call := row.Tools[0]
				var args map[string]any
				if call.ID == "" || call.Name != "lookup_test_value" || jsonvalue.Unmarshal([]byte(call.Arguments), &args) != nil || fmt.Sprint(args["value"]) != "9007199254740993" {
					t.Fatalf("tool call did not preserve schema value: %+v", call)
				}
				const marker = "VERIFY_OK_9B2F"
				body["tool_choice"] = "none"
				switch operation {
				case "chat":
					body["messages"] = []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}}}}, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": marker}}
				case "responses":
					body["input"] = []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"type": "function_call", "call_id": call.ID, "name": call.Name, "arguments": call.Arguments}, map[string]any{"type": "function_call_output", "call_id": call.ID, "output": marker}}
				case "messages":
					body["tool_choice"] = map[string]any{"type": "none"}
					body["messages"] = []any{map[string]any{"role": "user", "content": prompt}, map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": args}}}, map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": marker}}}}
				}
				follow := liveInferenceRow{Provider: provider, Model: model, Operation: operation, Streaming: stream, Scenario: "tool_result"}
				err = readLiveInference(client, base+livePath(operation), key, body, &follow)
				*rows = append(*rows, follow)
				if err != nil || !follow.Terminal || !strings.Contains(follow.Answer, marker) {
					t.Fatalf("followup err=%v terminal=%v answer=%q", err, follow.Terminal, follow.Answer)
				}
			})
		}
	}
	t.Run("concurrent_streams", func(t *testing.T) {
		batch := make([]liveInferenceRow, 4)
		var workers sync.WaitGroup
		for i := range batch {
			workers.Add(1)
			go func(i int) {
				defer workers.Done()
				row := &batch[i]
				*row = liveInferenceRow{Provider: provider, Model: model, Operation: "responses", Streaming: true, Scenario: "concurrent_stream"}
				err := readLiveInference(client, base+livePath("responses"), key, liveBody(model, "responses", "Reply exactly CONCURRENT_OK.", true), row)
				if err != nil || !row.Terminal || !strings.Contains(row.Answer, "CONCURRENT_OK") {
					t.Errorf("worker %d err=%v terminal=%v answer=%q", i, err, row.Terminal, row.Answer)
				}
			}(i)
		}
		workers.Wait()
		*rows = append(*rows, batch...)
	})
	t.Run("cache_branches", func(t *testing.T) {
		if provider != "grok_build" || os.Getenv("GROK2API_LIVE_TARGET_URL") != "" {
			t.Skip("branch test uses the copied Build route only")
		}
		account := os.Getenv("GROK2API_LIVE_BRANCH_ACCOUNT")
		if account == "" || account == "4045" || account == "4102" {
			t.Skip("set an eligible branch test account")
		}
		traceDir := t.TempDir()
		t.Setenv("GROK2API_UPSTREAM_TRACE_DIR", traceDir)
		routes := liveJSON(t, client, base+"/api/admin/v1/models?pageSize=100", admin, http.MethodGet, nil)
		for _, raw := range routes["data"].(map[string]any)["items"].([]any) {
			route := raw.(map[string]any)
			if route["upstreamModel"] == model {
				liveJSON(t, client, base+"/api/admin/v1/models/"+liveString(route["id"]), admin, http.MethodPatch, map[string]any{"accountIds": []string{account}})
				break
			}
		}
		cacheKey := fmt.Sprintf("branch-main-%d", time.Now().UnixNano())
		var reference strings.Builder
		fmt.Fprintf(&reference, "Experiment %s.\n", cacheKey)
		for i := 0; i < 500; i++ {
			fmt.Fprintf(&reference, "Record %04d: code %d; category %d.\n", i, i*17+3, i%23)
		}
		history := []any{map[string]any{"type": "message", "role": "user", "content": "Reply exactly MAIN_OK."}}
		mainTurn := 0
		for step, aux := range []bool{false, false, true, false, true, false, false} {
			body := liveBody(model, "responses", "", true)
			body["store"], body["include"] = false, []string{"reasoning.encrypted_content"}
			body["reasoning"] = map[string]any{"effort": "low", "summary": "concise"}
			body["max_output_tokens"] = 256
			want, label := "MAIN_OK", "main"
			if aux {
				body["prompt_cache_key"], body["instructions"], body["input"] = cacheKey+"-aux", "Independent title task.", "Reply exactly AUX_OK."
				want, label = "AUX_OK", "aux"
			} else {
				mainTurn++
				input := make([]any, 0, len(history))
				for _, raw := range history {
					item := raw.(map[string]any)
					if mainTurn <= 3 && item["type"] == "reasoning" {
						continue
					}
					input = append(input, raw)
				}
				body["prompt_cache_key"], body["instructions"], body["input"] = cacheKey, reference.String(), input
			}
			headers := http.Header{"X-Codex-Window-Id": {"shared-window"}}
			if mainTurn >= 4 {
				headers.Set("X-Codex-Window-Id", "new-window-same-cache")
			}
			row := liveInferenceRow{Provider: provider, Model: model, Operation: "responses", Streaming: true, Scenario: fmt.Sprintf("branch_%d_%s", step+1, label)}
			err := readLiveInference(client, base+livePath("responses"), key, body, &row, headers)
			*rows = append(*rows, row)
			report, _ := json.Marshal(row)
			t.Log(string(report))
			if err != nil || !row.Terminal || !strings.Contains(row.Answer, want) {
				t.Fatalf("step %d err=%v terminal=%v", step+1, err, row.Terminal)
			}
			if !aux {
				history = append(history, row.Output...)
				history = append(history, map[string]any{"type": "message", "role": "user", "content": "Reply exactly MAIN_OK again."})
			}
		}
		files, err := filepath.Glob(filepath.Join(traceDir, "*.req.json"))
		if err != nil || len(files) != 7 {
			t.Fatalf("expected seven actual upstream request traces: count=%d err=%v", len(files), err)
		}
		var previous []any
		var mainOptions map[string]any
		mainCache, auxCache := "", ""
		for i, path := range files {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]any
			if err := json.Unmarshal(data, &body); err != nil {
				t.Fatal(err)
			}
			cache := liveString(body["prompt_cache_key"])
			if i == 2 || i == 4 {
				if cache == "" || cache == mainCache || auxCache != "" && auxCache != cache {
					t.Fatal("auxiliary cache identity not isolated and stable")
				}
				auxCache = cache
				continue
			}
			if mainCache != "" && cache != mainCache || cache == "" {
				t.Fatal("main upstream cache identity changed")
			}
			mainCache = cache
			input, ok := body["input"].([]any)
			if !ok || len(input) < len(previous) || !reflect.DeepEqual(previous, input[:len(previous)]) && len(previous) > 0 {
				t.Fatalf("step %d changed actual upstream input prefix", i+1)
			}
			previous = input
			delete(body, "input")
			if mainOptions != nil && !reflect.DeepEqual(mainOptions, body) {
				t.Fatalf("step %d changed non-input upstream fields", i+1)
			}
			mainOptions = body
		}
		t.Log("verified: main upstream key and options stable; auxiliary key isolated; all four main transitions preserve the actual upstream input prefix")
	})
	t.Run("append_only_cache", func(t *testing.T) {
		cacheKey := fmt.Sprintf("append-cache-%s-%d", provider, time.Now().UnixNano())
		history := []any{map[string]any{"type": "message", "role": "user", "content": "Explain one benefit of stable prompt prefixes in about 40 words."}}
		instructions := "Experiment " + cacheKey + ".\n" + strings.Repeat("Reference: Keep earlier conversation messages and encrypted reasoning unchanged; append each new turn at the end. ", 160)
		for turn := 1; turn <= 5; turn++ {
			input := make([]any, 0, len(history))
			for _, raw := range history {
				item, _ := raw.(map[string]any)
				// Exercise server restoration, then switch to the native client's
				// full output replay without changing any conversation content.
				if turn <= 3 && item["type"] == "reasoning" {
					continue
				}
				input = append(input, raw)
			}
			body := liveBody(model, "responses", "", true)
			body["input"], body["instructions"], body["prompt_cache_key"] = input, instructions, cacheKey
			body["store"], body["include"] = false, []string{"reasoning.encrypted_content"}
			body["reasoning"] = map[string]any{"effort": "low", "summary": "detailed"}
			row := liveInferenceRow{Provider: provider, Model: model, Operation: "responses", Streaming: true, Scenario: fmt.Sprintf("append_cache_turn_%d", turn)}
			err := readLiveInference(client, base+livePath("responses"), key, body, &row)
			*rows = append(*rows, row)
			report, _ := json.Marshal(row)
			t.Log(string(report))
			if err != nil || !row.Terminal || row.Answer == "" || len(row.Output) == 0 {
				t.Fatalf("turn=%d err=%v terminal=%v output_items=%d", turn, err, row.Terminal, len(row.Output))
			}
			history = append(history, row.Output...)
			history = append(history, map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("Give example %d in about 40 words.", turn)})
		}
	})
	t.Run("long_prefix_cache", func(t *testing.T) {
		// Pin the copied route to an account observed in this test. This makes
		// repeated-prefix measurements independent of account rotation.
		audits := liveJSON(t, client, base+"/api/admin/v1/request-audits?pageSize=100", admin, http.MethodGet, nil)
		wanted := map[string]bool{}
		for _, row := range *rows {
			if row.Provider == provider && row.Status == 200 {
				wanted[row.RequestID] = true
			}
		}
		account := ""
		for _, raw := range audits["data"].(map[string]any)["items"].([]any) {
			item := raw.(map[string]any)
			if wanted[liveString(item["requestId"])] && liveString(item["accountId"]) != "" {
				account = liveString(item["accountId"])
				break
			}
		}
		if account == "" {
			t.Fatal("no observed account for cache control")
		}
		if os.Getenv("GROK2API_LIVE_TARGET_URL") == "" {
			routes := liveJSON(t, client, base+"/api/admin/v1/models?pageSize=100", admin, http.MethodGet, nil)
			for _, raw := range routes["data"].(map[string]any)["items"].([]any) {
				route := raw.(map[string]any)
				if route["upstreamModel"] == model {
					liveJSON(t, client, base+"/api/admin/v1/models/"+liveString(route["id"]), admin, http.MethodPatch, map[string]any{"accountIds": []string{account}})
					break
				}
			}
		}
		var prefix strings.Builder
		for i := range 500 {
			fmt.Fprintf(&prefix, "Reference record %04d has value %d.\n", i, i*17+3)
		}
		cacheKey := fmt.Sprintf("sixdim-%s-%d", provider, time.Now().UnixNano())
		for turn := range 3 {
			body := liveBody(model, "responses", "Reply exactly CACHE_OK.", true)
			body["instructions"] = prefix.String() + "\nUse the reference only if asked."
			body["prompt_cache_key"] = cacheKey
			if turn == 2 {
				body["input"] = []any{map[string]any{"role": "user", "content": "Reply exactly CACHE_OK."}, map[string]any{"role": "assistant", "content": "CACHE_OK"}, map[string]any{"role": "user", "content": "What is the value of reference record 0123? Reply with only the number."}}
			}
			row := liveInferenceRow{Provider: provider, Model: model, Operation: "responses", Streaming: true, Scenario: fmt.Sprintf("cache_turn_%d", turn)}
			err := readLiveInference(client, base+livePath("responses"), key, body, &row)
			*rows = append(*rows, row)
			want := "CACHE_OK"
			if turn == 2 {
				want = "2094"
			}
			if err != nil || !row.Terminal || !strings.Contains(row.Answer, want) {
				t.Errorf("turn %d err=%v terminal=%v answer=%q", turn, err, row.Terminal, row.Answer)
			}
		}
	})
	t.Run("long_stream", func(t *testing.T) {
		row := liveInferenceRow{Provider: provider, Model: model, Operation: "responses", Streaming: true, Scenario: "long_stream"}
		err := readLiveInference(client, base+livePath("responses"), key, liveBody(model, "responses", "Output all integers from 1 through 256, separated by commas. Do not skip any integer. Do not include any other text.", true), &row)
		*rows = append(*rows, row)
		if err != nil || !row.Terminal {
			t.Fatalf("long stream err=%v terminal=%v", err, row.Terminal)
		}
		numbers := strings.FieldsFunc(row.Answer, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t' })
		if len(numbers) != 256 {
			t.Fatalf("long stream number count=%d", len(numbers))
		}
		for i, n := range numbers {
			if n != strconv.Itoa(i+1) {
				t.Fatalf("number %d=%q", i, n)
			}
		}
		t.Logf("first_text_ms=%d last_text_ms=%d elapsed_ms=%d events=%d", row.FirstTextMS, row.LastTextMS, row.ElapsedMS, row.Events)
	})
	t.Run("client_cancel", func(t *testing.T) {
		row := liveInferenceRow{Provider: provider, Model: model, Operation: "responses", Streaming: true, Scenario: "client_cancel", CancelAfterText: true}
		err := readLiveInference(client, base+livePath("responses"), key, liveBody(model, "responses", "Write a numbered list from 1 to 5000, one number per line. Do not summarize or skip numbers.", true), &row)
		*rows = append(*rows, row)
		if err != nil || !row.Cancelled {
			t.Fatalf("cancel test err=%v cancelled=%v", err, row.Cancelled)
		}
	})
}
