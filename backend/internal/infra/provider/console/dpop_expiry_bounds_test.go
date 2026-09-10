package console

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
)

func TestDPoPTokenLifetimeRejectsOverflowBeforeDurationConversion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		seconds int64
		valid   bool
	}{
		{"normal", 300, true}, {"maximum", 3600, true}, {"zero", 0, false}, {"negative", -1, false}, {"over_limit", 3601, false},
		// On 64-bit systems this wraps to a positive duration around 119 seconds.
		{"positive_wrap", 18446744193, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tokenCalls, responseCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/dpop/token" {
					tokenCalls.Add(1)
					recorded := httptest.NewRecorder()
					serveTestDPoPToken(t, recorded, r)
					var payload map[string]any
					if err := json.Unmarshal(recorded.Body.Bytes(), &payload); err != nil {
						t.Error(err)
						return
					}
					payload["expires_in"] = tc.seconds
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(payload)
					return
				}
				responseCalls.Add(1)
				verifyTestDPoPProof(t, r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_expiry","object":"response","status":"completed","output":[]}`)
			}))
			t.Cleanup(server.Close)
			adapter, credential := newConsoleTestAdapter(t, server.URL)
			response, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
			if response != nil {
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
			}
			if tc.valid {
				if err != nil || response == nil || response.StatusCode != 200 || responseCalls.Load() != 1 {
					t.Fatalf("valid lifetime failed: response=%v err=%v calls=%d", response, err, responseCalls.Load())
				}
			} else if err == nil || responseCalls.Load() != 0 {
				t.Fatalf("invalid expires_in=%d accepted: err=%v inference_calls=%d", tc.seconds, err, responseCalls.Load())
			}
			if tokenCalls.Load() != 1 {
				t.Fatalf("mint_calls=%d", tokenCalls.Load())
			}
		})
	}
}
