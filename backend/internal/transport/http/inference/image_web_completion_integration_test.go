package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	fhttptest "github.com/bogdanfinn/fhttp/httptest"
	"github.com/bogdanfinn/websocket"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/gin-gonic/gin"
)

func TestWebImagineCompletionIntegration(t *testing.T) {
	for _, stream := range []bool{false, true} {
		stages := []string{"success", "storage", "short_write", "receipt", "failed", "download", "refused"}
		if stream {
			stages = append(stages, "cancel_before_final", "cancel_after_final", "close_before_read")
		} else {
			stages = append(stages, "partial", "close", "cancel")
		}
		for _, stage := range stages {
			t.Run(fmt.Sprintf("stream=%t/%s", stream, stage), func(t *testing.T) {
				var generations atomic.Int32
				count := 1
				if !stream {
					count = 2
				}
				upstream := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
					if r.URL.Path != "/ws/imagine/listen" {
						w.WriteHeader(404)
						return
					}
					if stage == "refused" {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(403)
						_, _ = w.Write([]byte(`{"error":{"code":"content-moderated"}}`))
						return
					}
					conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
					for range 2 {
						var request any
						if err := conn.ReadJSON(&request); err != nil {
							return
						}
					}
					generations.Add(1)
					if stage == "failed" {
						_ = conn.WriteJSON(map[string]any{"type": "error"})
						return
					}
					if stage == "cancel_before_final" || stage == "close_before_read" {
						_ = conn.WriteJSON(map[string]any{"type": "image", "id": "preview", "blob": completionImagePNG, "percentage_complete": 50})
						_, _, _ = conn.ReadMessage()
						return
					}
					for i := 0; i < count; i++ {
						id := fmt.Sprint(i)
						if stage == "partial" && i == 1 {
							_ = conn.WriteJSON(map[string]any{"type": "json", "id": id, "current_status": "completed", "moderated": true})
							continue
						}
						image := map[string]any{"type": "image", "id": id, "blob": completionImagePNG, "percentage_complete": 100}
						if stage == "download" {
							delete(image, "blob")
							image["url"] = "https://example.invalid/generated.png"
						}
						_ = conn.WriteJSON(image)
						_ = conn.WriteJSON(map[string]any{"type": "json", "id": id, "current_status": "completed", "moderated": false})
					}
				}))
				defer upstream.Close()
				fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-image-quality", account.ProviderWeb, func(store provider.ImageAssetStore) provider.ImageAssetStore {
					if stage == "storage" {
						return imageFaultStore{store}
					}
					return store
				})
				fx.receipts.fail = stage == "receipt"
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				retained := responsebuffer.NewPool(512 << 20).Request(256 << 20)
				ctx = responsebuffer.WithContext(ctx, retained)
				result, err := fx.service.GenerateImage(ctx, gateway.ImageGenerationInput{RequestID: "web-image", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", Count: count, Streaming: stream, ResponseFormat: "b64_json", PartialImages: 0})
				if err != nil && stage != "failed" {
					t.Fatal(err)
				}
				if result != nil {
					if stream && result.StatusCode == http.StatusOK && responsebuffer.BudgetOf(result.Body) != retained {
						t.Fatal("stream lost the logical request memory budget")
					}
					if stage == "close" || stage == "close_before_read" {
						_ = result.Body.Close()
					} else if stage == "cancel" || stage == "cancel_before_final" {
						cancel()
					} else {
						if stage == "cancel_after_final" {
							// The producer confirms a final frame before writing the SSE
							// terminal event. Wait for its durable image as that barrier.
							deadline := time.Now().Add(5 * time.Second)
							for {
								images, _, err := fx.media.AdminListImages(context.Background(), 1, 10, "")
								if err != nil {
									t.Fatal(err)
								}
								if len(images) > 0 {
									break
								}
								if time.Now().After(deadline) {
									t.Fatal("producer did not archive final image")
								}
								time.Sleep(time.Millisecond)
							}
							cancel()
						} else {
							recorder := httptest.NewRecorder()
							c, _ := ginImageContext(recorder, ctx)
							if stage == "short_write" {
								c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
							}
							(&Handler{}).writeResult(c, result, stream, streamProtocolImage)
							if stage == "success" && recorder.Code != 200 {
								t.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
							}
						}
					}
				}
				record := waitVoiceAudit(t, fx.audits)
				if retained.Snapshot().Used != 0 {
					t.Fatal("stream memory reservation leaked")
				}
				wantCount, generation := count, "completed"
				switch stage {
				case "partial":
					wantCount, generation = 1, "partial"
				case "failed":
					wantCount, generation = 0, "failed"
				case "cancel_before_final", "close_before_read":
					wantCount, generation = 0, "unconfirmed"
				case "refused":
					wantCount, generation = 0, "not_started"
				}
				if record.MediaOutputImages != int64(wantCount) || record.GenerationOutcome != generation {
					t.Fatalf("image facts: count=%d generation=%s want=%d/%s error=%s", record.MediaOutputImages, record.GenerationOutcome, wantCount, generation, record.ErrorCode)
				}
				price, _ := audit.EstimateOfficialImageCost("grok-imagine-image", "", "", wantCount)
				if wantCount == 0 {
					price.CostInUSDTicks = 0
				}
				key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
				if err != nil || key.BilledUsageUSDTicks != price.CostInUSDTicks || key.ReservedUsageUSDTicks != 0 || record.EstimatedCostInUSDTicks != price.CostInUSDTicks {
					t.Fatalf("billing=%d/%d reserved=%d err=%v", key.BilledUsageUSDTicks, price.CostInUSDTicks, key.ReservedUsageUSDTicks, err)
				}
				if stage == "short_write" && (record.DeliveredBytes != 3 || record.DeliveryOutcome != "canceled") {
					t.Fatalf("short write: %+v", record)
				}
				if stage == "success" && (record.DeliveryOutcome != "completed" || record.DeliveredBytes == 0) {
					t.Fatalf("success: %+v", record)
				}
				fx.receipts.mu.Lock()
				defer fx.receipts.mu.Unlock()
				if len(fx.receipts.facts) != 1 {
					t.Fatalf("physical calls=%d", len(fx.receipts.facts))
				}
				fact := fx.receipts.facts[0]
				if fact.Attempt.AccountID != fx.account.ID || fact.Attempt.RequestID != record.EventID || fact.BodyOutcome == "pending" {
					t.Fatalf("unfinished physical fact: %+v", fact)
				}
				if stage != "refused" && generations.Load() != 1 {
					t.Fatalf("generation calls=%d", generations.Load())
				}
			})
		}
	}
}

