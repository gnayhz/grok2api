package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/cli"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/console"
	webprovider "github.com/chenyme/grok2api/backend/internal/infra/provider/web"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/streampipe"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/testsupport"
	"github.com/gin-gonic/gin"
)

type textConversionFault func(*provider.Response)
type textBuildFault struct {
	*cli.Adapter
	fault textConversionFault
}

func TestWithheldTextUsageRetainsActualAccounts(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole} {
		for _, operation := range []audit.Operation{audit.OperationResponses, audit.OperationChat, audit.OperationMessages} {
			for _, stream := range []bool{false, true} {
				for _, exhausted := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/exhausted=%t", kind, operation, stream, exhausted), func(t *testing.T) {
						var generated atomic.Int32
						upstream := completionHTTPUpstreamWithResponse(t, "grok-4.5", &generated, func(call int32, body map[string]any) {
							if call == 1 || exhausted {
								body["output"] = body["output"].([]any)[1:]
								body["suppress_thinking"] = true
							}
							if call == 1 {
								body["usage"] = map[string]any{"input_tokens": 100, "output_tokens": 15, "total_tokens": 115, "input_tokens_details": map[string]any{"cached_tokens": 30}, "context_details": map[string]any{"input_tokens": 120, "output_tokens": 15}, "cost_in_usd_ticks": 12345}
							} else {
								body["usage"].(map[string]any)["cost_in_usd_ticks"] = 55
							}
						})
						defer upstream.Close()
						fx := newProviderCompletionFixture(t, upstream.URL, "grok-4.5", kind, nil, nil)
						ctx := context.Background()
						first := fx.account
						first.Priority = 200
						if _, err := fx.accounts.UpdateAdministration(ctx, first.ID, repository.AccountAdminPatch{AccountUpdates: repository.AccountUpdates{Priority: &first.Priority}}); err != nil {
							t.Fatal(err)
						}
						second := first
						second.ID = 0
						second.Priority = 100
						second.Name = "second"
						second.SourceKey = "second"
						second.UserID = "another-user"
						second, _, err := fx.accounts.UpsertByIdentity(ctx, second)
						if err != nil {
							t.Fatal(err)
						}
						if err := testsupport.Capabilities(ctx, fx.models, fx.accounts, second.ID, []string{"grok-4.5"}, time.Now()); err != nil {
							t.Fatal(err)
						}
						if kind == account.ProviderConsole {
							for _, id := range []uint64{first.ID, second.ID} {
								if err := saveQuotaWindowsFixture(fx.accounts, ctx, id, "", time.Now(), []account.QuotaWindow{{Mode: console.QuotaMode, Remaining: 50, Total: 50, WindowSeconds: 3600}}); err != nil {
									t.Fatal(err)
								}
							}
						}
						fx.service.UpdateMaxAttempts(2)
						fx.service.SetGuardSnapshotSource(gateway.StaticGuardSnapshotSource(gateway.QualityRetryRuntime{Enabled: true, MaxAttempts: 2, GuardedModels: []string{"grok-4.5", "grok-chat-fast"}}))
						payload := map[string]any{"model": fx.publicModel, "stream": stream}
						path := "/v1/responses"
						if operation == audit.OperationResponses {
							payload["input"] = "synthetic retry"
						} else {
							payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic retry"}}
							path = "/v1/chat/completions"
						}
						if operation == audit.OperationMessages {
							path = "/v1/messages"
							payload["max_tokens"] = 2048
							payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
						}
						data, _ := json.Marshal(payload)
						request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
						request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
						request.Header.Set("Content-Type", "application/json")
						request.Header.Set("anthropic-version", "2023-06-01")
						out := httptest.NewRecorder()
						fx.router.ServeHTTP(out, request)
						record := waitVoiceAudit(t, fx.audits)
						record, err = fx.audits.Get(ctx, record.ID)
						if err != nil {
							t.Fatal(err)
						}
						if generated.Load() != 2 || len(record.GenerationUsages) != 2 {
							t.Fatalf("calls=%d generations=%+v status=%d error=%s", generated.Load(), record.GenerationUsages, out.Code, record.ErrorCode)
						}
						a, b := record.GenerationUsages[0], record.GenerationUsages[1]
						if a.AccountID != first.ID || b.AccountID != second.ID || a.Selected || b.Selected == exhausted || a.InputTokens != 100 || a.CachedInputTokens != 30 || a.ContextInputTokens != 120 || a.CostInUSDTicks != 12345 || b.CostInUSDTicks != 55 || a.Outcome != "completed" || b.Outcome != "completed" {
							t.Fatalf("misattributed generation: %+v", record.GenerationUsages)
						}
						key, err := fx.clients.Get(ctx, fx.created.Key.ID)
						if err != nil {
							t.Fatal(err)
						}
						wantCost := int64(55)
						if exhausted {
							wantCost = 0
						}
						if key.BilledUsageUSDTicks != wantCost || key.ReservedUsageUSDTicks != 0 || record.CostInUSDTicks != wantCost {
							t.Fatalf("billing changed: billed=%d reserved=%d audit=%d expected=%d", key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, record.CostInUSDTicks, wantCost)
						}
						if !exhausted && (record.InputTokens != 20 || record.AccountID == nil || *record.AccountID != second.ID || out.Code != 200) {
							t.Fatalf("wrong selected main row: %+v", record)
						}
						if kind == account.ProviderConsole {
							for _, id := range []uint64{first.ID, second.ID} {
								windows, err := fx.accounts.GetQuotaWindows(ctx, []uint64{id})
								if err != nil || len(windows[id]) != 1 || windows[id][0].Remaining != 49 {
									t.Fatalf("quota account=%d windows=%+v error=%v", id, windows, err)
								}
							}
						}
					})
				}
			}
		}
	}
}

