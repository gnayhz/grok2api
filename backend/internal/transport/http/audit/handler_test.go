package audit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

func TestAuditGenerationDetailContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "generation-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAuditRepository(database)
	value := auditdomain.Record{EventID: "evt_generation_http_0001", RequestID: "generation-contract", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 55, CreatedAt: time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC), GenerationUsages: []auditdomain.GenerationUsage{
		{PhysicalID: "generation-contract/1", Ordinal: 1, AccountID: 11, AccountName: "first", Model: "grok-4.5", Outcome: "completed", UsageSource: auditdomain.UsageSourceUpstream, InputTokens: 100, OutputTokens: 15, TotalTokens: 115, CachedInputTokens: 30, CacheCreationTokens: 2, ContextInputTokens: 120, ContextOutputTokens: 15, ReasoningTokens: 4, CostInUSDTicks: 12345, EstimatedCostInUSDTicks: 12000, PricingModel: "grok-4.5", PricingVersion: "2026-09-08"},
		{PhysicalID: "generation-contract/2", Ordinal: 2, AccountID: 36028797018963969, AccountName: "selected", Model: "grok-4.5", Selected: true, Outcome: "completed", UsageSource: auditdomain.UsageSourceUpstream, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 55},
	}}
	if err := repo.Create(ctx, value); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	NewHandler(auditapp.NewService(repo, newTestAuditJournal(t, 8), slog.Default(), 4, time.Second)).Register(router.Group("/api/admin/v1"))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest("GET", "/api/admin/v1/request-audits/1", nil))
	if w.Code != 200 {
		t.Fatalf("HTTP %d %s", w.Code, w.Body.String())
	}
	var payload struct {
		Data auditDetailResponse `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.GenerationUsages) != 2 || payload.Data.GenerationUsages[1].AccountID != "36028797018963969" || payload.Data.Audit.InputTokens != 20 {
		t.Fatalf("detail changed counters or identity: %+v", payload.Data)
	}
	if path := os.Getenv("GROK_TEST_AUDIT_DETAIL_FIXTURE"); path != "" {
		if err := os.WriteFile(path, w.Body.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAuditDetailReturnsCompleteTextAndBinaryBodies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "audit-handler.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repository := relational.NewAuditRepository(database)
	now := time.Now().UTC()
	status := http.StatusBadGateway
	if err := repository.Create(ctx, auditdomain.Record{
		EventID: "evt_audit_handler_0001", RequestID: "request-detail", ClientKeyID: 1, ModelRouteID: 1, StatusCode: status, CreatedAt: now,
		Attempts: []auditdomain.Attempt{
			{Number: 1, Source: auditdomain.AttemptSourceUpstreamHTTP, Stage: "upstream_response", StartedAt: now, UpstreamStatusCode: &status, ResponseHeaders: http.Header{"Content-Type": {"application/json"}}, ResponseBody: []byte(`{"error":"complete"}`), ResponseBodyTruncated: true},
			{Number: 2, Source: auditdomain.AttemptSourceUpstreamHTTP, Stage: "upstream_response", StartedAt: now, UpstreamStatusCode: &status, ResponseHeaders: http.Header{}, ResponseBody: []byte{0x00, 0xff, 0x01}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	service := auditapp.NewService(repository, newTestAuditJournal(t, 8), slog.Default(), 4, time.Second)
	router := gin.New()
	NewHandler(service).Register(router.Group("/api/admin/v1"))

	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/1", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data struct {
			Audit struct {
				AttemptCount int `json:"attemptCount"`
			} `json:"audit"`
			Attempts []struct {
				ResponseBody          string `json:"responseBody"`
				ResponseBodyEncoding  string `json:"responseBodyEncoding"`
				ResponseBodyTruncated bool   `json:"responseBodyTruncated"`
			} `json:"attempts"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Data.Audit.AttemptCount != 2 || len(payload.Data.Attempts) != 2 {
		t.Fatalf("payload = %#v", payload)
	}
	if payload.Data.Attempts[0].ResponseBodyEncoding != "utf8" || payload.Data.Attempts[0].ResponseBody != `{"error":"complete"}` || !payload.Data.Attempts[0].ResponseBodyTruncated {
		t.Fatalf("text body = %#v", payload.Data.Attempts[0])
	}
	if payload.Data.Attempts[1].ResponseBodyEncoding != "base64" || payload.Data.Attempts[1].ResponseBody != base64.StdEncoding.EncodeToString([]byte{0x00, 0xff, 0x01}) {
		t.Fatalf("binary body = %#v", payload.Data.Attempts[1])
	}

	missing := httptest.NewRecorder()
	router.ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/api/admin/v1/request-audits/999", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, body = %s", missing.Code, missing.Body.String())
	}
}

