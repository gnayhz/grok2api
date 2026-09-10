package inference

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
)

func TestSTTRequestCannotSilentlyChangeParameterMeaning(t *testing.T) {
	for _, tc := range []struct{ name, payload, param string }{
		{"fractional_sample_rate", `{"url":"https://audio.example/input.wav","sample_rate":16000.9}`, "sample_rate"},
		{"fractional_channels", `{"url":"https://audio.example/input.wav","channels":2.9}`, "channels"},
		{"invalid_boolean", `{"url":"https://audio.example/input.wav","diarize":"flase"}`, "diarize"},
		{"object_boolean", `{"url":"https://audio.example/input.wav","format":{"value":true}}`, "format"},
		{"invalid_channels", `{"url":"https://audio.example/input.wav","channels":"two"}`, "channels"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Error(err)
				}
				t.Logf("actual upstream %s=%q", tc.param, r.FormValue(tc.param))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"text": "synthetic", "duration": 1})
			})
			defer upstream.Close()
			fx := newVoiceCompletionFixture(t, upstream.URL, "grok-stt")
			r := httptest.NewRequest(http.MethodPost, "/v1/stt", strings.NewReader(tc.payload))
			r.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			fx.router.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || calls.Load() != 0 {
				t.Fatalf("parameter meaning silently changed: status=%d calls=%d", w.Code, calls.Load())
			}
		})
	}
}
