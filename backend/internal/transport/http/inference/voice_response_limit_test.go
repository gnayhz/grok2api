package inference

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

// A valid JSON prefix and whitespace must not hide an unread document suffix.
// Exercise real DPoP/HTTP, the registered public routes and SQL billing together.
func TestConsoleVoiceResponseLimitAtHTTP(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			for _, framing := range []string{"chunked", "declared"} {
				t.Run(framing, func(t *testing.T) {
					for _, tc := range []struct{ name, method, path, model, request, response string }{
						{"stt", http.MethodPost, "/v1/stt", "grok-stt", `{"url":"https://audio.example/synthetic.wav"}`, `{"text":"synthetic","duration":3.45}`},
						{"voices", http.MethodGet, "/v1/tts/voices", "grok-voice-latest", "", `{"voices":[{"voice_id":"ara","name":"Ara"}]}`},
						{"voice", http.MethodGet, "/v1/tts/voices/ara", "grok-voice-latest", "", `{"voice_id":"ara","name":"Ara"}`},
					} {
						t.Run(tc.name, func(t *testing.T) {
							var calls atomic.Int32
							upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
								w.Header().Set("Content-Type", "application/json")
								if calls.Load() > 1 {
									_, _ = w.Write([]byte(tc.response))
									return
								}
								const prefixLimit = (64 << 20) + 1
								const suffix = `{"invalid":"second document after the accepted prefix"}`
								if framing == "declared" {
									w.Header().Set("Content-Length", strconv.Itoa(prefixLimit+len(suffix)))
								}
								if _, err := w.Write([]byte(tc.response)); err != nil {
									return
								}
								spaces := bytes.Repeat([]byte{' '}, 32<<10)
								for remaining := prefixLimit - len(tc.response); remaining > 0; {
									n := min(remaining, len(spaces))
									if _, err := w.Write(spaces[:n]); err != nil {
										return
									}
									remaining -= n
								}
								_, _ = w.Write([]byte(suffix))
							})
							defer upstream.Close()
							fx := newProviderCompletionFixtureOnDatabase(t, upstream.URL, tc.model, account.ProviderConsole, nil, nil, nil, nil, compactionDatabase(t, dialect), "")
							send := func() *httptest.ResponseRecorder {
								r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.request))
								r.Header.Set("Authorization", "Bearer "+fx.created.Secret)
								r.Header.Set("Content-Type", "application/json")
								w := httptest.NewRecorder()
								fx.router.ServeHTTP(w, r)
								return w
							}
							w := send()
							if w.Code != http.StatusBadGateway {
								t.Errorf("incomplete upstream JSON accepted: status=%d delivered_bytes=%d", w.Code, w.Body.Len())
							}
							record := waitVoiceAudit(t, fx.audits)
							wantGeneration := "not_started"
							if tc.name == "stt" {
								wantGeneration = "unconfirmed"
							}
							if record.GenerationOutcome != wantGeneration || record.AudioDurationMS != 0 || record.UsageSource != audit.UsageSourceNone || record.EstimatedCostInUSDTicks != 0 {
								t.Errorf("unread response created generation/usage: generation=%s duration_ms=%d source=%s cost=%d", record.GenerationOutcome, record.AudioDurationMS, record.UsageSource, record.EstimatedCostInUSDTicks)
							}
							key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
							if err != nil || key.BilledUsageUSDTicks != 0 {
								t.Errorf("unread response billed: ticks=%d err=%v", key.BilledUsageUSDTicks, err)
							}
							if calls.Load() != 1 {
								t.Fatalf("response validation caused another upstream attempt: calls=%d", calls.Load())
							}
							w = send()
							if w.Code != http.StatusOK || calls.Load() != 2 {
								t.Fatalf("next response did not recover after closing the oversized body: status=%d calls=%d", w.Code, calls.Load())
							}
						})
					}

				})
			}
		})
	}
}