func TestAuditResponseDerivesOutputThroughput(t *testing.T) {
	firstTokenMS := int64(250)
	response := newAuditResponse(auditdomain.Record{ClientIP: "203.0.113.8", StatusCode: http.StatusOK, Streaming: true, ReasoningEffort: "high", FirstTokenMS: &firstTokenMS, DurationMS: 1250, OutputTokens: 80})
	if response.ClientIP != "203.0.113.8" || response.ReasoningEffort != "high" || response.FirstTokenMS == nil || *response.FirstTokenMS != 250 || response.OutputTokensPerSecond == nil || *response.OutputTokensPerSecond != 80 {
		t.Fatalf("response = %#v", response)
	}

	unmeasured := newAuditResponse(auditdomain.Record{DurationMS: 1250, OutputTokens: 80})
	if unmeasured.OutputTokensPerSecond != nil {
		t.Fatalf("unmeasured throughput = %v", *unmeasured.OutputTokensPerSecond)
	}

	lateFirst := int64(19763)
	late := newAuditResponse(auditdomain.Record{StatusCode: http.StatusOK, Streaming: true, FirstTokenMS: &lateFirst, DurationMS: 19827, OutputTokens: 1511, ReasoningTokens: 1400})
	if late.OutputTokensPerSecond == nil || *late.OutputTokensPerSecond != float64(1511)*1000/19827 {
		t.Fatalf("late first-token throughput = %#v", late)
	}
	burstFirst := int64(10000)
	burst := newAuditResponse(auditdomain.Record{StatusCode: http.StatusOK, Streaming: true, FirstTokenMS: &burstFirst, DurationMS: 10100, OutputTokens: 2000})
	// 亚秒窗口的"速率"是末尾整包爆发除以毫秒的假象,不再作为吞吐展示;
	// 这类行的降智签名只在 first==dur 的 terminal_burst 形态归档。
	if burst.OutputTokensPerSecond != nil {
		t.Fatalf("sub-second burst window must not render a rate = %#v", burst)
	}

	// 降智档位:健康速率行无档位;亚秒窗口但仍有生成时间的行无档位
	// (速度列展示为空已足够);terminal_burst(first==dur、零思考、输出达
	// 口径)必须可见——速度列为空恰是它的形态(续聊链连续降智
	// 的审计签名)。
	if response.DegradeClass != "" {
		t.Fatalf("healthy row must not classify: %q", response.DegradeClass)
	}
	if burst.DegradeClass != "" {
		t.Fatalf("sub-second burst with real generation window must not classify: %q", burst.DegradeClass)
	}
	terminalFirst := int64(20362)
	terminal := newAuditResponse(auditdomain.Record{StatusCode: http.StatusOK, Streaming: true, FirstTokenMS: &terminalFirst, DurationMS: 20362, OutputTokens: 339, ReasoningTokens: 0})
	if terminal.OutputTokensPerSecond != nil || terminal.DegradeClass != auditdomain.DegradeClassTerminalBurst {
		t.Fatalf("terminal burst row = %#v", terminal)
	}
	failed := newAuditResponse(auditdomain.Record{StatusCode: http.StatusBadGateway, Streaming: true, FirstTokenMS: &terminalFirst, DurationMS: 20362, OutputTokens: 339})
	if failed.DegradeClass != "" {
		t.Fatalf("failed row must not classify: %q", failed.DegradeClass)
	}
}

func TestAuditResponseExplainsBillingWithoutChangingStoredTotal(t *testing.T) {
	estimated := newAuditResponse(auditdomain.Record{
		InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, ContextInputTokens: 100,
		EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: auditdomain.OfficialPricingAsOf,
	})
	if estimated.Billing == nil || estimated.Billing.Source != "official" || estimated.Billing.Method != "official_rates" || estimated.Billing.TotalInUSDTicks != 1_840_000 || len(estimated.Billing.Components) != 3 {
		t.Fatalf("estimated billing = %#v", estimated.Billing)
	}
	if estimated.Billing.Components[0].Kind != "uncached_input" || estimated.Billing.Components[1].Kind != "output" || estimated.Billing.Components[2].Kind != "cached_input" {
		t.Fatalf("billing component order = %#v", estimated.Billing.Components)
	}
	var componentTotal int64
	for _, component := range estimated.Billing.Components {
		componentTotal += component.SubtotalInUSDTicks
	}
	if componentTotal != estimated.Billing.TotalInUSDTicks {
		t.Fatalf("component total = %d, billing = %#v", componentTotal, estimated.Billing)
	}

	upstream := newAuditResponse(auditdomain.Record{CostInUSDTicks: 2_500_000})
	if upstream.Billing == nil || upstream.Billing.Source != "upstream" || upstream.Billing.Method != "upstream_reported" || upstream.Billing.TotalInUSDTicks != 2_500_000 || len(upstream.Billing.Components) != 0 {
		t.Fatalf("upstream billing = %#v", upstream.Billing)
	}

	historical := newAuditResponse(auditdomain.Record{
		InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50,
		EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "",
	})
	if historical.Billing == nil || historical.Billing.Method != "stored_estimate" || len(historical.Billing.Components) != 0 || historical.Billing.TotalInUSDTicks != 1_840_000 {
		t.Fatalf("historical billing = %#v", historical.Billing)
	}
}
