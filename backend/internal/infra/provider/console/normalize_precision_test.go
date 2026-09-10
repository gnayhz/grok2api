package console

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizeConsolePreservesSchemaAndSamplingNumbers(t *testing.T) {
	input := []byte(`{"model":"public","input":"hello","temperature":0.1234567890123456789,"seed":9007199254740993,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","enum":[9007199254740993,9223372036854775807],"minimum":-9007199254740993}}}}],"text":{"format":{"type":"json_schema","name":"exact","schema":{"type":"integer","const":9007199254740993}}}}`)
	output, err := normalizeRequest(input, ModelSpec{UpstreamModel: "grok-4.3"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{[]byte(`0.1234567890123456789`), []byte(`"seed":9007199254740993`), []byte(`9223372036854775807`), []byte(`-9007199254740993`), []byte(`"const":9007199254740993`)} {
		if !bytes.Contains(output, want) {
			t.Fatalf("normalization lost %s: %s", want, output)
		}
	}
	if !json.Valid(output) {
		t.Fatal("invalid normalized JSON")
	}
	second, err := normalizeRequest(output, ModelSpec{UpstreamModel: "grok-4.3"})
	if err != nil || !bytes.Equal(output, second) {
		t.Fatalf("normalization destabilized prefix: err=%v\n%s\n%s", err, output, second)
	}
}

func TestNormalizeConsoleRejectsNonObjectOrTrailingJSON(t *testing.T) {
	for _, input := range []string{"null", "[]", "{}{}", "{} true", "{} garbage"} {
		if _, err := normalizeRequest([]byte(input), ModelSpec{UpstreamModel: "grok-4.3"}); err == nil {
			t.Fatalf("accepted %q", input)
		}
	}
}