// Each public result shape must wait for clean upstream EOF. A usable prefix
// followed by truncation, another document or cancellation supplies no complete
// generation fact. The next request also proves account/network capacity release.
func TestConsoleVoiceResponseReadFailuresAtHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, method, path, model, request, response, contentType string
	}{
		{"tts_binary", http.MethodPost, "/v1/tts", "grok-voice-latest", `{"text":"synthetic","language":"en"}`, "synthetic audio", "audio/mpeg"},
		{"tts_json", http.MethodPost, "/v1/audio/speech", "grok-voice-latest", `{"input":"synthetic","with_timestamps":true}`, `{"audio":"c3ludGhldGlj","content_type":"audio/mpeg"}`, "application/json"},
		{"stt_native", http.MethodPost, "/v1/stt", "grok-stt", `{"url":"https://audio.example/synthetic.wav"}`, `{"text":"synthetic","duration":3.45}`, "application/json"},
		{"stt_compatible", http.MethodPost, "/v1/audio/transcriptions", "grok-stt", `{"url":"https://audio.example/synthetic.wav","response_format":"text"}`, `{"text":"synthetic","duration":3.45}`, "application/json"},
		{"voices", http.MethodGet, "/v1/tts/voices", "grok-voice-latest", "", `{"voices":[{"voice_id":"ara","name":"Ara"}]}`, "application/json"},
		{"voice", http.MethodGet, "/v1/tts/voices/ara", "grok-voice-latest", "", `{"voice_id":"ara","name":"Ara"}`, "application/json"},
	} {
		for _, stage := range []string{"truncated", "second_json", "cancel"} {
			if stage == "second_json" && tc.name == "tts_binary" {
				continue
			}
			t.Run(tc.name+"/"+stage, func(t *testing.T) {
				var calls atomic.Int32
				prefixSent := make(chan struct{})
				requestCtx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var entered, exited atomic.Int32
				upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					entered.Add(1)
					defer exited.Add(1)
					w.Header().Set("Content-Type", tc.contentType)
					if calls.Load() > 1 {
						_, _ = w.Write([]byte(tc.response))
						return
					}
					switch stage {
					case "truncated":
						w.Header().Set("Content-Length", strconv.Itoa(len(tc.response)+1024))
						_, _ = w.Write([]byte(tc.response))
					case "second_json":
						_, _ = w.Write([]byte(tc.response + ` {"second":true}`))
					case "cancel":
						_, _ = w.Write([]byte(tc.response))
						w.(fhttp.Flusher).Flush()
						close(prefixSent)
						<-r.Context().Done()
					}
				})
				defer upstream.Close()
				fx := newVoiceCompletionFixture(t, upstream.URL, tc.model)
				send := func(ctx context.Context) *httptest.ResponseRecorder {
					r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.request)).WithContext(ctx)
					r.Header.Set("Authorization", "Bearer "+fx.created.Secret)
					r.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					fx.router.ServeHTTP(w, r)
					return w
				}
				done := make(chan *httptest.ResponseRecorder, 1)
				go func() { done <- send(requestCtx) }()
				if stage == "cancel" {
					select {
					case <-prefixSent:
						cancel()
					case <-time.After(5 * time.Second):
						t.Fatal("upstream prefix not received")
					}
				}
				var w *httptest.ResponseRecorder
				select {
				case w = <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("request did not terminate")
				}
				if w.Code == http.StatusOK {
					t.Errorf("partial response returned success: stage=%s bytes=%d", stage, w.Body.Len())
				}
				record := waitVoiceAudit(t, fx.audits)
				if record.GenerationOutcome == "completed" || record.AudioDurationMS != 0 || record.UsageSource != audit.UsageSourceNone || record.EstimatedCostInUSDTicks != 0 || record.DeliveredBytes != 0 {
					t.Errorf("partial response supplied completed facts: %+v", record)
				}
				key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
				if err != nil || key.BilledUsageUSDTicks != 0 || key.ReservedUsageUSDTicks != 0 {
					t.Errorf("failed response retained billing: reserved=%d billed=%d err=%v", key.ReservedUsageUSDTicks, key.BilledUsageUSDTicks, err)
				}
				if calls.Load() != 1 {
					t.Fatalf("unexpected replay after partial response: %d", calls.Load())
				}
				deadline := time.Now().Add(time.Second)
				for entered.Load() != exited.Load() && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				if entered.Load() != exited.Load() {
					t.Fatal("upstream request body/connection not released before next request")
				}
				w = send(context.Background())
				if w.Code != http.StatusOK || calls.Load() != 2 {
					t.Fatalf("next normal request failed: status=%d calls=%d", w.Code, calls.Load())
				}
			})
		}
	}
}
