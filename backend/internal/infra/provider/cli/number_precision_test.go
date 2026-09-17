package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

const precisionSchema = `{"type":"object","properties":{"id":{"type":"integer","const":9007199254740993},"score":{"type":"number","minimum":0.1234567890123456789}}}`

func TestBuildToolSchemaNumberPrecision(t *testing.T) {
	body := []byte(`{"input":"hi","tools":[{"type":"namespace","name":"db","tools":[{"type":"function","name":"lookup","parameters":` + precisionSchema + `}]}]}`)
	normalized, compatibility, err := normalizeResponsesRequestWithMetadata(body, "grok-4.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	check := func(raw []byte) {
		t.Helper()
		for _, number := range []string{"9007199254740993", "0.1234567890123456789"} {
			if !strings.Contains(string(raw), number) {
				t.Errorf("lost number %s: %s", number, raw)
			}
		}
	}
	check(normalized)
	upstream := []byte(`{"id":"resp_1","status":"completed","tools":[],"output":[]}`)
	response, err := compatibility.normalizeResponseJSON(upstream)
	if err != nil {
		t.Fatal(err)
	}
	check(response)
	stream := compatibility.normalizeResponseStream(io.NopCloser(strings.NewReader(`data: {"type":"response.completed","sequence_number":1,"response":` + string(upstream) + "}\n\n")))
	defer stream.Close()
	output, err := io.ReadAll(stream)
	if err != nil {
		t.Fatal(err)
	}
	check(output)
}

func TestCloneJSONTreeIsolationAndNumbers(t *testing.T) {
	original := map[string]any{"nested": []any{map[string]any{"id": json.Number("9007199254740993"), "name": "original"}}, "nil": nil}
	cloned := cloneJSONObject(original)
	child := cloned["nested"].([]any)[0].(map[string]any)
	if child["id"] != json.Number("9007199254740993") {
		t.Fatalf("number changed: %#v", child["id"])
	}
	child["name"] = "changed"
	cloned["nested"].([]any)[0] = "replaced"
	if original["nested"].([]any)[0].(map[string]any)["name"] != "original" {
		t.Fatal("cloned tree aliases the source")
	}
}

func TestBuildNumericProtocolControlsKeepDecimalForms(t *testing.T) {
	for _, raw := range []string{"7", "7.0", "7e0"} {
		if value, ok := exactJSONInt64(json.Number(raw)); !ok || value != 7 {
			t.Fatalf("sequence %q = %d, %v", raw, value, ok)
		}
		body := []byte(`{"input":[{"type":"local_shell_call_output","call_id":"call_1","output":"done","exit_code":` + raw + `}]}`)
		normalized, _, err := normalizeResponsesRequestWithMetadata(body, "grok-4.3", nil)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(normalized), `"exit_code":7`) {
			t.Fatalf("lost shell outcome %q: %s", raw, normalized)
		}
	}
}

func BenchmarkNormalizeBuildToolSchemas(b *testing.B) {
	tools := make([]string, 32)
	for i := range tools {
		tools[i] = fmt.Sprintf(`{"type":"function","name":"lookup_%d","parameters":%s}`, i, precisionSchema)
	}
	body := []byte(`{"input":"hi","tools":[` + strings.Join(tools, ",") + `]}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := normalizeResponsesRequestWithMetadata(body, "grok-4.3", nil); err != nil {
			b.Fatal(err)
		}
	}
}

func TestResponsesNullInputReturnsError(t *testing.T) {
	if _, _, err := normalizeResponsesRequestWithMetadata([]byte("null"), "grok-4.6", nil); err == nil {
		t.Fatal("null request accepted")
	}
}
