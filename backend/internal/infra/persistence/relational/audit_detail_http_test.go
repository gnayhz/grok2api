package relational

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	audithttp "github.com/chenyme/grok2api/backend/internal/transport/http/audit"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func TestAuditDetailHTTPKeepsGenerationsDuringRetention(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key := settlementTestKey(t, a, "detail-http")
			value := settlementRecord(key.ID, "detail-http", 30)
			value.CreatedAt = time.Now().UTC().Add(-48 * time.Hour)
			value.GenerationUsages = []audit.GenerationUsage{{PhysicalID: "detail-http/1", Ordinal: 1, AccountID: 36028797018963969, Selected: true, Outcome: "completed", UsageSource: audit.UsageSourceUpstream, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 30}}
			repo := NewAuditRepository(a)
			writer := auditapp.NewService(repo, newTestAuditJournal(t, 8), nil, 4, time.Millisecond)
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
			if err := writer.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			rows, _, err := repo.List(ctx, 0, 1)
			if err != nil || len(rows) != 1 {
				t.Fatal(err)
			}
			router := gin.New()
			audithttp.NewHandler(writer).Register(router.Group("/admin"))
			read := func() *httptest.ResponseRecorder {
				w := httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/admin/request-audits/%d", rows[0].ID), nil).WithContext(ctx))
				return w
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var once atomic.Bool
			const hook = "audit_http_after_attempts"
			if err := a.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "request_audit_attempts" && tx.Error == nil && once.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.db.Callback().Query().Remove(hook) })
			response := make(chan *httptest.ResponseRecorder, 1)
			go func() { response <- read() }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			deleted := make(chan error, 1)
			go func() {
				n, e := NewAuditRepository(b).DeleteOlderThan(ctx, time.Now().UTC(), 10)
				if e == nil && n != 1 {
					e = fmt.Errorf("deleted=%d", n)
				}
				deleted <- e
			}()
			if dialect == "postgres" {
				select {
				case err := <-deleted:
					deleted <- err
				case <-ctx.Done():
					close(release)
					t.Fatal(ctx.Err())
				}
			}
			close(release)
			var w *httptest.ResponseRecorder
			select {
			case w = <-response:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if w.Code != 200 {
				t.Fatalf("detail HTTP %d %s", w.Code, w.Body.String())
			}
			var payload struct {
				Data struct {
					Audit struct {
						AttemptCount  int    `json:"attemptCount"`
						EstimatedCost int64  `json:"estimatedCostInUsdTicks"`
						Ledger        string `json:"ledgerOutcome"`
					} `json:"audit"`
					Attempts    []json.RawMessage `json:"attempts"`
					Generations []struct {
						AccountID string `json:"accountId"`
						Cost      int64  `json:"costInUsdTicks"`
					} `json:"generationUsages"`
				} `json:"data"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Data.Audit.AttemptCount != 1 || len(payload.Data.Attempts) != 1 || len(payload.Data.Generations) != 1 || payload.Data.Generations[0].AccountID != "36028797018963969" || payload.Data.Generations[0].Cost != 30 || payload.Data.Audit.EstimatedCost != 30 || payload.Data.Audit.Ledger != "committed" {
				t.Fatalf("incomplete HTTP detail: %s", w.Body.String())
			}
			select {
			case err := <-deleted:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if next := read(); next.Code != 404 {
				t.Fatalf("next detail must observe deletion: %d %s", next.Code, next.Body.String())
			}
			assertSettlementKey(t, b, key.ID, 30, 0)
			if tableRowCount(t, b, "billing_settlements") != 1 {
				t.Fatal("detail read/retention changed settlement")
			}
		})
	}
}