// Kept local to media integration tests so the handler owns every real result.
func ginImageContext(recorder *httptest.ResponseRecorder, ctx context.Context) (*gin.Context, *gin.Engine) {
	c, engine := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil).WithContext(ctx)
	return c, engine
}

func TestWebImageURLCompletionIntegration(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	for _, stream := range []bool{false, true} {
		for _, stage := range []string{"success", "download", "storage", "short_write", "budget_upload", "budget_download", "cancel_after_final", "preview"} {
			if stage == "preview" && !stream {
				continue
			}
			t.Run(fmt.Sprintf("edit/stream=%t/%s", stream, stage), func(t *testing.T) {
				var uploads, generations, downloads atomic.Int32
				upstream := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
					switch r.URL.Path {
					case "/http/upload-file-v2/direct":
						uploads.Add(1)
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write([]byte(`{"uploadId":"upload-1","fileMetadata":{"fileMetadataId":"file-1","fileUri":"users/test/reference/content"}}`))
					case "/rest/app-chat/conversations/new":
						generations.Add(1)
						w.Header().Set("Content-Type", "application/json")
						if stage == "preview" {
							_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"response": map[string]any{"streamingImageGenerationResponse": map[string]any{"imageUrl": "users/test/generated/id-part-0/image.png", "progress": 50}}}})
							w.(fhttp.Flusher).Flush()
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"response": map[string]any{"streamingImageGenerationResponse": map[string]any{"imageUrl": "users/test/generated/id/image.png", "progress": 100}}}})
					default:
						w.WriteHeader(404)
					}
				}))
				defer upstream.Close()
				proxy := imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, r *http.Request) {
					downloads.Add(1)
					if r.Host != "assets.grok.com" {
						t.Errorf("asset host=%s", r.Host)
					}
					if stage == "download" {
						w.WriteHeader(404)
						return
					}
					w.Header().Set("Content-Type", "image/png")
					raw, _ := base64.StdEncoding.DecodeString(completionImagePNG)
					_, _ = w.Write(raw)
				})
				fx := newMediaCompletionFixture(t, upstream.URL, "imagine-image-edit", account.ProviderWeb, func(store provider.ImageAssetStore) provider.ImageAssetStore {
					if stage == "storage" {
						return imageFaultStore{store}
					}
					return store
				})
				fx.useProxy(proxy)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if strings.HasPrefix(stage, "budget_") {
					limit := 1
					if stage == "budget_download" {
						limit = 2
					}
					budget := inferencedomain.NewAttemptBudget(limit)
					defer budget.Close()
					ctx = physical.WithPhysicalCallBudget(ctx, budget)
				}
				partials := 0
				if stage == "preview" {
					partials = 1
				}
				result, err := fx.service.EditImage(ctx, gateway.ImageEditInput{RequestID: "web-edit", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", ImageURLs: []string{"data:image/png;base64," + completionImagePNG}, Count: 1, ResponseFormat: "url", Streaming: stream, PartialImages: partials})
				if err != nil && stage != "budget_upload" {
					t.Fatal(err)
				}
				var output string
				if result != nil {
					if stage == "cancel_after_final" {
						if stream {
							deadline := time.Now().Add(5 * time.Second)
							for {
								images, _, err := fx.media.AdminListImages(context.Background(), 1, 10, "")
								if err != nil {
									t.Fatal(err)
								}
								if len(images) > 0 {
									break
								}
								if time.Now().After(deadline) {
									t.Fatal("edit producer did not archive")
								}
								time.Sleep(time.Millisecond)
							}
						}
						cancel()
					} else {
						recorder := httptest.NewRecorder()
						c, _ := ginImageContext(recorder, ctx)
						if stage == "short_write" {
							c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
						}
						(&Handler{}).writeResult(c, result, stream, streamProtocolImage)
						output = recorder.Body.String()
					}
				}
				record := waitVoiceAudit(t, fx.audits)
				wantCount := int64(1)
				if stage == "budget_upload" {
					wantCount = 0
				}
				if record.MediaOutputImages != wantCount || wantCount > 0 && record.GenerationOutcome != "completed" {
					t.Fatalf("edit generation=%s count=%d code=%s output=%s", record.GenerationOutcome, record.MediaOutputImages, record.ErrorCode, output)
				}
				price, _ := audit.EstimateOfficialImageEditCost("grok-imagine-image-edit", "", "", int(wantCount), 1)
				if wantCount == 0 {
					price.CostInUSDTicks = 0
				}
				key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
				if err != nil || key.BilledUsageUSDTicks != price.CostInUSDTicks || key.ReservedUsageUSDTicks != 0 {
					t.Fatalf("edit billing=%d/%d reserved=%d err=%v", key.BilledUsageUSDTicks, price.CostInUSDTicks, key.ReservedUsageUSDTicks, err)
				}
				if stage == "success" || stage == "preview" {
					if record.DeliveryOutcome != "completed" || stream && !strings.Contains(output, "image_edit.completed") {
						t.Fatalf("edit delivery=%s code=%s output=%s", record.DeliveryOutcome, record.ErrorCode, output)
					}
					if stage == "preview" && (!strings.Contains(output, "image_edit.partial_image") || record.MediaOutputImages != 1) {
						t.Fatalf("preview output=%s", output)
					}
				}
				if stage == "short_write" && (record.DeliveredBytes != 3 || record.DeliveryOutcome != "canceled") {
					t.Fatalf("edit short write=%+v", record)
				}
				if uploads.Load() != 1 || generations.Load() > 1 {
					t.Fatalf("edit repeated=%d/%d", uploads.Load(), generations.Load())
				}
				fx.receipts.mu.Lock()
				defer fx.receipts.mu.Unlock()
				wantCalls := 3
				if stage == "budget_upload" {
					wantCalls = 1
				}
				if stage == "budget_download" {
					wantCalls = 2
				}
				if stage == "preview" {
					wantCalls = 4
				}
				if len(fx.receipts.facts) != wantCalls {
					t.Fatalf("edit physical calls=%d want=%d downloads=%d", len(fx.receipts.facts), wantCalls, downloads.Load())
				}
				for i, fact := range fx.receipts.facts {
					if fact.Attempt.AccountID != fx.account.ID || fact.Attempt.RequestID != record.EventID || fact.Attempt.Ordinal != uint64(i+1) || fact.BodyOutcome == "pending" {
						t.Fatalf("edit physical identity=%+v", fact)
					}
				}
			})
		}
	}
}

