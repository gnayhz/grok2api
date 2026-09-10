package audit

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

// The production writer settles real records before all three HTTP read paths
// explain them. Expected wire values are fixed, not calculated by the new owner.
func TestAuditExplanationReadContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db := explanationDatabase(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			keys := relational.NewClientKeyRepository(db)
			key, err := keys.Create(ctx, clientkey.Key{Name: "explanation", Prefix: "explanation", SecretHash: strings.Repeat("a", 64), EncryptedSecret: "test-fixture", Enabled: true, RPMLimit: 120, MaxConcurrent: 8, ModelScope: clientkey.ModelScopeAll, ProviderScope: clientkey.ProviderScopeAll, TierScope: clientkey.TierScopeAll})
			if err != nil {
				t.Fatal(err)
			}
			repo := relational.NewAuditRepository(db)
			journal := newTestAuditJournal(t, 32)
			service := auditapp.NewService(repo, journal, nil, 8, time.Millisecond)
			if err := service.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				stop, done := context.WithTimeout(context.Background(), time.Second)
				defer done()
				if err := service.Close(stop); err != nil {
					t.Error(err)
				}
			})
			first, terminal, late, subsecond := int64(250), int64(1250), int64(19763), int64(1200)
			tests := []struct {
				name    string
				value   auditdomain.Record
				billing string
				rate    *float64
				class   string
			}{
				{"current_formula", auditdomain.Record{InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, ContextInputTokens: 100, EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2026-08-13", Streaming: true, FirstTokenMS: &first, DurationMS: 1250}, `{"source":"official","method":"official_rates","model":"grok-build-0.1","version":"2026-08-13","tier":"standard","components":[{"kind":"uncached_input","unit":"token","quantity":80,"unitPriceInUsdTicks":10000,"subtotalInUsdTicks":800000},{"kind":"output","unit":"token","quantity":50,"unitPriceInUsdTicks":20000,"subtotalInUsdTicks":1000000},{"kind":"cached_input","unit":"token","quantity":20,"unitPriceInUsdTicks":2000,"subtotalInUsdTicks":40000}],"totalInUsdTicks":1840000}`, floatPointer(50), ""},
				{"upstream_wins", auditdomain.Record{CostInUSDTicks: 2_500_000, EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2026-08-13"}, `{"source":"upstream","method":"upstream_reported","components":[],"totalInUsdTicks":2500000}`, nil, ""},
				{"missing_model", auditdomain.Record{EstimatedCostInUSDTicks: 17}, `null`, nil, ""},
				{"old_version", auditdomain.Record{EstimatedCostInUSDTicks: 1_840_000, PricingModel: "grok-build-0.1", PricingVersion: "2025-01-01"}, `{"source":"official","method":"stored_estimate","model":"grok-build-0.1","version":"2025-01-01","components":[],"totalInUsdTicks":1840000}`, nil, ""},
				{"missing_version", auditdomain.Record{EstimatedCostInUSDTicks: 9, PricingModel: "grok-build-0.1"}, `{"source":"official","method":"stored_estimate","model":"grok-build-0.1","components":[],"totalInUsdTicks":9}`, nil, ""},
				{"mismatched_amount", auditdomain.Record{InputTokens: 100, CachedInputTokens: 20, OutputTokens: 50, EstimatedCostInUSDTicks: 7, PricingModel: "grok-build-0.1", PricingVersion: "2026-08-13"}, `{"source":"official","method":"stored_estimate","model":"grok-build-0.1","version":"2026-08-13","components":[],"totalInUsdTicks":7}`, nil, ""},
				{"wrapped_stored_estimate", auditdomain.Record{InputTokens: math.MaxInt64, OutputTokens: 5, EstimatedCostInUSDTicks: 560000, PricingModel: "grok-4.5", PricingVersion: "2026-08-13"}, `{"source":"official","method":"stored_estimate","model":"grok-4.5","version":"2026-08-13","components":[],"totalInUsdTicks":560000}`, nil, ""},
				{"unknown_model", auditdomain.Record{EstimatedCostInUSDTicks: 11, PricingModel: "future-model", PricingVersion: "2026-08-13"}, `{"source":"official","method":"stored_estimate","model":"future-model","version":"2026-08-13","components":[],"totalInUsdTicks":11}`, nil, ""},
				{"stt_stored_only", auditdomain.Record{Operation: auditdomain.OperationSTT, AudioDurationMS: 3450, EstimatedCostInUSDTicks: 958334, PricingModel: "grok-stt-rest", PricingVersion: "2026-08-13"}, `{"source":"official","method":"stored_estimate","model":"grok-stt-rest","version":"2026-08-13","components":[],"totalInUsdTicks":958334}`, nil, ""},
				{"terminal_burst", auditdomain.Record{Streaming: true, FirstTokenMS: &terminal, DurationMS: 1250, OutputTokens: 339}, `null`, nil, "terminal_burst"},
				{"subsecond_window", auditdomain.Record{Streaming: true, FirstTokenMS: &subsecond, DurationMS: 1250, OutputTokens: 339}, `null`, nil, ""},
				{"reasoning_window", auditdomain.Record{Streaming: true, FirstTokenMS: &late, DurationMS: 19827, OutputTokens: 1511, ReasoningTokens: 1400}, `null`, floatPointer(float64(1511) * 1000 / 19827), ""},
				{"error_with_200", auditdomain.Record{Streaming: true, FirstTokenMS: &terminal, DurationMS: 1250, OutputTokens: 339, ErrorCode: "delivery_failed"}, `null`, nil, ""},
				{"missing_measurement", auditdomain.Record{Streaming: true, DurationMS: 1250, OutputTokens: 339}, `null`, nil, ""},
				{"non_stream", auditdomain.Record{FirstTokenMS: &first, DurationMS: 1250, OutputTokens: 339}, `null`, nil, ""},
			}
			var expectedBilled int64
			for i := range tests {
				v := &tests[i].value
				v.EventID, v.RequestID = "evt_explanation_"+tests[i].name, tests[i].name
				v.ClientKeyID, v.ModelRouteID, v.StatusCode = key.ID, 1, 200
				v.CreatedAt = time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
				v.GenerationOutcome, v.DeliveryOutcome = "completed", "failed"
				v.PhysicalReceipt, v.QualityReceipt = "unconfirmed", "failed"
				if v.CostInUSDTicks > 0 {
					expectedBilled += v.CostInUSDTicks
				} else {
					expectedBilled += v.EstimatedCostInUSDTicks
				}
				if err := service.Create(ctx, *v); err != nil {
					t.Fatal(err)
				}
			}
			before, total, err := service.List(ctx, 1, 100)
			if err != nil || total != int64(len(tests)) {
				t.Fatalf("stored count=%d err=%v", total, err)
			}
			storedKey, err := keys.Get(ctx, key.ID)
			if err != nil || storedKey.BilledUsageUSDTicks != expectedBilled || storedKey.ReservedUsageUSDTicks != 0 {
				t.Fatalf("stored key=%+v err=%v want billed=%d", storedKey, err, expectedBilled)
			}
			router := gin.New()
			NewHandler(service).Register(router.Group("/api/admin/v1"))
			read := func(path string) map[string]json.RawMessage {
				t.Helper()
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
				if w.Code != 200 {
					t.Fatalf("%s: HTTP %d %s", path, w.Code, w.Body.String())
				}
				var envelope struct {
					Data map[string]json.RawMessage `json:"data"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				return envelope.Data
			}
			assertRow := func(raw json.RawMessage) {
				t.Helper()
				var row map[string]json.RawMessage
				var actual auditResponse
				if err := json.Unmarshal(raw, &row); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(raw, &actual); err != nil {
					t.Fatal(err)
				}
				for _, tc := range tests {
					if actual.RequestID != tc.name {
						continue
					}
					var want, got any
					if err := json.Unmarshal([]byte(tc.billing), &want); err != nil {
						t.Fatal(err)
					}
					if rawBilling, ok := row["billing"]; ok {
						if err := json.Unmarshal(rawBilling, &got); err != nil {
							t.Fatal(err)
						}
					}
					if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(actual.OutputTokensPerSecond, tc.rate) || actual.DegradeClass != tc.class {
						t.Fatalf("%s: billing=%s rate=%v class=%q; want=%s rate=%v class=%q", tc.name, row["billing"], actual.OutputTokensPerSecond, actual.DegradeClass, tc.billing, tc.rate, tc.class)
					}
					if tc.rate == nil && row["outputTokensPerSecond"] != nil {
						t.Fatalf("%s: missing rate must be omitted", tc.name)
					}
					if actual.CostInUSDTicks != tc.value.CostInUSDTicks || actual.EstimatedCostInUSDTicks != tc.value.EstimatedCostInUSDTicks || actual.LedgerOutcome != "committed" || actual.GenerationOutcome != "completed" || actual.DeliveryOutcome != "failed" || actual.PhysicalReceipt != "unconfirmed" || actual.QualityReceipt != "failed" {
						t.Fatalf("%s: read changed stored cost or independent outcomes: %+v", tc.name, actual)
					}
					if string(row["clientKeyId"]) != fmt.Sprintf("\"%d\"", key.ID) {
						t.Fatalf("key identity lost string encoding: %s", row["clientKeyId"])
					}
					return
				}
				t.Fatalf("unexpected record %s", actual.RequestID)
			}
			for _, path := range []string{"/api/admin/v1/request-audits?pageSize=100", "/api/admin/v1/request-audits?pagination=cursor&pageSize=100"} {
				var rows []json.RawMessage
				if err := json.Unmarshal(read(path)["items"], &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != len(tests) {
					t.Fatalf("%s: count=%d", path, len(rows))
				}
				for _, row := range rows {
					assertRow(row)
				}
			}
			for _, row := range before {
				assertRow(read(fmt.Sprintf("/api/admin/v1/request-audits/%d", row.ID))["audit"])
			}
			after, _, err := service.List(ctx, 1, 100)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("read mutated stored records: err=%v", err)
			}
			afterKey, err := keys.Get(ctx, key.ID)
			if err != nil || !reflect.DeepEqual(storedKey, afterKey) {
				t.Fatalf("read mutated key ledger: err=%v", err)
			}
			if pending := journal.Snapshot().Records; pending != 0 {
				t.Fatalf("pending records=%d", pending)
			}
		})
	}
}

func floatPointer(v float64) *float64 { return &v }

func explanationDatabase(t testing.TB, dialect string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if dialect == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "explanation.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("isolated TEST_POSTGRES_DSN required")
		}
		admin, openErr := sql.Open("pgx", dsn)
		if openErr != nil {
			t.Fatal(openErr)
		}
		schema := fmt.Sprintf("g54_explanation_%d", time.Now().UnixNano())
		if _, err = admin.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
			admin.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			defer admin.Close()
			if _, err := admin.ExecContext(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		q := parsed.Query()
		q.Set("search_path", schema)
		parsed.RawQuery = q.Encode()
		db, err = relational.OpenPostgres(ctx, parsed.String(), 8, 4)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}
