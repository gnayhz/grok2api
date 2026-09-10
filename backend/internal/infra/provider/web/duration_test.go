package web

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestWebFractionalDeadlinesReachHTTPAndWebSocket(t *testing.T) {
	for _, family := range []string{"text", "image", "video"} {
		t.Run(family, func(t *testing.T) {
			var calls atomic.Int32
			server := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
				calls.Add(1)
				if family == "video" {
					_, _ = io.Copy(io.Discard, r.Body)
					<-r.Context().Done()
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
			if err != nil {
				t.Fatal(err)
			}
			encrypted, err := cipher.Encrypt("synthetic-duration-credential")
			if err != nil {
				t.Fatal(err)
			}
			manager := infraegress.NewManager(egressRepositoryStub{}, cipher)
			defer manager.Close(context.Background())
			adapter := NewAdapter(Config{BaseURL: server.URL, StatsigMode: "manual", StatsigManualValue: "synthetic-signature", ChatTimeout: 175 * time.Millisecond, ImageTimeout: 175 * time.Millisecond, VideoTimeout: 175 * time.Millisecond}, manager, cipher, nil, nil)
			credential := account.Credential{ID: 1, Provider: account.ProviderWeb, UserID: "497f19f8-49d4-458a-bee4-43ec3dcaf8ca", EncryptedAccessToken: encrypted}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			if family == "text" {
				_, err = adapter.ForwardResponse(ctx, provider.ResponseResourceRequest{Credential: credential, Method: http.MethodPost, Path: "/responses", Model: "grok-chat-fast", Body: []byte(`{"model":"grok-chat-fast","input":"hello","stream":false}`)})
			} else if family == "image" {
				_, err = adapter.GenerateImage(ctx, provider.ImageGenerationRequest{Credential: credential, Model: "grok-imagine-image-quality", Prompt: "teapot", Count: 1})
			} else {
				_, err = adapter.GenerateVideo(ctx, provider.VideoRequest{Credential: credential, Prompt: "teapot", Duration: 6})
			}
			if err == nil || ctx.Err() != nil || calls.Load() != 1 || time.Since(start) < 150*time.Millisecond || time.Since(start) >= time.Second {
				t.Fatalf("fractional media deadline: calls=%d elapsed=%s parent=%v err=%v", calls.Load(), time.Since(start), ctx.Err(), err)
			}
		})
	}
}
