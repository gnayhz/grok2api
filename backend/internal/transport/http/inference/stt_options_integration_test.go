package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

func TestSTTOptionsAcrossHTTPEncodings(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, path := range []string{"/v1/stt", "/v1/audio/transcriptions"} {
			for _, encoding := range []string{"json", "multipart"} {
				t.Run(dialect+path+"/"+encoding, func(t *testing.T) {
					var calls atomic.Int32
					type expectedRequest struct {
						fields map[string][]string
						file   []byte
					}
					expected := make(chan expectedRequest, 1)
					upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
						var want expectedRequest
						select {
						case want = <-expected:
						default:
							t.Error("unexpected upstream request")
							w.WriteHeader(500)
							return
						}
						if err := r.ParseMultipartForm(1 << 20); err != nil {
							t.Error(err)
							return
						}
						defer r.MultipartForm.RemoveAll()
						for key, values := range want.fields {
							if !reflect.DeepEqual(r.MultipartForm.Value[key], values) {
								t.Errorf("wire %s=%q want=%q", key, r.MultipartForm.Value[key], values)
							}
						}
						if want.file != nil {
							f, _, err := r.FormFile("file")
							if err != nil {
								t.Error(err)
								return
							}
							data, err := io.ReadAll(f)
							_ = f.Close()
							if err != nil || !bytes.Equal(data, want.file) {
								t.Errorf("file input changed: %q %v", data, err)
							}
						}
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"text":"synthetic","duration":1}`))
					})
					defer upstream.Close()
					fx := newProviderCompletionFixtureOnDatabase(t, upstream.URL, "grok-stt", account.ProviderConsole, nil, nil, nil, nil, compactionDatabase(t, dialect), "")
					send := func(values map[string]any, repeated map[string][]string, file []byte) *httptest.ResponseRecorder {
						var body bytes.Buffer
						contentType := "application/json"
						if encoding == "json" {
							if err := json.NewEncoder(&body).Encode(values); err != nil {
								t.Fatal(err)
							}
						} else {
							writer := multipart.NewWriter(&body)
							for key, value := range values {
								if list, ok := value.([]string); ok {
									for _, item := range list {
										if err := writer.WriteField(key, item); err != nil {
											t.Fatal(err)
										}
									}
									continue
								}
								var text string
								if value != nil {
									if s, ok := value.(string); ok {
										text = s
									} else {
										raw, err := json.Marshal(value)
										if err != nil {
											t.Fatal(err)
										}
										text = string(raw)
									}
								}
								if err := writer.WriteField(key, text); err != nil {
									t.Fatal(err)
								}
							}
							for key, list := range repeated {
								for _, value := range list {
									if err := writer.WriteField(key, value); err != nil {
										t.Fatal(err)
									}
								}
							}
							if file != nil {
								part, err := writer.CreateFormFile("file", "synthetic.wav")
								if err != nil {
									t.Fatal(err)
								}
								if _, err = part.Write(file); err != nil {
									t.Fatal(err)
								}
							}
							if err := writer.Close(); err != nil {
								t.Fatal(err)
							}
							contentType = writer.FormDataContentType()
						}
						r := httptest.NewRequest(http.MethodPost, path, &body)
						r.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						r.Header.Set("Content-Type", contentType)
						w := httptest.NewRecorder()
						fx.router.ServeHTTP(w, r)
						return w
					}
					for _, tc := range []struct {
						name, param string
						value       any
					}{
						{"fractional_rate", "sample_rate", json.Number("16000.9")},
						{"fractional_channels", "channels", json.Number("2.9")},
						{"numeric_overflow", "sample_rate", json.Number("9223372036854775808")},
						{"exponent_overflow", "sample_rate", json.Number("1e1000000")},
						{"precise_fraction", "channels", json.Number("2.0000000000000001")},
						{"channels_overflow", "channels", "9223372036854775808"},
						{"channels_text", "channels", "two"},
						{"channels_zero", "channels", 0},
						{"channels_negative", "channels", -2},
						{"boolean_typo", "diarize", "flase"},
						{"boolean_object", "format", map[string]bool{"value": true}},
						{"boolean_number", "multichannel", 2},
						{"boolean_array", "filler_words", []bool{true}},
						{"threshold_nan", "vad_threshold", "NaN"},
						{"threshold_inf", "vad_threshold", "Inf"},
						{"threshold_text", "vad_threshold", "quiet"},
					} {
						t.Run(tc.name, func(t *testing.T) {
							w := send(map[string]any{"url": "https://audio.example/synthetic.wav", tc.param: tc.value}, nil, nil)
							var payload struct {
								Error struct {
									Param string `json:"param"`
								} `json:"error"`
							}
							if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
								t.Fatal(err)
							}
							if w.Code != 400 || payload.Error.Param != tc.param || calls.Load() != 0 {
								t.Fatalf("invalid input crossed boundary: status=%d param=%q calls=%d", w.Code, payload.Error.Param, calls.Load())
							}
						})
					}
					if encoding == "multipart" {
						for _, repeated := range []map[string][]string{{"sample_rate": {"16000", "48000"}}, {"channels": {"2", "2"}}, {"diarize": {"true", "false"}}, {"sample_rate_hertz": {"48000"}, "sample_rate": {"16000"}}} {
							w := send(map[string]any{"url": "https://audio.example/synthetic.wav"}, repeated, nil)
							if w.Code != 400 || calls.Load() != 0 {
								t.Fatalf("ambiguous multipart accepted: status=%d calls=%d", w.Code, calls.Load())
							}
						}
					}
					key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
					if err != nil || key.BilledUsageUSDTicks != 0 || key.ReservedUsageUSDTicks != 0 {
						t.Fatalf("invalid input billed/reserved: %+v %v", key, err)
					}
					_, count, err := fx.audits.List(context.Background(), 0, 100)
					if err != nil || count != 0 {
						t.Fatalf("invalid input created upstream audit: count=%d err=%v", count, err)
					}
					for index, values := range []map[string]any{
						{"sample_rate": json.Number("1.6e4"), "channels": json.Number("2.0"), "format": "YES", "multichannel": true, "diarize": 1, "filler_words": "on", "vad_threshold": 0.25, "language": "en", "audio_format": "wav", "keyterm": []string{"hello", "世界"}},
						{"sample_rate": json.Number("9007199254740993"), "channels": "2", "format": "off", "multichannel": false, "diarize": "NO", "filler_words": 0, "vad_threshold": 0},
						{"sample_rate": nil, "channels": nil, "format": nil, "multichannel": nil, "diarize": nil, "filler_words": nil, "vad_threshold": nil},
					} {
						if encoding == "json" && index == 0 {
							values["diarize"] = json.Number("1.0")
							values["filler_words"] = json.Number("1e0")
						}
						values["url"] = "https://audio.example/synthetic.wav"
						wantFields := map[string][]string{"url": {"https://audio.example/synthetic.wav"}, "model": {"grok-stt"}}
						switch index {
						case 0:
							for key, value := range map[string]string{"sample_rate": "16000", "channels": "2", "format": "true", "multichannel": "true", "diarize": "true", "filler_words": "true", "vad_threshold": "0.25", "language": "en", "audio_format": "wav"} {
								wantFields[key] = []string{value}
							}
							wantFields["keyterm"] = []string{"hello", "世界"}
						case 1:
							wantFields["sample_rate"] = []string{"9007199254740993"}
							wantFields["channels"] = []string{"2"}
							wantFields["vad_threshold"] = []string{"0"}
							for _, key := range []string{"format", "multichannel", "diarize", "filler_words"} {
								wantFields[key] = nil
							}
						case 2:
							for _, key := range []string{"sample_rate", "channels", "format", "multichannel", "diarize", "filler_words", "vad_threshold"} {
								wantFields[key] = nil
							}
						}
						expected <- expectedRequest{fields: wantFields}
						w := send(values, nil, nil)
						if w.Code != 200 || calls.Load() != int32(index+1) {
							t.Fatalf("valid input failed: status=%d calls=%d body=%s", w.Code, calls.Load(), w.Body.String())
						}
					}
					if encoding == "multipart" {
						wantFile := []byte("synthetic file bytes")
						expected <- expectedRequest{fields: map[string][]string{"sample_rate": {"16000"}, "url": nil}, file: wantFile}
						w := send(map[string]any{"sample_rate_hertz": "16000"}, nil, wantFile)
						if w.Code != 200 || calls.Load() != 4 {
							t.Fatalf("file/alias failed: %d %s", w.Code, w.Body.String())
						}
					}
					price, _ := audit.EstimateOfficialSTTCost(1, false)
					key, err = fx.clients.Get(context.Background(), fx.created.Key.ID)
					if err != nil || key.BilledUsageUSDTicks != int64(calls.Load())*price.CostInUSDTicks {
						t.Fatalf("accepted inputs lost billing: billed=%d calls=%d err=%v", key.BilledUsageUSDTicks, calls.Load(), err)
					}
				})
			}
		}
	}
}