type textConsoleFault struct {
	*console.Adapter
	fault textConversionFault
}
type textWebFault struct {
	*webprovider.Adapter
	fault textConversionFault
}

func (a textBuildFault) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	r, e := a.Adapter.ForwardResponse(ctx, request)
	if r != nil {
		a.fault(r)
	}
	return r, e
}
func (a textConsoleFault) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	r, e := a.Adapter.ForwardResponse(ctx, request)
	if r != nil {
		a.fault(r)
	}
	return r, e
}
func (a textWebFault) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	r, e := a.Adapter.ForwardResponse(ctx, request)
	if r != nil {
		a.fault(r)
	}
	return r, e
}

type textAdmissionFault struct{}

func (textAdmissionFault) RecordQualityEvent(context.Context, gateway.QualityObservation, time.Duration) error {
	return errors.New("injected admission receipt failure")
}

func TestWebTextArchiveFailureRetainsGenerationWithoutReplay(t *testing.T) {
	for _, operation := range []audit.Operation{audit.OperationResponses, audit.OperationChat, audit.OperationMessages} {
		t.Run(string(operation), func(t *testing.T) {
			var generated atomic.Int32
			up := completionWebUpstreamWithExtra(t, &generated, []map[string]any{{"type": "response.grok.output", "output": map[string]any{"card_attachment": map[string]any{"image_chunk": map[string]any{"progress": 100, "image_url": "https://assets.grok.com/generated/unused.png"}}}}})
			defer up.Close()
			// No asset store is configured. The real Web adapter fails its archive
			// stage after consuming the native terminal; no asset download occurs.
			fx := newProviderCompletionFixture(t, up.URL, "grok-chat-fast", account.ProviderWeb, nil, nil)
			ctx := context.Background()
			second := fx.account
			second.ID = 0
			second.Name = "second"
			second.SourceKey = "second"
			second.UserID = "597f19f8-49d4-458a-bee4-43ec3dcaf8ca"
			second, _, err := fx.accounts.UpsertByIdentity(ctx, second)
			if err != nil {
				t.Fatal(err)
			}
			if err := testsupport.Capabilities(ctx, fx.models, fx.accounts, second.ID, []string{"grok-chat-fast"}, time.Now()); err != nil {
				t.Fatal(err)
			}
			fx.service.UpdateMaxAttempts(3)
			payload := map[string]any{"model": fx.publicModel}
			path := "/v1/responses"
			if operation == audit.OperationResponses {
				payload["input"] = "synthetic image in text"
			} else {
				payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic image in text"}}
				path = "/v1/chat/completions"
			}
			if operation == audit.OperationMessages {
				path = "/v1/messages"
				payload["max_tokens"] = 2048
			}
			data, _ := json.Marshal(payload)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(data)))
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("anthropic-version", "2023-06-01")
			out := httptest.NewRecorder()
			fx.router.ServeHTTP(out, request)
			record := waitVoiceAudit(t, fx.audits)
			record, err = fx.audits.Get(ctx, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if generated.Load() != 1 || len(record.GenerationUsages) != 1 || record.GenerationOutcome != "completed" || record.InputTokens == 0 || record.OutputTokens == 0 || record.ErrorCode != "media_post_processing_failed" || record.DeliveryOutcome == "completed" {
				t.Fatalf("lost archive failure facts: calls=%d audit=%+v", generated.Load(), record)
			}
			fact := record.GenerationUsages[0]
			if !fact.Selected || fact.UsageSource != audit.UsageSourceEstimated || record.AccountID == nil || fact.AccountID != *record.AccountID {
				t.Fatalf("wrong account or source: %+v", fact)
			}
			key, err := fx.clients.Get(ctx, fx.created.Key.ID)
			if err != nil || key.ReservedUsageUSDTicks != 0 || key.BilledUsageUSDTicks != record.EstimatedCostInUSDTicks || key.BilledUsageUSDTicks == 0 {
				t.Fatalf("lost archive usage settlement: billed=%d reserved=%d err=%v", key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, err)
			}
		})
	}
}

