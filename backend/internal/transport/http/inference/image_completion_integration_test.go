package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/gin-gonic/gin"
)

const completionImagePNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII="

type imageFaultStore struct{ provider.ImageAssetStore }

func (s imageFaultStore) SaveImage(context.Context, []byte) (mediadomain.Asset, error) {
	return mediadomain.Asset{}, errors.New("injected image storage failure")
}

func TestConsoleImageCompletionIntegration(t *testing.T) {
	for _, edit := range []bool{false, true} {
		for _, format := range []string{"url", "b64_json"} {
			stages := []string{"success", "short_write", "close", "cancel", "partial", "malformed", "receipt", "refused", "invalid_request"}
			if format == "url" {
				stages = append(stages, "download", "storage", "budget_download")
			} else {
				stages = append(stages, "invalid_base64", "memory_budget")
			}
			for _, stage := range stages {
				name := "generate/" + format + "/" + stage
				if edit {
					name = "edit/" + format + "/" + stage
				}
				t.Run(name, func(t *testing.T) {
					var calls, generations atomic.Int32
					upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
						if r.URL.Path == "/generated.png" {
							if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
								t.Error("credentials on image download")
							}
							if stage == "download" {
								w.WriteHeader(404)
								return
							}
							raw, _ := base64.StdEncoding.DecodeString(completionImagePNG)
							w.Header().Set("Content-Type", "image/png")
							_, _ = w.Write(raw)
							return
						}
						generations.Add(1)
						w.Header().Set("Content-Type", "application/json")
						if stage == "refused" {
							w.WriteHeader(400)
							_, _ = w.Write([]byte(`{"error":"invalid prompt"}`))
							return
						}
						if stage == "malformed" {
							_, _ = w.Write([]byte(`{"data":[`))
							return
						}
						items := []map[string]string{}
						count := 2
						if stage == "partial" {
							count = 1
						}
						for i := 0; i < count; i++ {
							if format == "url" {
								items = append(items, map[string]string{"url": "http://" + r.Host + "/generated.png"})
							} else {
								encoded := completionImagePNG
								if stage == "invalid_base64" && i == 1 {
									encoded = "%%%"
								}
								items = append(items, map[string]string{"b64_json": encoded})
							}
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
					})
					defer upstream.Close()
					fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-image", account.ProviderConsole, func(store provider.ImageAssetStore) provider.ImageAssetStore {
						if stage == "storage" {
							return imageFaultStore{store}
						}
						return store
					})
					fx.receipts.fail = stage == "receipt"
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					if stage == "budget_download" {
						budget := inferencedomain.NewAttemptBudget(2)
						defer budget.Close()
						ctx = egress.WithPhysicalCallBudget(ctx, budget)
					}
					if stage == "memory_budget" {
						ctx = responsebuffer.WithContext(ctx, responsebuffer.NewPool(1024).Request(1024))
					}
					quality := ""
					if stage == "invalid_request" {
						quality = "not-supported"
					}
					var result *gateway.Result
					var err error
					if edit {
						result, err = fx.service.EditImage(ctx, gateway.ImageEditInput{RequestID: "image", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", Count: 2, ResponseFormat: format, ImageURLs: []string{"data:image/png;base64," + completionImagePNG}, Quality: quality})
					} else {
						result, err = fx.service.GenerateImage(ctx, gateway.ImageGenerationInput{RequestID: "image", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", Count: 2, ResponseFormat: format, Quality: quality})
					}
					if err != nil && stage != "malformed" && stage != "invalid_request" && stage != "memory_budget" {
						t.Fatal(err)
					}
					if stage == "cancel" {
						cancel()
					} else if result != nil {
						if stage == "close" {
							_ = result.Body.Close()
						} else {
							recorder := httptest.NewRecorder()
							c, _ := gin.CreateTestContext(recorder)
							c.Request = httptest.NewRequest("POST", "/v1/images/generations", nil).WithContext(ctx)
							if stage == "short_write" {
								c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
							}
							(&Handler{}).writeResult(c, result, false, streamProtocolImage)
						}
					}
					record := waitVoiceAudit(t, fx.audits)
					wantGeneration, count := "completed", 2
					switch stage {
					case "partial", "invalid_base64":
						wantGeneration, count = "partial", 1
					case "malformed", "memory_budget":
						wantGeneration, count = "unconfirmed", 0
					case "refused", "invalid_request":
						wantGeneration, count = "not_started", 0
					}
					if record.GenerationOutcome != wantGeneration || record.MediaOutputImages != int64(count) {
						t.Fatalf("generation=%s count=%d want=%s/%d code=%s", record.GenerationOutcome, record.MediaOutputImages, wantGeneration, count, record.ErrorCode)
					}
					pricing, _ := audit.EstimateOfficialImageCost("grok-imagine-image", "", "", count)
					if edit {
						pricing, _ = audit.EstimateOfficialImageEditCost("grok-imagine-image", "", "", count, 1)
					}
					wantCost := pricing.CostInUSDTicks
					if count == 0 {
						wantCost = 0
					}
					key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
					if err != nil || key.BilledUsageUSDTicks != wantCost || record.EstimatedCostInUSDTicks != wantCost || key.ReservedUsageUSDTicks != 0 {
						t.Fatalf("billing=%d reserved=%d audit=%d want=%d err=%v", key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, record.EstimatedCostInUSDTicks, wantCost, err)
					}
					if stage == "short_write" && (record.DeliveredBytes != 3 || record.DeliveryOutcome != "canceled") {
						t.Fatalf("short write: %+v", record)
					}
					if stage == "cancel" && record.DeliveryOutcome != "canceled" {
						t.Fatalf("cancel: %+v", record)
					}
					if stage == "success" && (record.DeliveryOutcome != "completed" || record.DeliveredBytes <= 0) {
						t.Fatalf("delivery: %+v", record)
					}
					if stage == "invalid_request" {
						if generations.Load() != 0 || record.AccountID != nil || record.UpstreamStatusCode != 0 {
							t.Fatalf("local validation: %+v calls=%d", record, generations.Load())
						}
					} else if generations.Load() != 1 {
						t.Fatalf("regenerated after known output: %d", generations.Load())
					}
					fx.receipts.mu.Lock()
					defer fx.receipts.mu.Unlock()
					if stage == "invalid_request" && len(fx.receipts.facts) != 0 {
						t.Fatal("local request reached network")
					}
					if stage == "budget_download" && len(fx.receipts.facts) != 2 {
						t.Fatalf("download escaped budget: %d", len(fx.receipts.facts))
					}
					for i, fact := range fx.receipts.facts {
						if fact.Attempt.Ordinal != uint64(i+1) || fact.Attempt.AccountID != fx.account.ID || fact.Attempt.RequestID != record.EventID || fact.BodyOutcome == "pending" {
							t.Fatalf("physical fact: %+v", fact)
						}
					}
					if format == "url" && stage == "success" {
						assets, _, err := fx.media.AdminListImages(context.Background(), 1, 10, "")
						if err != nil || len(assets) == 0 {
							t.Fatalf("media not archived: %v", err)
						}
						_, body, err := fx.media.OpenImage(context.Background(), assets[0].ID)
						if err != nil {
							t.Fatal(err)
						}
						data, err := io.ReadAll(body)
						_ = body.Close()
						if err != nil || base64.StdEncoding.EncodeToString(data) != completionImagePNG {
							t.Fatalf("media bytes changed: %v", err)
						}
					}
				})
			}
		}
	}
}

