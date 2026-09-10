package inference

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
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
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/gin-gonic/gin"
)

func TestWebImageChatCompletionIntegration(t *testing.T) { testWebImageChatCompletion(t, false) }
func TestWebLiteImageChatCompletionIntegration(t *testing.T) {
	if imageAssetTLSChild(t) {
		testWebImageChatCompletion(t, true)
	}
}
func testWebImageChatCompletion(t *testing.T, lite bool) {
	for _, operation := range []audit.Operation{audit.OperationResponses, audit.OperationChat, audit.OperationMessages} {
		for _, stream := range []bool{false, true} {
			stages := []string{"success", "close", "cancel", "short_write", "storage", "partial", "receipt", "budget", "invalid"}
			if operation == audit.OperationResponses {
				stages = append(stages, "store")
			}
			if lite {
				stages = append(stages, "download")
			}
			for _, stage := range stages {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", operation, stream, stage), func(t *testing.T) {
					var generations atomic.Int32
					upstream := fhttptest.NewServer(fhttp.HandlerFunc(func(w fhttp.ResponseWriter, r *fhttp.Request) {
						conn, err := (&websocket.Upgrader{CheckOrigin: func(*fhttp.Request) bool { return true }}).Upgrade(w, r, nil)
						if err != nil {
							t.Error(err)
							return
						}
						defer conn.Close()
						_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
						if lite {
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
							return
						}
						for range 2 {
							var request any
							if err := conn.ReadJSON(&request); err != nil {
								return
							}
						}
						generations.Add(1)
						for i := 0; i < 2; i++ {
							id := fmt.Sprint(i)
							if stage == "partial" && i == 1 {
								_ = conn.WriteJSON(map[string]any{"type": "json", "id": id, "current_status": "completed", "moderated": true})
								continue
							}
							_ = conn.WriteJSON(map[string]any{"type": "image", "id": id, "blob": completionImagePNG, "percentage_complete": 100})
							_ = conn.WriteJSON(map[string]any{"type": "json", "id": id, "current_status": "completed", "moderated": false})
						}
					}))
					defer upstream.Close()
					model, quotaMode := "grok-imagine-image-quality", "image_pro"
					if lite {
						model, quotaMode = "grok-imagine-image", "fast"
					}
					fx := newMediaCompletionFixture(t, upstream.URL, model, account.ProviderWeb, func(store provider.ImageAssetStore) provider.ImageAssetStore {
						if stage == "storage" {
							return imageFaultStore{store}
						}
						return store
					})
					if lite {
						proxy := imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, _ *http.Request) {
							if stage == "download" {
								w.WriteHeader(404)
								return
							}
							raw, _ := base64.StdEncoding.DecodeString(completionImagePNG)
							w.Header().Set("Content-Type", "image/png")
							_, _ = w.Write(raw)
						})
						fx.useProxy(proxy)
					}
					if err := saveQuotaWindowsFixture(fx.accounts, context.Background(), fx.account.ID, account.WebTierBasic, time.Now(), []account.QuotaWindow{{Mode: quotaMode, Remaining: 20, Total: 20}}); err != nil {
						t.Fatal(err)
					}
					fx.receipts.fail = stage == "receipt"
					if stage == "budget" {
						fx.created.Key.BillingLimitUSDTicks = 1
						if _, err := fx.clients.Patch(context.Background(), fx.created.Key.ID, clientkey.ManagementPatch{BillingLimitUSDTicks: &fx.created.Key.BillingLimitUSDTicks}); err != nil {
							t.Fatal(err)
						}
					}
					fx.service.UpdateMaxAttempts(3)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					retained := responsebuffer.NewPool(512 << 20).Request(256 << 20)
					ctx = responsebuffer.WithContext(ctx, retained)
					payload := map[string]any{"model": fx.publicModel, "stream": stream, "max_tokens": 100, "image_config": map[string]any{"n": 2, "response_format": "b64_json"}, "messages": []map[string]string{{"role": "user", "content": "synthetic image"}}}
					if operation == audit.OperationResponses {
						delete(payload, "messages")
						payload["input"] = "synthetic image"
					}
					if stage == "store" {
						payload["store"] = true
					}
					if stage == "invalid" {
						payload["image_config"].(map[string]any)["n"] = 0
					}
					body, _ := json.Marshal(payload)
					input := gateway.Input{RequestID: "image-chat", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Body: body, Streaming: stream, Operation: operation}
					var result *gateway.Result
					var err error
					path, protocol := "/v1/responses", streamProtocolResponses
					switch operation {
					case audit.OperationResponses:
						result, err = fx.service.CreateResponse(ctx, input)
					case audit.OperationChat:
						path, protocol = "/v1/chat/completions", streamProtocolChat
						result, err = fx.service.CreateChatCompletion(ctx, input)
					case audit.OperationMessages:
						path, protocol = "/v1/messages", streamProtocolAnthropic
						result, err = fx.service.CreateMessage(ctx, input)
					}
					if stage == "store" || stage == "invalid" {
						if result != nil {
							_ = result.Body.Close()
						}
						var invalid *inferencedomain.RequestValidationError
						if !errors.As(err, &invalid) || generations.Load() != 0 {
							t.Fatalf("validation=%v calls=%d", err, generations.Load())
						}
						record := waitVoiceAudit(t, fx.audits)
						if record.StatusCode != 400 || record.AccountID != nil || len(record.Attempts) != 0 {
							t.Fatalf("local rejection attributed upstream: %+v", record)
						}
						return
					}
					if stage == "budget" {
						if result != nil {
							_ = result.Body.Close()
						}
						if !errors.Is(err, clientkeyapp.ErrBillingLimit) || generations.Load() != 0 {
							t.Fatalf("budget err=%v calls=%d", err, generations.Load())
						}
						return
					}
					if err != nil && stage != "receipt" {
						t.Fatal(err)
					}
					if stage == "close" {
						_ = result.Body.Close()
					} else if stage == "cancel" {
						cancel()
					} else if result != nil {
						recorder := httptest.NewRecorder()
						c, _ := gin.CreateTestContext(recorder)
						c.Request = httptest.NewRequest("POST", path, nil).WithContext(ctx)
						if stage == "short_write" {
							c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
						}
						(&Handler{}).writeResult(c, result, stream, protocol)
						if stage == "success" && operation == audit.OperationResponses && !strings.Contains(recorder.Body.String(), `"store":false`) {
							t.Fatalf("ephemeral response claims storage: %s", recorder.Body.String())
						}
						if stage == "success" && (recorder.Code != 200 || strings.Contains(recorder.Body.String(), "commit_failed")) {
							t.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
						}
					}
					record := waitVoiceAudit(t, fx.audits)
					if retained.Snapshot().Used != 0 {
						t.Fatal("image text response retained memory after completion")
					}
					count, outcome := 2, "completed"
					if stage == "partial" || (lite && (stage == "storage" || stage == "download")) {
						count, outcome = 1, "partial"
					}
					if record.MediaOutputImages != int64(count) || record.GenerationOutcome != outcome {
						t.Fatalf("facts=%d/%s want=%d/%s error=%s", record.MediaOutputImages, record.GenerationOutcome, count, outcome, record.ErrorCode)
					}
					price, _ := audit.EstimateOfficialImageCost("grok-imagine-image", "", "", count)
					key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
					if err != nil || record.EstimatedCostInUSDTicks != price.CostInUSDTicks || key.BilledUsageUSDTicks != price.CostInUSDTicks || key.ReservedUsageUSDTicks != 0 {
						t.Fatalf("billing=%d record=%d reserved=%d want=%d err=%v", key.BilledUsageUSDTicks, record.EstimatedCostInUSDTicks, key.ReservedUsageUSDTicks, price.CostInUSDTicks, err)
					}
					wantCalls := int32(1)
					if lite {
						wantCalls = 2
						if stage == "storage" || stage == "download" {
							wantCalls = 1
						}
					}
					if generations.Load() != wantCalls {
						t.Fatalf("generation calls=%d", generations.Load())
					}
					windows, err := fx.accounts.GetQuotaWindows(context.Background(), []uint64{fx.account.ID})
					if err != nil || len(windows[fx.account.ID]) != 1 || windows[fx.account.ID][0].Remaining != 20-count {
						t.Fatalf("quota=%+v err=%v", windows, err)
					}
					if stage == "receipt" && record.PhysicalReceipt != "failed" {
						t.Fatalf("receipt=%s", record.PhysicalReceipt)
					}
					if stage == "success" && operation == audit.OperationResponses {
						input.PreviousResponseID = record.ResponseID
						payload["previous_response_id"] = record.ResponseID
						input.Body, _ = json.Marshal(payload)
						continuation, continuationErr := fx.service.CreateResponse(context.Background(), input)
						if continuation != nil {
							_ = continuation.Body.Close()
						}
						if err := continuationErr; !errors.Is(err, gateway.ErrResponseStateUnsupported) {
							t.Fatalf("ephemeral continuation err=%v", err)
						}
						if _, err := fx.service.GetResponse(context.Background(), gateway.ResourceInput{ClientKey: fx.created.Key, ResponseID: record.ResponseID}); !errors.Is(err, gateway.ErrResponseNotFound) {
							t.Fatalf("ephemeral GET err=%v", err)
						}
						if generations.Load() != wantCalls {
							t.Fatal("unsupported continuation generated another image")
						}
					}
					if record.OwnershipCommit != "not_required" {
						t.Fatalf("ephemeral image identity ownership=%s", record.OwnershipCommit)
					}
					if stage == "success" && record.DeliveryOutcome != "completed" {
						t.Fatalf("delivery=%s error=%s", record.DeliveryOutcome, record.ErrorCode)
					}
					if stage == "partial" && record.DeliveryOutcome == "completed" {
						t.Fatal("partial images reported complete")
					}
					if stage == "cancel" {
						// The canceled response must release the only account slot before a
						// second real generation can be accepted by this same service.
						next, err := fx.service.CreateResponse(context.Background(), gateway.Input{RequestID: "after-cancel", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Body: []byte(fmt.Sprintf(`{"model":%q,"input":"another synthetic image"}`, fx.publicModel))})
						if err != nil {
							t.Fatalf("capacity after cancel: %v", err)
						}
						_ = next.Body.Close()
					}
				})
			}
		}
	}
}
