package cli

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	reasoningreplay "github.com/chenyme/grok2api/backend/internal/application/history"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestForwardResponseDefersReplayUntilGuardAccepts(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, operation := range []string{"responses", "messages"} {
			t.Run(operation+map[bool]string{false: "/json", true: "/stream"}[streaming], func(t *testing.T) {
				cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
				if err != nil {
					t.Fatal(err)
				}
				token, err := cipher.Encrypt("access-token")
				if err != nil {
					t.Fatal(err)
				}
				store := memory.NewReasoningReplayStore(10)
				adapter := NewAdapter(Config{BaseURL: "https://cli-chat-proxy.grok.com/v1"}, cipher)
				adapter.SetReasoningReplay(reasoningreplay.New(store, reasoningreplay.Config{Enabled: true, TTL: time.Hour}, nil))
				raw := make([]byte, 256)
				for i := range raw {
					raw[i] = byte(i)
				}
				enc := base64.RawStdEncoding.EncodeToString(raw)
				payload := `{"id":"r1","output":[{"type":"reasoning","encrypted_content":"` + enc + `"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
				if streaming {
					payload = "data: {\"type\":\"response.completed\",\"response\":" + payload + "}\n\n"
				}
				adapter.http.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 200, Status: "200 OK", Header: make(http.Header), Body: io.NopCloser(strings.NewReader(payload)), Request: req}, nil
				})
				request := provider.ResponseResourceRequest{Credential: account.Credential{ID: 7, Provider: account.ProviderBuild, EncryptedAccessToken: token}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.6", Operation: operation, ReasoningReplayKey: "guard-session", Body: []byte(`{"model":"grok-4.6","input":"hello"}`), Streaming: streaming, DeferOutputCommit: true}
				response, err := adapter.ForwardResponse(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				if response.AcceptOutput == nil {
					t.Fatal("adapter lost acceptance callback")
				}
				if _, err := io.Copy(io.Discard, response.Body); err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				scope := adapter.scopedReasoningReplayKey(request, "https://cli-chat-proxy.grok.com/v1")
				_, found, err := store.Get(context.Background(), request.Model, scope, time.Now(), time.Hour)
				if err != nil || found {
					t.Fatalf("unaccepted response cached: found=%v err=%v", found, err)
				}
				response.AcceptOutput()
				_, found, err = store.Get(context.Background(), request.Model, scope, time.Now(), time.Hour)
				if err != nil || !found {
					t.Fatalf("accepted response missing: found=%v err=%v", found, err)
				}
			})
		}
	}
}