func TestConsoleImagePublicRoutesIntegration(t *testing.T) {
	for _, path := range []string{"/v1/images/generations", "/v1/images/edits"} {
		t.Run(path, func(t *testing.T) {
			var calls atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{{"b64_json": completionImagePNG}}})
			})
			defer upstream.Close()
			fx := newVoiceCompletionFixture(t, upstream.URL, "grok-imagine-image")
			payload := map[string]any{"model": fx.publicModel, "prompt": "synthetic", "response_format": "b64_json", "image": map[string]string{"url": "data:image/png;base64," + completionImagePNG}}
			body, _ := json.Marshal(payload)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			fx.router.ServeHTTP(recorder, request)
			if recorder.Code != 200 {
				t.Fatalf("route: %d %s", recorder.Code, recorder.Body.String())
			}
			record := waitVoiceAudit(t, fx.audits)
			if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.MediaOutputImages != 1 {
				t.Fatalf("public image: %+v", record)
			}
		})
	}
}

func TestConsoleImageGenerationSurvivesPostProcessingFailure(t *testing.T) {
	var calls atomic.Int32
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"url":"https://example.invalid/generated.png"}]}`))
	})
	defer upstream.Close()
	fx := newVoiceCompletionFixture(t, upstream.URL, "grok-imagine-image")
	result, _ := fx.service.GenerateImage(context.Background(), gateway.ImageGenerationInput{RequestID: "image-postprocessing", ClientKey: fx.created.Key, PublicModel: "grok-imagine-image", Prompt: "synthetic", Count: 1})
	if result != nil {
		_ = result.Body.Close()
	}
	record := waitVoiceAudit(t, fx.audits)
	if record.GenerationOutcome != "completed" || record.MediaOutputImages != 1 || record.EstimatedCostInUSDTicks == 0 {
		t.Fatalf("generated image lost after storage failure: generation=%q images=%d cost=%d", record.GenerationOutcome, record.MediaOutputImages, record.EstimatedCostInUSDTicks)
	}
}

func TestImageEditCompletionRecognized(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/images/edits", nil)
	committed, code := false, ""
	result := &gateway.Result{StatusCode: 200, Body: io.NopCloser(strings.NewReader("event: image_edit.completed\ndata: {\"type\":\"image_edit.completed\",\"b64_json\":\"c3ludGhldGlj\"}\n\n")),
		CommitCompletion: func(gateway.Completion) error { committed = true; return nil },
		Finalize:         func(_ gateway.Usage, _, errorCode string) { code = errorCode },
	}
	(&Handler{}).writeResult(c, result, true, streamProtocolImage)
	if !committed || code != "" {
		t.Fatalf("image edit terminal rejected: committed=%v code=%q output=%s", committed, code, recorder.Body.String())
	}
}

// The response is close to the public 128 MiB transfer ceiling. Streaming the
// synthetic base64 from the server avoids a second test-owned payload array.
func TestConsoleLargeImageResponseBudget(t *testing.T) {
	if testing.Short() {
		t.Skip("large image response memory boundary")
	}
	const prefix = `{"data":[{"b64_json":"`
	const suffix = `"}]}`
	payloadBytes := (maxJSONResponseTransferBytes - len(prefix) - len(suffix)) / 4 * 4
	var calls atomic.Int32
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, _ *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, prefix)
		chunk := strings.Repeat("A", 32<<10)
		for remaining := payloadBytes; remaining > 0; {
			n := min(remaining, len(chunk))
			if _, err := io.WriteString(w, chunk[:n]); err != nil {
				return
			}
			remaining -= n
		}
		_, _ = io.WriteString(w, suffix)
	})
	defer upstream.Close()
	fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-image", account.ProviderConsole, nil)
	before := responsebuffer.ProcessSnapshot()
	result, err := fx.service.GenerateImage(context.Background(), gateway.ImageGenerationInput{RequestID: "large-image", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", Count: 1, ResponseFormat: "b64_json"})
	if err != nil {
		t.Fatal(err)
	}
	budget := responsebuffer.BudgetOf(result.Body)
	if budget == nil || budget.Snapshot().Used == 0 {
		t.Fatal("missing retained body budget")
	}
	recorder := httptest.NewRecorder()
	c, _ := ginImageContext(recorder, context.Background())
	// A transport failure must still release the borrowed allocation and settle
	// the known generation. It also keeps the test's output storage constant.
	c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
	(&Handler{}).writeResult(c, result, false, streamProtocolImage)
	record := waitVoiceAudit(t, fx.audits)
	after := budget.Snapshot()
	if record.MediaOutputImages != 1 || record.GenerationOutcome != "completed" || record.DeliveryOutcome != "canceled" || record.DeliveredBytes != 3 {
		t.Fatalf("generation/delivery=%+v", record)
	}
	if after.Limit != 256<<20 || after.Peak > after.Limit || after.Used != 0 || after.Rejected != 0 {
		t.Fatalf("budget=%+v", after)
	}
	if responsebuffer.ProcessSnapshot().Used != before.Used {
		t.Fatal("process image reservation leaked")
	}
	t.Logf("payload=%d request_budget=%+v", payloadBytes+len(prefix)+len(suffix), after)
}

func BenchmarkImageJSONCompletion(b *testing.B) {
	for _, size := range []int{64 << 10, 1 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			payload := []byte(`{"data":[{"b64_json":"` + strings.Repeat("A", size) + `"}]}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			for i := 0; i < b.N; i++ {
				budget := responsebuffer.NewPool(512 << 20).Request(256 << 20)
				buffer := responsebuffer.New(budget, len(payload))
				_, _ = buffer.Write(payload)
				body := buffer.Body()
				writer := &imageDiscardWriter{header: make(http.Header)}
				_, err := copyJSONWithCompletion(writer, body, streamProtocolImage, func(responseMetadata) error { return nil })
				_ = body.Close()
				if err != nil {
					b.Fatal(err)
				}
				if budget.Snapshot().Used != 0 {
					b.Fatal("response budget leaked")
				}
			}
		})
	}
}

type imageDiscardWriter struct {
	gin.ResponseWriter
	header http.Header
}

func (w *imageDiscardWriter) Header() http.Header         { return w.header }
func (w *imageDiscardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (w *imageDiscardWriter) WriteHeader(int)             {}
func (w *imageDiscardWriter) WriteHeaderNow()             {}
