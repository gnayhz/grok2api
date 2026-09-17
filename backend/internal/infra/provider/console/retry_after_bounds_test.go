package console

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestConsoleRetryAfterDoesNotWrapLargeDelay(t *testing.T) {
	for _, source := range []string{"header", "body"} {
		for _, seconds := range []int64{300, 18446744193} {
			t.Run(fmt.Sprintf("%s/%d", source, seconds), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if serveTestDPoPToken(t, w, r) {
						return
					}
					verifyTestDPoPProof(t, r)
					w.Header().Set("Content-Type", "text/plain")
					if source == "header" {
						w.Header().Set("Retry-After", fmt.Sprint(seconds))
					}
					w.WriteHeader(http.StatusTooManyRequests)
					text := "Requests per Minute (actual/limit): 2/1"
					if source == "body" {
						text += fmt.Sprintf(". Resets in: %ds", seconds)
					}
					_, _ = io.WriteString(w, text)
				}))
				t.Cleanup(server.Close)
				adapter, credential := newConsoleTestAdapter(t, server.URL)
				r, err := adapter.ForwardResponse(context.Background(), provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Operation: conversation.OperationResponses, NormalizeBody: true, Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
				if err != nil {
					t.Fatal(err)
				}
				defer r.Body.Close()
				_, _ = io.Copy(io.Discard, r.Body)
				if r.StatusCode != 429 || r.RateLimit == nil {
					t.Fatalf("status=%d metadata=%+v", r.StatusCode, r.RateLimit)
				}
				if seconds == 300 && r.RateLimit.RetryAfter != 300*time.Second {
					t.Fatalf("ordinary RetryAfter=%v", r.RateLimit.RetryAfter)
				}
				if seconds > int64((1<<63-1)/time.Second) && r.RateLimit.RetryAfter < 24*time.Hour {
					t.Fatalf("unrepresentable %d seconds became short retry %v (header=%q)", seconds, r.RateLimit.RetryAfter, r.Header.Get("Retry-After"))
				}
			})
		}
	}
}

func TestConsoleMediaAndVoicePreserveLongRetryAfter(t *testing.T) {
	for _, operation := range []string{"tts", "video", "voice_ws"} {
		for _, seconds := range []int64{300, 18446744193} {
			t.Run(fmt.Sprintf("%s/%d", operation, seconds), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if serveTestDPoPToken(t, w, r) {
						return
					}
					verifyTestDPoPProof(t, r)
					calls.Add(1)
					w.Header().Set("Retry-After", fmt.Sprint(seconds))
					w.WriteHeader(429)
					_, _ = io.WriteString(w, `{"error":{"message":"rate limited"}}`)
				}))
				defer server.Close()
				adapter, credential := newConsoleTestAdapter(t, server.URL)
				var err error
				switch operation {
				case "tts":
					_, err = adapter.SynthesizeSpeech(context.Background(), provider.TTSRequest{Credential: credential, Text: "hello", Language: "en"})
				case "video":
					_, err = adapter.GenerateVideo(context.Background(), provider.VideoRequest{Credential: credential, Model: "grok-imagine-video", Prompt: "hello", Duration: 5})
				case "voice_ws":
					var cleanup func()
					_, cleanup, err = adapter.DialVoiceWebSocket(context.Background(), provider.VoiceWebSocketRequest{Credential: credential, Path: "/stt", Model: "grok-stt"})
					if cleanup != nil {
						cleanup()
					}
				}
				status, ok := provider.ErrorHTTPStatus(err)
				if err == nil || !ok || status != 429 || calls.Load() != 1 {
					t.Fatalf("status=%d calls=%d err=%v", status, calls.Load(), err)
				}
				want := 300 * time.Second
				if seconds > 100000 {
					want = time.Duration(1<<63 - 1)
				}
				if got := provider.ErrorRetryAfter(err); got != want {
					t.Fatalf("delay=%v want=%v", got, want)
				}
			})
		}
	}
}
