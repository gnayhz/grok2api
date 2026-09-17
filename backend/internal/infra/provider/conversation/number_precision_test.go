package conversation

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

const exactNumberObject = `{"id":9007199254740993,"min":-9007199254740993,"fraction":0.1234567890123456789,"large":1e100}`

func requireExactNumbers(t *testing.T, data []byte) {
	t.Helper()
	for _, number := range []string{"9007199254740993", "-9007199254740993", "0.1234567890123456789", "1e100"} {
		if !bytes.Contains(data, []byte(number)) {
			t.Errorf("lost exact number %s: %s", number, data)
		}
	}
}

func TestConversationRequestNumberPrecision(t *testing.T) {
	for _, tc := range []struct{ name, operation, body string }{
		{"chat function schema", OperationChat, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{"value":{"const":` + exactNumberObject + `}}}}}]}`},
		{"chat native tool", OperationChat, `{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"custom","options":` + exactNumberObject + `}]}`},
		{"chat tool result", OperationChat, `{"messages":[{"role":"tool","tool_call_id":"call_1","content":` + exactNumberObject + `}]}`},
		{"messages schema", OperationMessages, `{"max_tokens":256,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"value":{"const":` + exactNumberObject + `}}}}]}`},
		{"messages structured output", OperationMessages, `{"max_tokens":256,"messages":[{"role":"user","content":"hi"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","const":` + exactNumberObject + `}}}}`},
		{"messages tool history", OperationMessages, `{"max_tokens":256,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":` + exactNumberObject + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			converted, _, err := ConvertRequestWithOptions([]byte(tc.body), "grok-4.3", tc.operation)
			if err != nil {
				t.Fatal(err)
			}
			requireExactNumbers(t, converted)
		})
	}
}

func TestMessagesToolInputNumberPrecisionBothModes(t *testing.T) {
	item := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "lookup", "arguments": exactNumberObject}
	response := map[string]any{"id": "resp_1", "model": "grok-4.3", "status": "completed", "output": []any{item}}
	body, _ := json.Marshal(response)
	converted, err := ConvertResponseJSON(body, OperationMessages)
	if err != nil {
		t.Fatal(err)
	}
	requireExactNumbers(t, converted)
	added, _ := json.Marshal(map[string]any{"type": "response.output_item.added", "item": item})
	done, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "item": item})
	completed, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
	stream := "data: " + string(added) + "\n\ndata: " + string(done) + "\n\ndata: " + string(completed) + "\n\n"
	reader := ConvertResponseStreamWithOptions(io.NopCloser(strings.NewReader(stream)), OperationMessages, ResponseOptions{})
	defer reader.Close()
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	requireExactNumbers(t, output)
}

func TestChatNullFunctionReturnsError(t *testing.T) {
	_, _, err := ConvertRequestWithOptions([]byte(`{"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":null}]}`), "grok-4.3", OperationChat)
	if err == nil {
		t.Fatal("null function must return a validation error")
	}
}

func BenchmarkMessagesToolHistory(b *testing.B) {
	body := []byte(`{"max_tokens":256,"messages":[{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":` + exactNumberObject + `}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := ConvertRequestWithOptions(body, "grok-4.3", OperationMessages); err != nil {
			b.Fatal(err)
		}
	}
}

func TestResponsesNullInputReturnsError(t *testing.T) {
	if _, err := ConvertRequest([]byte("null"), "grok", OperationResponses); err == nil {
		t.Fatal("null request accepted")
	}
}
