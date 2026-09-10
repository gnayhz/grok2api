package responsecheck

import "testing"

func TestGenerationRequiresExplicitProtocolEvidence(t *testing.T) {
	for _, tc := range []struct{ payload, want string }{
		{`{"status":"completed","output":[],"usage":{"input_tokens":0}}`, "completed"},
		{`{"status":"incomplete","output":[],"usage":{"input_tokens":20}}`, "failed"},
		{`{"usage":{"input_tokens":20}}`, "unconfirmed"},
		{`{"output":[{"text":"status completed","usage":{"input_tokens":20}}]}`, "unconfirmed"},
		{`{"status":"completed","output":[]`, "unconfirmed"},
		{`{"object":"chat.completion","choices":[{"finish_reason":"stop"}]}`, "completed"},
		{`{"object":"chat.completion","choices":[{"finish_reason":null}]}`, "unconfirmed"},
		{`{"type":"message","stop_reason":"end_turn"}`, "completed"},
		{`{"type":"message","stop_reason":null}`, "unconfirmed"},
	} {
		if got := JSONGeneration([]byte(tc.payload)); got != tc.want {
			t.Errorf("%s: %s want %s", tc.payload, got, tc.want)
		}
	}
}
