package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/gin-gonic/gin"
)

func TestRESTVoiceKnownGenerationSurvivesClose(t *testing.T) {
	var calls atomic.Int32
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"synthetic","duration":3.45}`))
	})
	defer upstream.Close()
	fx := newVoiceCompletionFixture(t, upstream.URL, "grok-stt")
	result, err := fx.service.TranscribeSpeech(context.Background(), gateway.STTInput{
		RequestID: "rest-close", ClientKey: fx.created.Key, PublicModel: "grok-stt", FileData: []byte("synthetic"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Body.Close(); err != nil {
		t.Fatal(err)
	}
	record := waitVoiceAudit(t, fx.audits)
	if record.EstimatedCostInUSDTicks <= 0 || record.AudioDurationMS != 3450 || record.GenerationOutcome != "completed" {
		t.Fatalf("known generation lost after body close: cost=%d duration=%d generation=%q delivery=%q", record.EstimatedCostInUSDTicks, record.AudioDurationMS, record.GenerationOutcome, record.DeliveryOutcome)
	}
}

func TestRESTVoiceGenerationAndDeliveryIntegration(t *testing.T) {
	for _, modality := range []string{"tts_binary", "tts_json", "stt_native", "stt_text"} {
		for _, stage := range []string{"success", "short_write", "write_error", "close", "cancel_unclaimed", "cancel_claimed", "receipt", "authorization_retry"} {
			t.Run(modality+"/"+stage, func(t *testing.T) {
				var calls atomic.Int32
				upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
					if stage == "authorization_retry" && calls.Load() == 1 {
						w.WriteHeader(401)
						return
					}
					if strings.HasPrefix(modality, "tts") {
						var request struct {
							Text string `json:"text"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Text != "Hello 世界" {
							t.Errorf("sent text=%q err=%v", request.Text, err)
						}
					}
					if modality == "tts_binary" {
						w.Header().Set("Content-Type", "audio/mpeg")
						_, _ = w.Write([]byte("synthetic audio"))
					} else if modality == "tts_json" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"audio":"c3ludGhldGljIGF1ZGlv","content_type":"audio/mpeg","duration":3.45}`))
					} else {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"text":"synthetic audio","duration":3.45}`))
					}
				})
				defer upstream.Close()
				model := "grok-stt"
				if strings.HasPrefix(modality, "tts") {
					model = "grok-voice-latest"
				}
				fx := newVoiceCompletionFixture(t, upstream.URL, model)
				fx.receipts.fail = stage == "receipt"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var result *gateway.Result
				var err error
				if strings.HasPrefix(modality, "tts") {
					result, err = fx.service.SynthesizeSpeech(ctx, gateway.TTSInput{RequestID: "rest", ClientKey: fx.created.Key, PublicModel: model, Text: "  Hello 世界  ", Language: "en", WithTimestamps: modality == "tts_json"})
				} else {
					format := ""
					if modality == "stt_text" {
						format = "text"
					}
					result, err = fx.service.TranscribeSpeech(ctx, gateway.STTInput{RequestID: "rest", ClientKey: fx.created.Key, PublicModel: model, FileData: []byte("synthetic audio"), ResponseFormat: format})
				}
				if err != nil {
					t.Fatal(err)
				}
				var wantBytes int64
				delivery, admission := "completed", "admitted"
				if stage == "close" {
					_ = result.Body.Close()
					delivery, admission = "not_started", "not_admitted"
				} else if stage == "cancel_unclaimed" {
					cancel()
					delivery, admission = "canceled", "not_admitted"
				} else {
					if stage == "cancel_claimed" {
						if err := result.BeginDelivery(); err != nil {
							t.Fatal(err)
						}
						cancel()
						delivery, admission = "canceled", "not_admitted"
					}
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/tts", nil).WithContext(ctx)
					if stage == "short_write" || stage == "write_error" {
						writer := &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
						if stage == "write_error" {
							writer.writeErr = errors.New("client gone")
						}
						c.Writer = writer
						delivery = "canceled"
					}
					(&Handler{}).writeMediaResult(c, result)
					wantBytes = int64(recorder.Body.Len())
				}
				record := waitVoiceAudit(t, fx.audits)
				if record.GenerationOutcome != "completed" || record.DeliveryOutcome != delivery || record.AdmissionOutcome != admission || record.DeliveredBytes != wantBytes || record.UpstreamStatusCode != 200 {
					t.Fatalf("completion: gen=%s delivery=%s admission=%s bytes=%d/%d upstream=%d", record.GenerationOutcome, record.DeliveryOutcome, record.AdmissionOutcome, record.DeliveredBytes, wantBytes, record.UpstreamStatusCode)
				}
				wantCost := int64(1_200_000)
				if strings.HasPrefix(modality, "stt") {
					price, _ := audit.EstimateOfficialSTTCost(3.45, false)
					wantCost = price.CostInUSDTicks
					if record.UsageSource != audit.UsageSourceUpstream || record.AudioDurationMS != 3450 {
						t.Fatalf("duration: %+v", record)
					}
				}
				key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
				if err != nil || record.EstimatedCostInUSDTicks != wantCost || key.BilledUsageUSDTicks != wantCost {
					t.Fatalf("cost: audit=%d billing=%d want=%d err=%v", record.EstimatedCostInUSDTicks, key.BilledUsageUSDTicks, wantCost, err)
				}
				wantReceipt := "committed"
				if stage == "receipt" {
					wantReceipt = "failed"
				}
				if record.PhysicalReceipt != wantReceipt {
					t.Fatalf("receipt: %s", record.PhysicalReceipt)
				}
				fx.receipts.mu.Lock()

				wantCalls := 2
				if stage == "authorization_retry" {
					wantCalls = 4
				}
				if len(fx.receipts.facts) != wantCalls {
					t.Fatalf("physical calls: %d want %d", len(fx.receipts.facts), wantCalls)
				}
				for i, fact := range fx.receipts.facts {
					if fact.Attempt.Ordinal != uint64(i+1) || fact.Attempt.AccountID != fx.account.ID || fact.Attempt.RequestID != record.EventID || fact.BodyOutcome == "pending" {
						t.Fatalf("physical identity or completion: %+v", fact)
					}
				}
				fx.receipts.mu.Unlock()
				result.Finalize(gateway.Usage{}, "", "duplicate")
				_ = result.Body.Close()
				_, total, err := fx.audits.List(context.Background(), 0, 2)
				if err != nil || total != 1 {
					t.Fatalf("duplicate finalization: count=%d err=%v", total, err)
				}
				if stage == "cancel_unclaimed" || stage == "cancel_claimed" {
					var next *gateway.Result
					if strings.HasPrefix(modality, "tts") {
						next, err = fx.service.SynthesizeSpeech(context.Background(), gateway.TTSInput{RequestID: "after-cancel", ClientKey: fx.created.Key, PublicModel: model, Text: "Hello 世界", Language: "en", WithTimestamps: modality == "tts_json"})
					} else {
						next, err = fx.service.TranscribeSpeech(context.Background(), gateway.STTInput{RequestID: "after-cancel", ClientKey: fx.created.Key, PublicModel: model, FileData: []byte("synthetic")})
					}
					if err != nil {
						t.Fatalf("account capacity after cancel: %v", err)
					}
					_ = next.Body.Close()
				}
			})
		}
	}
}

