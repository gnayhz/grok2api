package console

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestConsoleFractionalDeadlinesReachAllRequestFamilies(t *testing.T) {
	for _, family := range []string{"text", "image", "video", "voice"} {
		t.Run(family, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if serveTestDPoPToken(t, w, r) {
					return
				}
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				<-r.Context().Done()
			}))
			defer server.Close()
			adapter, credential := newConsoleTestAdapter(t, server.URL)
			adapter.UpdateConfig(Config{BaseURL: server.URL, Timeout: 175 * time.Millisecond, StreamIdleTimeout: 125 * time.Millisecond})
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			var err error
			switch family {
			case "text":
				var response *provider.Response
				response, err = adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-4.3", Body: []byte(`{"model":"grok-4.3","input":"hello"}`)})
				if response != nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
			case "image":
				_, err = adapter.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "grok-imagine-image", Prompt: "teapot", Count: 1})
			case "video":
				_, err = adapter.GenerateVideo(ctx, provider.VideoRequest{Credential: credential, Model: "grok-imagine-video", Prompt: "teapot", Duration: 6})
			case "voice":
				_, err = adapter.SynthesizeSpeech(ctx, provider.TTSRequest{Credential: credential, Text: "hello", Language: "en"})
			}
			if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil || calls.Load() != 1 || time.Since(start) >= time.Second {
				t.Fatalf("fractional provider deadline: calls=%d elapsed=%s parent=%v err=%v", calls.Load(), time.Since(start), ctx.Err(), err)
			}
		})
	}
}