func TestCanonicalTextUsagePresenceAndBounds(t *testing.T) {
	for _, stage := range []string{"zero", "absent", "missing_total", "negative", "invalid_type", "overflow_sum"} {
		t.Run(stage, func(t *testing.T) {
			var generated atomic.Int32
			up := completionHTTPUpstreamWithResponse(t, "grok-4.5", &generated, func(_ int32, body map[string]any) {
				switch stage {
				case "zero":
					body["usage"] = map[string]any{"input_tokens": 0, "output_tokens": 0}
				case "absent":
					delete(body, "usage")
					body["output"].([]any)[1].(map[string]any)["usage"] = map[string]any{"input_tokens": 99999}
				case "missing_total":
					body["usage"] = map[string]any{"input_tokens": 20, "output_tokens": 5}
				case "negative":
					body["usage"] = map[string]any{"input_tokens": -2, "output_tokens": -7, "total_tokens": -9}
				case "invalid_type":
					body["usage"] = map[string]any{"input_tokens": "20"}
				case "overflow_sum":
					body["usage"] = map[string]any{"input_tokens": int64(9223372036854775807), "output_tokens": 5}
				}
			})
			defer up.Close()
			fx := newProviderCompletionFixture(t, up.URL, "grok-4.5", account.ProviderConsole, nil, nil)
			fx.service.SetGuardSnapshotSource(gateway.StaticGuardSnapshotSource(gateway.QualityRetryRuntime{Enabled: false}))
			body := []byte(fmt.Sprintf(`{"model":%q,"input":"synthetic"}`, fx.publicModel))
			result, err := fx.service.CreateResponse(context.Background(), gateway.Input{RequestID: "usage-presence", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Body: body})
			if err != nil {
				t.Fatal(err)
			}
			_ = result.Body.Close()
			record := waitVoiceAudit(t, fx.audits)
			record, err = fx.audits.Get(context.Background(), record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(record.GenerationUsages) != 1 || record.GenerationOutcome != "completed" {
				t.Fatalf("lost generation: %+v", record)
			}
			fact := record.GenerationUsages[0]
			if stage == "overflow_sum" && (fact.EstimatedCostInUSDTicks != 0 || fact.PricingModel != "" || record.EstimatedCostInUSDTicks != 0 || record.PricingModel != "") {
				t.Fatalf("unrepresentable pricing accepted: input=%d output=%d detail_estimate=%d detail_model=%q main_estimate=%d main_model=%q", fact.InputTokens, fact.OutputTokens, fact.EstimatedCostInUSDTicks, fact.PricingModel, record.EstimatedCostInUSDTicks, record.PricingModel)
			}
			wantSource := audit.UsageSourceUpstream
			if stage == "absent" || stage == "invalid_type" {
				wantSource = audit.UsageSourceNone
			}
			wantTotal := int64(0)
			if stage == "missing_total" {
				wantTotal = 25
			} else if stage == "overflow_sum" {
				wantTotal = 9223372036854775807
			}
			if fact.UsageSource != wantSource || record.UsageSource != wantSource || fact.TotalTokens != wantTotal || record.TotalTokens != wantTotal {
				t.Fatalf("presence/bounds mismatch: fact=%+v main=%+v", fact, record)
			}
		})
	}
}

// Real Build HTTP, Console DPoP/HTTP and Web WS adapters feed the Gateway and
// SQL billing. Only the local converter/receipt port is fault-injected.
func TestTextGenerationBeforeDeliveryMatrix(t *testing.T) {
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		for _, operation := range []audit.Operation{audit.OperationResponses, audit.OperationChat, audit.OperationMessages} {
			for _, stream := range []bool{false, true} {
				stages := []string{"success", "close_after_read", "conversion"}
				if !stream {
					stages = append(stages, "unread_close", "cancel", "cancel_conversion", "short_write")
					if kind != account.ProviderWeb {
						stages = append(stages, "admission_receipt")
					}
				}
				for _, stage := range stages {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/%s", kind, operation, stream, stage), func(t *testing.T) {
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						var generated atomic.Int32
						model, endpoint := "grok-4.5", ""
						if kind == account.ProviderWeb {
							model = "grok-chat-fast"
							up := completionWebUpstream(t, &generated)
							defer up.Close()
							endpoint = up.URL
						} else {
							up := completionHTTPUpstream(t, model, &generated)
							defer up.Close()
							endpoint = up.URL
						}
						var wrap func(provider.Adapter) provider.Adapter
						if stage == "conversion" || stage == "cancel_conversion" {
							fault := textConversionFault(func(r *provider.Response) {
								if stream {
									r.ConvertJSON = nil
									r.ConvertStream = func(source io.ReadCloser) io.ReadCloser {
										return streampipe.Transform(source, func(source io.Reader, _ io.Writer) error {
											_, _ = io.Copy(io.Discard, source)
											return errors.New("injected stream converter failure")
										})
									}
								} else {
									r.ConvertStream = nil
									r.ConvertJSON = func([]byte) ([]byte, error) {
										if stage == "cancel_conversion" {
											cancel()
										}
										return nil, errors.New("injected JSON converter failure")
									}
								}
							})
							wrap = func(a provider.Adapter) provider.Adapter {
								switch v := a.(type) {
								case *cli.Adapter:
									return textBuildFault{v, fault}
								case *console.Adapter:
									return textConsoleFault{v, fault}
								case *webprovider.Adapter:
									return textWebFault{v, fault}
								default:
									panic("unexpected adapter")
								}
							}
						}
						fx := newProviderCompletionFixture(t, endpoint, model, kind, nil, wrap)
						fx.service.SetGuardSnapshotSource(gateway.StaticGuardSnapshotSource(gateway.QualityRetryRuntime{Enabled: true, MaxAttempts: 2, GuardedModels: []string{"grok-4.5", "grok-chat-fast"}}))
						if stage == "admission_receipt" {
							fx.service.SetQualityEventRecorder(textAdmissionFault{})
						}
						budget := responsebuffer.NewPool(512 << 20).Request(96 << 20)
						ctx = responsebuffer.WithContext(ctx, budget)
						payload := map[string]any{"model": fx.publicModel, "stream": stream}
						if operation == audit.OperationResponses {
							payload["input"] = "synthetic text"
						} else {
							payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic text"}}
						}
						if operation == audit.OperationMessages {
							payload["max_tokens"] = 2048
							payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
						}
						body, _ := json.Marshal(payload)
						input := gateway.Input{RequestID: "text-generation", ClientKey: fx.created.Key, PublicModel: fx.publicModel, Body: body, Streaming: stream, Operation: operation}
						var result *gateway.Result
						var err error
						switch operation {
						case audit.OperationChat:
							result, err = fx.service.CreateChatCompletion(ctx, input)
						case audit.OperationMessages:
							result, err = fx.service.CreateMessage(ctx, input)
						default:
							result, err = fx.service.CreateResponse(ctx, input)
						}
						preFailure := (!stream && (stage == "conversion" || stage == "cancel_conversion")) || stage == "admission_receipt"
						if preFailure {
							if err == nil {
								if result != nil {
									_ = result.Body.Close()
								}
								t.Fatal("expected pre-handoff error")
							}
						} else {
							if err != nil {
								t.Fatal(err)
							}
							switch stage {
							case "unread_close":
								_ = result.Body.Close()
							case "cancel":
								cancel()
							case "close_after_read":
								_, _ = io.Copy(io.Discard, result.Body)
								_ = result.Body.Close()
							default:
								recorder := httptest.NewRecorder()
								c, _ := gin.CreateTestContext(recorder)
								c.Request = httptest.NewRequest("POST", "/v1/responses", nil).WithContext(ctx)
								if stage == "short_write" {
									c.Writer = &completionPartialWriter{ResponseWriter: c.Writer, limit: 3}
								}
								protocol := streamProtocolResponses
								if operation == audit.OperationChat {
									protocol = streamProtocolChat
								} else if operation == audit.OperationMessages {
									protocol = streamProtocolAnthropic
								}
								(&Handler{}).writeResult(c, result, stream, protocol)
								if stage == "success" && recorder.Code != 200 {
									t.Fatalf("HTTP %d %s", recorder.Code, recorder.Body.String())
								}
							}
						}
						record := waitVoiceAudit(t, fx.audits)
						record, err = fx.audits.Get(context.Background(), record.ID)
						if err != nil {
							t.Fatal(err)
						}
						if len(record.GenerationUsages) != 1 || record.GenerationOutcome != "completed" {
							t.Fatalf("lost known generation: %+v", record)
						}
						fact := record.GenerationUsages[0]
						if !fact.Selected || fact.AccountID != fx.account.ID || fact.InputTokens != record.InputTokens || fact.OutputTokens != record.OutputTokens || fact.Outcome != "completed" {
							t.Fatalf("wrong attempt: %+v record=%+v", fact, record)
						}
						if kind != account.ProviderWeb && (fact.InputTokens != 20 || fact.OutputTokens != 5 || fact.ReasoningTokens != 2) {
							t.Fatalf("lost canonical counters: %+v", fact)
						}
						if kind == account.ProviderWeb && fact.UsageSource != audit.UsageSourceEstimated {
							t.Fatalf("Web estimate mislabeled: %+v", fact)
						}
						key, err := fx.clients.Get(context.Background(), fx.created.Key.ID)
						if err != nil || key.ReservedUsageUSDTicks != 0 || key.BilledUsageUSDTicks != record.EstimatedCostInUSDTicks {
							t.Fatalf("settlement mismatch billed=%d expected=%d reserved=%d err=%v", key.BilledUsageUSDTicks, record.EstimatedCostInUSDTicks, key.ReservedUsageUSDTicks, err)
						}
						if stage != "success" && record.DeliveryOutcome == "completed" {
							t.Fatalf("invented delivery: %+v", record)
						}
						if generated.Load() != 1 || budget.Snapshot().Used != 0 {
							t.Fatalf("calls=%d retained=%d", generated.Load(), budget.Snapshot().Used)
						}
					})
				}
			}
		}
	}
}
