package inference

import (
	"context"
	"encoding/json"
	"fmt"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"math"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestPricingRangePreservesGenerationAndLedger(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole} {
			for _, operation := range []audit.Operation{audit.OperationResponses, audit.OperationChat, audit.OperationMessages} {
				for _, stream := range []bool{false, true} {
					for _, mode := range []string{"product_positive_wrap", "sum_negative_wrap", "upstream_cost"} {
						t.Run(fmt.Sprintf("%s/%s/%s/stream=%t/%s", dialect, kind, operation, stream, mode), func(t *testing.T) {
							ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
							defer cancel()
							inputTokens, outputTokens, reportedCost := int64(math.MaxInt64), int64(5), int64(0)
							if mode == "sum_negative_wrap" {
								inputTokens = math.MaxInt64 / 40000
								outputTokens = 1 // Both long-context products fit; their sum does not.
							}
							if mode == "upstream_cost" {
								reportedCost = 55
							}
							var generated atomic.Int32
							up := completionHTTPUpstreamWithResponse(t, "grok-4.5", &generated, func(call int32, response map[string]any) {
								if call == 1 {
									response["usage"] = map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "cost_in_usd_ticks": reportedCost}
								}
							})
							defer up.Close()
							db := compactionDatabase(t, dialect)
							fx := newProviderCompletionFixtureOnDatabase(t, up.URL, "grok-4.5", kind, nil, nil, nil, nil, db, "")
							journal, err := relational.OpenAuditJournal(ctx, filepath.Join(t.TempDir(), "pricing-pending.db"), relational.AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20})
							if err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() {
								if err := journal.Close(); err != nil {
									t.Error(err)
								}
							})
							writer := auditapp.NewService(fx.audits, journal, nil, 4, time.Millisecond)
							writer.SetBillingObserver(fx.clientService)
							if err := writer.Start(ctx); err != nil {
								t.Fatal(err)
							}
							t.Cleanup(func() {
								stop, done := context.WithTimeout(context.Background(), time.Second)
								defer done()
								if err := writer.Close(stop); err != nil {
									t.Error(err)
								}
							})
							service := gateway.NewService(fx.models, writer, fx.accountService, fx.clientService, fx.registry, fx.selector, historyapp.NewResponseResources(relational.NewResponseRepository(db)), security.RandomTokenSource{}, executionapp.NewPhysicalJournalFactory(), nil, 1)
							service.SetGuardSnapshotSource(gateway.StaticGuardSnapshotSource(gateway.QualityRetryRuntime{Enabled: false}))
							service.SetQualityEventRecorder(fx.receipts)
							router := gin.New()
							router.Use(middleware.RequestID(nil), middleware.ClientAuth(fx.clientService))
							NewHandler(service, nil, 1<<20).Register(router.Group("/v1"))
							path := "/v1/responses"
							payload := map[string]any{"model": fx.publicModel, "stream": stream, "store": false}
							if operation == audit.OperationResponses {
								payload["input"] = "synthetic pricing"
							} else {
								payload["messages"] = []any{map[string]any{"role": "user", "content": "synthetic pricing"}}
							}
							if operation == audit.OperationChat {
								path = "/v1/chat/completions"
							}
							if operation == audit.OperationMessages {
								path = "/v1/messages"
								payload["max_tokens"] = 2048
								payload["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 1024}
							}
							body, err := json.Marshal(payload)
							if err != nil {
								t.Fatal(err)
							}
							send := func() {
								t.Helper()
								r := httptest.NewRequest("POST", path, strings.NewReader(string(body))).WithContext(ctx)
								r.Header.Set("Authorization", "Bearer "+fx.created.Secret)
								r.Header.Set("Content-Type", "application/json")
								if operation == audit.OperationMessages {
									r.Header.Set("anthropic-version", "2023-06-01")
								}
								w := httptest.NewRecorder()
								router.ServeHTTP(w, r)
								if w.Code != 200 || !strings.Contains(w.Body.String(), "completion answer") {
									t.Fatalf("HTTP %d: %s", w.Code, w.Body.String())
								}
							}
							send()
							record := waitVoiceAudit(t, fx.audits)
							record, err = fx.audits.Get(ctx, record.ID)
							if err != nil {
								t.Fatal(err)
							}
							if record.InputTokens != inputTokens || record.OutputTokens != outputTokens || record.CostInUSDTicks != reportedCost || record.EstimatedCostInUSDTicks != 0 || record.PricingModel != "" || record.PricingVersion != "" {
								t.Fatalf("invalid primary pricing or lost usage: %+v", record)
							}
							if record.GenerationOutcome != "completed" || record.DeliveryOutcome != "completed" || record.LedgerOutcome != "committed" || len(record.GenerationUsages) != 1 {
								t.Fatalf("lost generation or ledger: %+v", record)
							}
							fact := record.GenerationUsages[0]
							if !fact.Selected || fact.AccountID != fx.account.ID || fact.InputTokens != inputTokens || fact.OutputTokens != outputTokens || fact.CostInUSDTicks != reportedCost || fact.EstimatedCostInUSDTicks != 0 || fact.PricingModel != "" || fact.PricingVersion != "" {
								t.Fatalf("invalid generation pricing: %+v", fact)
							}
							key, err := fx.clients.Get(ctx, fx.created.Key.ID)
							if err != nil || key.BilledUsageUSDTicks != reportedCost || key.ReservedUsageUSDTicks != 0 {
								t.Fatalf("billing=%d reserved=%d err=%v want=%d", key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, err, reportedCost)
							}
							if state := journal.Snapshot(); state.Records != 0 || state.Rejected != 0 {
								t.Fatalf("valid generation quarantined: %+v", state)
							}
							if err := writer.CheckLedgerReady(); err != nil {
								t.Fatal(err)
							}
							// A normal following request proves the writer, reservation and
							// account leases remain usable after an unpriceable generation.
							send()
							rows, count, err := fx.audits.List(ctx, 0, 3)
							if err != nil || count != 2 || len(rows) != 2 {
								t.Fatalf("following audit count=%d err=%v", count, err)
							}
							if rows[0].InputTokens != 20 || rows[0].OutputTokens != 5 || rows[0].EstimatedCostInUSDTicks != 700000 || rows[0].LedgerOutcome != "committed" {
								t.Fatalf("normal following record=%+v", rows[0])
							}
							key, err = fx.clients.Get(ctx, fx.created.Key.ID)
							if err != nil || key.BilledUsageUSDTicks != reportedCost+700000 || key.ReservedUsageUSDTicks != 0 || generated.Load() != 2 {
								t.Fatalf("following billing=%d reserve=%d calls=%d err=%v", key.BilledUsageUSDTicks, key.ReservedUsageUSDTicks, generated.Load(), err)
							}
						})
					}
				}
			}
		}
	}
}
