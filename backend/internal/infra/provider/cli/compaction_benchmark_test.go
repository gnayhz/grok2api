package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// This companion benchmark isolates input preparation and the real Provider
// HTTP call from the full pipeline's SQL/Journal cost. The fixture is identical
// on both revisions and retains the public wire behavior in each path.
func BenchmarkCompactionInputProvider(b *testing.B) {
	for _, kind := range []string{"plain", "owned"} {
		b.Run(kind, func(b *testing.B) {
			var calls atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"resp_cost","object":"response","status":"completed","model":"grok-4.5","output":[]}`)
			}))
			defer upstream.Close()
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				b.Fatal(err)
			}
			token, err := cipher.Encrypt("synthetic")
			if err != nil {
				b.Fatal(err)
			}
			adapter := NewAdapter(Config{BaseURL: upstream.URL + "/v1"}, cipher)
			defer adapter.base.current.Load().CloseIdleConnections()
			prefix := ""
			if kind == "owned" {
				encrypted, err := cipher.Encrypt(`{"version":1,"session":"old","summary":"synthetic summary"}`)
				if err != nil {
					b.Fatal(err)
				}
				prefix = fmt.Sprintf(`{"type":"compaction","encrypted_content":%q},`, "g2a_compact_v1."+encrypted)
			}
			body := []byte(`{"input":[` + prefix + `{"role":"user","content":"` + strings.Repeat("synthetic text ", 2048) + `"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"id":{"type":"integer","const":17}}}}],"tool_choice":"none"}`)
			request := provider.ResponseResourceRequest{Credential: account.Credential{ID: 1, Provider: account.ProviderBuild, EncryptedAccessToken: token}, Method: http.MethodPost, Path: "/responses", Model: "grok-4.5", Operation: "responses", NormalizeBody: true, Body: body, PromptCacheKey: "cost"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				res, err := adapter.ForwardResponse(context.Background(), request)
				if err != nil {
					b.Fatal(err)
				}
				_, readErr := io.Copy(io.Discard, res.Body)
				_ = res.Body.Close()
				if readErr != nil || res.StatusCode != 200 {
					b.Fatalf("response=%d %v", res.StatusCode, readErr)
				}
			}
			b.StopTimer()
			if calls.Load() != int64(b.N) {
				b.Fatalf("physical calls=%d want=%d", calls.Load(), b.N)
			}
		})
	}
}
