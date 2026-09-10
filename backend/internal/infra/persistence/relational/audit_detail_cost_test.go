package relational

import (
	"context"
	"os"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

var detailCostSink audit.Record

// Run only when comparing the same committed record and database layout on
// before/after versions. Benchmark excludes schema setup and event insertion.
func TestAuditDetailReadCost(t *testing.T) {
	if os.Getenv("GROK_TEST_AUDIT_DETAIL_COST") != "1" {
		t.Skip("explicit local cost comparison")
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, db, "detail-cost")
			value := settlementRecord(key.ID, "detail-cost", 30)
			value.Attempts[0].ResponseBody = []byte(`{"error":"synthetic bounded detail"}`)
			value.GenerationUsages = []audit.GenerationUsage{{PhysicalID: "detail-cost/1", Ordinal: 1, AccountID: 1, Selected: true, Outcome: "completed", UsageSource: audit.UsageSourceUpstream, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 30}}
			repo := NewAuditRepository(db)
			if err := repo.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			rows, _, err := repo.List(ctx, 0, 1)
			if err != nil || len(rows) != 1 {
				t.Fatal(err)
			}
			id := rows[0].ID
			result := testing.Benchmark(func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					v, err := repo.Get(ctx, id)
					if err != nil {
						b.Fatal(err)
					}
					detailCostSink = v
				}
			})
			t.Logf("DETAIL_COST dialect=%s ns=%d bytes=%d allocs=%d", dialect, result.NsPerOp(), result.AllocedBytesPerOp(), result.AllocsPerOp())
		})
	}
}
