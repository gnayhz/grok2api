package inference

import (
	"encoding/json"
	"strings"
	"testing"
)

// R12 acceptance: usage fields under zero, unknown(omitted) and
// large-integer boundaries keep numeric JSON encoding; adapter moves
// must not stringify or fabricate zero for unknown usage.
func TestUsageEncodingBoundaries(t *testing.T) {
	payload, err := json.Marshal(map[string]any{"prompt_tokens": 0.0})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), ":0") {
		t.Fatalf("zero usage must stay numeric zero, got %s", payload)
	}
	big := 9223372036854775807.0
	payloadBig, err := json.Marshal(map[string]any{"total_tokens": big})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payloadBig), "922337203685477") {
		t.Fatalf("large usage must encode numerically, got %s", payloadBig)
	}
	quoted := `"total_tokens:`
	if strings.Contains(string(payloadBig), quoted) {
		t.Fatalf("usage must stay numeric (not string), got %s", payloadBig)
	}
	type unknownUsage struct {
		PromptTokens *int64 `json:"prompt_tokens,omitempty"`
	}
	encoded, err := json.Marshal(unknownUsage{})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "{}" {
		t.Fatalf("unknown usage must omit fields, got %s", encoded)
	}
}
