package web

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestWebToolNumbersSurvivePromptParsingAndMessages(t *testing.T) {
	const numbers = `{"id":9007199254740993,"fraction":0.1234567890123456789}`
	tools := json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object","const":` + numbers + `}}]`)
	config, err := parseToolConfiguration(tools, nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func(s string) {
		t.Helper()
		for _, n := range []string{"9007199254740993", "0.1234567890123456789"} {
			if !strings.Contains(s, n) {
				t.Errorf("lost number %s: %s", n, s)
			}
		}
	}
	check(injectToolPrompt("hi", config))
	for _, syntax := range []string{
		`<tool_calls><tool_call><tool_name>lookup</tool_name><parameters>` + numbers + `</parameters></tool_call></tool_calls>`,
		`{"tool_calls":[{"name":"lookup","arguments":` + numbers + `}]}`,
	} {
		calls := parseToolCalls(syntax, config.available).Calls
		if len(calls) != 1 {
			t.Fatalf("missing tool call: %s", syntax)
		}
		check(calls[0].Arguments)
		result := buildOpenAIResult(conversation.OperationMessages, "resp_1", "grok-chat-fast", parsedChat{ToolCalls: calls}, false)
		output, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		check(string(output))
	}
}
