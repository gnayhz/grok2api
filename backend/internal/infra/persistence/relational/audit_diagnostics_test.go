package relational

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"reflect"
	"testing"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	audithttp "github.com/chenyme/grok2api/backend/internal/transport/http/audit"
	"github.com/gin-gonic/gin"
)

func TestAuditDiagnosticsPersistDetailOnlyAndOldRowsRemainUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewAuditRepository(db)
			key := settlementTestKey(t, db, "synthetic-diag")
			old := settlementRecord(key.ID, "synthetic-old", 0)
			if err := repo.Create(ctx, old); err != nil {
				t.Fatal(err)
			}
			value := settlementRecord(key.ID, "synthetic-diag", 0)
			value.Diagnostics = &audit.ExecutionDiagnostics{Version: 1, Stages: []audit.StageDiagnostic{{Stage: "history_commit", US: 730, Calls: 1}}, Failures: []audit.FailureDiagnostic{{Component: "history", Stage: "store", Reason: "store_lock"}}, Exchanges: []audit.ExchangeDiagnostic{{PhysicalID: "synthetic/1", AccountID: 36028797018963969, NodeID: 3, Events: []audit.NetworkDiagnostic{{Stage: "got_conn", Reused: true, US: 50}}}}}
			if err := repo.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			rows, _, err := repo.List(ctx, 0, 10)
			if err != nil || len(rows) != 2 {
				t.Fatalf("rows=%d err=%v", len(rows), err)
			}
			var id uint64
			for _, row := range rows {
				if row.Diagnostics != nil {
					t.Fatal("list fetched large diagnostics")
				}
				got, err := repo.Get(ctx, row.ID)
				if err != nil {
					t.Fatal(err)
				}
				if row.RequestID == value.RequestID {
					id = row.ID
					if !reflect.DeepEqual(got.Diagnostics, value.Diagnostics) {
						t.Fatal("lost diagnostics")
					}
				} else if got.Diagnostics != nil {
					t.Fatal("fabricated old observations")
				}
			}
			writer := auditapp.NewService(repo, newTestAuditJournal(t, 8), nil, 4, 0)
			router := gin.New()
			audithttp.NewHandler(writer).Register(router.Group("/admin"))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest("GET", fmt.Sprintf("/admin/request-audits/%d", id), nil))
			var response struct {
				Data struct {
					Audit struct {
						Diagnostics json.RawMessage `json:"diagnostics"`
					} `json:"audit"`
				} `json:"data"`
			}
			if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &response) != nil {
				t.Fatalf("http=%d", w.Code)
			}
			var doc struct {
				Exchanges []struct {
					AccountID string `json:"accountId"`
				} `json:"exchanges"`
			}
			if json.Unmarshal(response.Data.Audit.Diagnostics, &doc) != nil || doc.Exchanges[0].AccountID != "36028797018963969" {
				t.Fatal("HTTP lost identity precision")
			}
			if err := db.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			got, err := repo.Get(ctx, id)
			if err != nil || !reflect.DeepEqual(got.Diagnostics, value.Diagnostics) {
				t.Fatal("restart migration changed diagnostics")
			}
		})
	}
}
