package responsecheck

import (
	"errors"
	"testing"
)

func TestCompletionRequiresOutput(t *testing.T) {
	for _, tc := range []struct {
		name, prefix, output string
		wantError            bool
	}{
		{"empty", "", `[]`, true},
		{"reasoning", `{"type":"response.reasoning_text.delta","delta":"thinking"}`, `[{"type":"reasoning","summary":[{"text":"thinking"}]}]`, true},
		{"whitespace", `{"type":"response.output_text.delta","delta":" \n"}`, `[]`, true},
		{"delta", `{"type":"response.output_text.delta","delta":"answer"}`, `[]`, false},
		{"aggregate", "", `[{"type":"message","content":[{"type":"output_text","text":"answer"}]}]`, false},
		{"refusal", "", `[{"type":"message","content":[{"type":"refusal","refusal":"cannot help"}]}]`, false},
		{"tool", "", `[{"type":"function_call","name":"clock","arguments":"{}"}]`, false},
		{"search", "", `[{"type":"web_search_call"}]`, false},
		{"multimodal", "", `[{"type":"message","content":[{"type":"output_audio"}]}]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var state Responses
			if err := state.Observe("", []byte(tc.prefix)); err != nil {
				t.Fatal(err)
			}
			body := `{"status":"completed","output":` + tc.output + `}`
			err := state.Observe("", []byte(`{"type":"response.completed","response":`+body+`}`))
			if errors.Is(err, ErrEmptyOutput) != tc.wantError {
				t.Fatalf("error=%v wantError=%v", err, tc.wantError)
			}
			if tc.prefix == "" && errors.Is(JSON([]byte(body)), ErrEmptyOutput) != tc.wantError {
				t.Fatal("JSON/SSE disagreement")
			}
		})
	}
}

func TestIncompleteAndFailedRemainUnchanged(t *testing.T) {
	for _, kind := range []string{"response.incomplete", "response.failed", "error"} {
		var state Responses
		if err := state.Observe(kind, []byte(`{"response":{"output":[]}}`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := JSON([]byte(`{"status":"incomplete","output":[]}`)); err != nil {
		t.Fatal(err)
	}
}
