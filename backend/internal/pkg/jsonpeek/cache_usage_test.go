package jsonpeek

import "testing"

func TestCacheUsageDistinguishesMissingFromExplicitZero(t *testing.T) {
	for _, tc := range []struct {
		name, fields string
		cached       int64
		reported     bool
	}{
		{"missing", ``, 0, false},
		{"empty_details", `,"input_tokens_details":{}`, 0, false},
		{"null", `,"cached_tokens":null`, 0, false},
		{"invalid_number", `,"cached_tokens":"0"`, 0, false},
		{"zero", `,"cached_tokens":0`, 0, true},
		{"nested_zero", `,"input_tokens_details":{"cached_tokens":0}`, 0, true},
		{"positive", `,"prompt_tokens_details":{"cached_tokens":128}`, 128, true},
		{"zero_precedence", `,"cache_read_input_tokens":0,"input_tokens_details":{"cached_tokens":128}`, 0, true},
		{"empty_fallback", `,"input_tokens_details":{},"prompt_tokens_details":{"cached_tokens":128}`, 128, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `{"input_tokens":256` + tc.fields + `}`
			for _, got := range []TokenUsage{TokenUsageObject([]byte(raw)), TokenUsageFrom([]byte(`{"response":{"usage":` + raw + `}}`))} {
				if !got.Found || got.Input != 256 || got.Cached != tc.cached || got.CachedReported != tc.reported {
					t.Fatalf("cache presence: %+v", got)
				}
			}
		})
	}
	got := TokenUsageFrom([]byte(`"usage":{"input_tokens":256,"cached_tokens":0`))
	if !got.Found || !got.CachedReported || got.Cached != 0 {
		t.Fatalf("fragment lost explicit zero: %+v", got)
	}
}