func TestRESTVoicePhysicalBudgetIncludesPreparationAndRetry(t *testing.T) {
	for _, limit := range []int{1, 2, 3, 4} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if calls.Load() == 1 {
					w.WriteHeader(401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"synthetic","duration":0}`))
			})
			defer upstream.Close()
			fx := newVoiceCompletionFixture(t, upstream.URL, "grok-stt")
			budget := inferencedomain.NewAttemptBudget(limit)
			defer budget.Close()
			ctx := physical.WithPhysicalCallBudget(context.Background(), budget)
			result, err := fx.service.TranscribeSpeech(ctx, gateway.STTInput{RequestID: "budget", ClientKey: fx.created.Key, PublicModel: "grok-stt", FileData: []byte("synthetic")})
			if limit < 4 && !errors.Is(err, inferencedomain.ErrAttemptBudget) {
				t.Fatalf("budget error: %v", err)
			}
			if limit == 4 && err != nil {
				t.Fatal(err)
			}
			if result != nil {
				_ = result.Body.Close()
			}
			record := waitVoiceAudit(t, fx.audits)
			if limit < 4 && record.ErrorCode != "physical_attempt_limit" {
				t.Fatalf("budget outcome: %+v", record)
			}
			if limit == 4 && (record.GenerationOutcome != "completed" || record.UsageSource != audit.UsageSourceUpstream) {
				t.Fatalf("explicit zero duration: %+v", record)
			}
			fx.receipts.mu.Lock()
			defer fx.receipts.mu.Unlock()
			if len(fx.receipts.facts) != limit || budget.Remaining() != 0 || calls.Load() != int32(limit/2) {
				t.Fatalf("limit=%d physical=%d primary=%d remaining=%d", limit, len(fx.receipts.facts), calls.Load(), budget.Remaining())
			}
		})
	}
}

func TestRESTVoiceUnknownAndInvalidResultsIntegration(t *testing.T) {
	for _, tc := range []struct {
		name, model, payload, contentType, generation string
		status                                        int
		localInvalid, reported                        bool
	}{
		{name: "duration_missing", payload: `{"text":""}`, generation: "completed"},
		{name: "duration_zero", payload: `{"text":"","duration":0}`, generation: "completed", reported: true},
		{name: "duration_negative", payload: `{"text":"","duration":-1}`, generation: "completed"},
		{name: "duration_overflow", payload: `{"text":"","duration":1e100}`, generation: "completed", reported: true},
		{name: "malformed_json", payload: `{`, generation: "unconfirmed"},
		{name: "missing_transcript", payload: `{}`, generation: "unconfirmed"},
		{name: "refused", payload: `{"error":"invalid audio"}`, status: 400, generation: "not_started"},
		{name: "local_invalid", generation: "not_started", localInvalid: true},
		{name: "tts_empty", model: "grok-voice-latest", contentType: "audio/mpeg", generation: "unconfirmed"},
		{name: "tts_invalid_audio", model: "grok-voice-latest", payload: `{"audio":"%%%"}`, generation: "unconfirmed"},
		{name: "tts_missing_audio", model: "grok-voice-latest", payload: `{}`, generation: "unconfirmed"},
		{name: "tts_html", model: "grok-voice-latest", payload: `<html>error</html>`, contentType: "text/html", generation: "unconfirmed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				typ := tc.contentType
				if typ == "" {
					typ = "application/json"
				}
				w.Header().Set("Content-Type", typ)
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_, _ = w.Write([]byte(tc.payload))
			})
			defer upstream.Close()
			model := tc.model
			if model == "" {
				model = "grok-stt"
			}
			fx := newVoiceCompletionFixture(t, upstream.URL, model)
			var result *gateway.Result
			var err error
			if tc.model != "" {
				result, err = fx.service.SynthesizeSpeech(context.Background(), gateway.TTSInput{RequestID: "invalid", ClientKey: fx.created.Key, PublicModel: model, Text: "hello", Language: "en"})
			} else {
				data := []byte("synthetic")
				if tc.localInvalid {
					data = nil
				}
				result, err = fx.service.TranscribeSpeech(context.Background(), gateway.STTInput{RequestID: "invalid", ClientKey: fx.created.Key, PublicModel: model, FileData: data})
			}
			if result != nil {
				_ = result.Body.Close()
			}
			if tc.localInvalid {
				var invalid *inferencedomain.RequestValidationError
				if !errors.As(err, &invalid) || calls.Load() != 0 {
					t.Fatalf("local validation err=%v calls=%d", err, calls.Load())
				}
			}
			record := waitVoiceAudit(t, fx.audits)
			wantSource := audit.UsageSourceNone
			if tc.reported {
				wantSource = audit.UsageSourceUpstream
			}
			if record.GenerationOutcome != tc.generation || record.EstimatedCostInUSDTicks != 0 || record.UsageSource != wantSource {
				t.Fatalf("invalid/unknown facts: %+v", record)
			}
			if tc.localInvalid && (record.AccountID != nil || record.UpstreamStatusCode != 0) {
				t.Fatalf("local input blamed upstream: %+v", record)
			}
		})
	}
}

func TestRESTVoicePublicRoutesIntegration(t *testing.T) {
	for _, path := range []string{"/v1/tts", "/v1/audio/speech", "/v1/stt", "/v1/audio/transcriptions"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if r.URL.Path == "/v1/tts" {
					w.Header().Set("Content-Type", "audio/mpeg")
					_, _ = w.Write([]byte("synthetic audio"))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"text":"synthetic","duration":3.45}`))
			})
			defer upstream.Close()
			model, payload := "grok-stt", `{"model":"grok-stt","url":"https://example.invalid/synthetic.wav"}`
			if path == "/v1/tts" {
				model, payload = "grok-voice-latest", `{"model":"grok-voice-latest","text":"hello","language":"en"}`
			}
			if path == "/v1/audio/speech" {
				model, payload = "grok-voice-latest", `{"model":"grok-voice-latest","input":"hello","voice":"alloy"}`
			}
			fx := newVoiceCompletionFixture(t, upstream.URL, model)
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			fx.router.ServeHTTP(recorder, request)
			if recorder.Code != 200 {
				t.Fatalf("public response=%d %s", recorder.Code, recorder.Body.String())
			}
			record := waitVoiceAudit(t, fx.audits)
			if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.DeliveredBytes != int64(recorder.Body.Len()) || record.EstimatedCostInUSDTicks == 0 {
				t.Fatalf("public completion: %+v", record)
			}
		})
	}
}
