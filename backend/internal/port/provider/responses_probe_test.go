package provider

import (
	"errors"
	"io"
	"strings"
	"testing"
)

type failedProbeReader struct{}

func (failedProbeReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestResponsesProbeSeparatesRefusalAndCompletion(t *testing.T) {
	completed := `{"object":"response","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}]}`
	for _, tc := range []struct {
		name                   string
		status                 int
		body                   io.Reader
		wantErr, reauth, quota bool
	}{
		{name: "completed", status: 200, body: strings.NewReader(completed)},
		{name: "tool_completed", status: 200, body: strings.NewReader(`{"status":"completed","output":[{"type":"function_call","name":"tool","arguments":"{}"}]}`)},
		{name: "failed", status: 200, body: strings.NewReader(`{"status":"failed","output":[],"error":{"code":"server_error"}}`), wantErr: true},
		{name: "incomplete", status: 200, body: strings.NewReader(`{"status":"incomplete","output":[]}`), wantErr: true},
		{name: "empty_completed", status: 200, body: strings.NewReader(`{"status":"completed","output":[]}`), wantErr: true},
		{name: "reasoning_only", status: 200, body: strings.NewReader(`{"status":"completed","output":[{"type":"reasoning","summary":[{"text":"thinking"}]}]}`), wantErr: true},
		{name: "unknown", status: 200, body: strings.NewReader(`{"usage":{"output_tokens":5}}`), wantErr: true},
		{name: "wrong_protocol", status: 200, body: strings.NewReader(`{"object":"chat.completion","choices":[{"finish_reason":"stop"}]}`), wantErr: true},
		{name: "bad_json", status: 200, body: strings.NewReader(`{broken`), wantErr: true},
		{name: "empty_body", status: 200, wantErr: true},
		{name: "read_failed", status: 200, body: failedProbeReader{}, wantErr: true},
		{name: "over_limit", status: 200, body: strings.NewReader(completed + strings.Repeat(" ", MaxDiagnosticBodyBytes)), wantErr: true},
		{name: "http_auth_even_if_body_unreadable", status: 401, body: failedProbeReader{}, wantErr: true, reauth: true},
		{name: "spending_limit", status: 402, body: strings.NewReader(`{"code":"personal-team-blocked:spending-limit"}`), quota: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rejection, err := InspectResponsesProbe(tc.status, tc.body)
			if (err != nil) != tc.wantErr || rejection.Rejected != tc.reauth || rejection.SpendingLimitBlocked != tc.quota {
				t.Fatalf("facts=%+v err=%v", rejection, err)
			}
			if tc.name == "read_failed" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read failure lost its cause: %v", err)
			}
		})
	}
}