func TestWebLiteImageCompletionIntegration(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	for _, stage := range []string{"success", "partial", "download", "storage", "short_write", "cancel", "receipt", "budget_generation", "budget_download", "refused"} {
		t.Run(stage, func(t *testing.T) {
			var generations, downloads, handshakes atomic.Int32
			upstream := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
				if r.URL.Path != "/ws/mgw/" {
					w.WriteHeader(404)
					return
				}
				handshakes.Add(1)
				if stage == "refused" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(403)
					_, _ = w.Write([]byte(`{"error":{"code":"content-moderated"}}`))
					return
				}
				conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				var initial map[string]any
				if err := conn.ReadJSON(&initial); err != nil {
					t.Error(err)
					return
				}
				id := initial["event"].(map[string]any)["event_id"]
				_ = conn.WriteJSON(map[string]any{"session_id": "lite", "event": map[string]any{"type": "session.created", "client_event_id": id}})
				_ = conn.WriteJSON(map[string]any{"session_id": "lite", "event": map[string]any{"type": "conversation.attached", "conversation": map[string]any{"id": "lite"}}})
				for range 2 {
					var input any
					if conn.ReadJSON(&input) != nil {
						return
					}
				}
				n := generations.Add(1)
				if stage == "partial" && n == 2 {
					_ = conn.WriteJSON(map[string]any{"session_id": "lite", "event": map[string]any{"type": "response.done", "response": map[string]any{"id": "empty", "status": "completed"}}})
					return
				}
				_ = conn.WriteJSON(map[string]any{"session_id": "lite", "event": map[string]any{"type": "response.grok.output", "output": map[string]any{"card_attachment": map[string]any{"jsonData": map[string]any{"id": fmt.Sprint(n), "image_chunk": map[string]any{"progress": 100, "imageUrl": fmt.Sprintf("users/test/generated/lite-%d/image.png", n), "moderated": false}}}}}})
			}))
			defer upstream.Close()
			proxy := imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, _ *http.Request) {
				downloads.Add(1)
				if stage == "download" {
					w.WriteHeader(404)
					return
				}
				w.Header().Set("Content-Type", "image/png")
				raw, _ := base64.StdEncoding.DecodeString(completionImagePNG)
				_, _ = w.Write(raw)
			})
			fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-image", account.ProviderWeb, func(store provider.ImageAssetStore) provider.ImageAssetStore {
				if stage == "storage" {
					return imageFaultStore{store}
				}
				return store
			})
			fx.useProxy(proxy)
			fx.service.UpdateMaxAttempts(3)
			if err := saveQuotaWindowsFixture(fx.accounts, context.Background(), fx.account.ID, account.WebTierBasic, time.Now(), []account.QuotaWindow{{Mode: "fast", Remaining: 20, Total: 20}}); err != nil {
				t.Fatal(err)
			}
			fx.receipts.fail = stage == "receipt"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if strings.HasPrefix(stage, "budget_") {
				limit := 1
				if stage == "budget_download" {
					limit = 2
				}
				budget := inferencedomain.NewAttemptBudget(limit)
				defer budget.Close()
				ctx = physical.WithPhysicalCallBudget(ctx, budget)
			}
			result, err := fx.service.GenerateImage(ctx, gateway.ImageGenerationInput{RequestID: "web-lite", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", Count: 2, ResponseFormat: "url"})
			if err != nil {
				t.Fatal(err)
			}
			if stage == "cancel" {
				cancel()
			} else {
				recorder := httptest.NewRecorder()
				c, _ := ginImageContext(recorder, ctx)
				if stage == "short_write" {
					c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
				}
				(&Handler{}).writeResult(c, result, false, streamProtocolImage)
				if stage == "success" && recorder.Code != 200 {
					t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
				}
			}
			record := waitVoiceAudit(t, fx.audits)
			count, outcome, calls := 2, "completed", 4
			switch stage {
			case "partial":
				count, outcome, calls = 1, "partial", 2
			case "budget_generation":
				count, outcome, calls = 1, "partial", 1
			case "budget_download":
				calls = 2
			case "download", "storage":
				calls = 3
			case "refused":
				count, outcome, calls = 0, "not_started", 1
			}
			if record.MediaOutputImages != int64(count) || record.GenerationOutcome != outcome {
				t.Fatalf("generation=%s/%d want=%s/%d error=%s", record.GenerationOutcome, record.MediaOutputImages, outcome, count, record.ErrorCode)
			}
			price, _ := audit.EstimateOfficialImageCost("grok-imagine-image", "", "", count)
			if count == 0 {
				price.CostInUSDTicks = 0
			}
			key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
			if err != nil || key.BilledUsageUSDTicks != price.CostInUSDTicks || key.ReservedUsageUSDTicks != 0 {
				t.Fatalf("billing=%d/%d reserved=%d err=%v", key.BilledUsageUSDTicks, price.CostInUSDTicks, key.ReservedUsageUSDTicks, err)
			}
			windows, err := fx.accounts.GetQuotaWindows(context.Background(), []uint64{fx.account.ID})
			if err != nil || len(windows[fx.account.ID]) != 1 || windows[fx.account.ID][0].Remaining != 20-count {
				t.Fatalf("quota=%+v count=%d err=%v", windows, count, err)
			}
			fx.receipts.mu.Lock()

			if len(fx.receipts.facts) != calls || int(handshakes.Load()+downloads.Load()) != calls {
				t.Fatalf("physical=%d handshake=%d downloads=%d want=%d", len(fx.receipts.facts), handshakes.Load(), downloads.Load(), calls)
			}
			for _, fact := range fx.receipts.facts {
				if fact.Attempt.AccountID != fx.account.ID || fact.Attempt.RequestID != record.EventID || fact.BodyOutcome == "pending" {
					t.Fatalf("physical=%+v", fact)
				}
			}
			fx.receipts.mu.Unlock()
			if stage == "cancel" {
				next, err := fx.service.GenerateImage(context.Background(), gateway.ImageGenerationInput{RequestID: "after-cancel", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Prompt: "synthetic", Count: 1, ResponseFormat: "url"})
				if err != nil {
					t.Fatalf("account capacity after cancel: %v", err)
				}
				_ = next.Body.Close()
			}
		})
	}
}
