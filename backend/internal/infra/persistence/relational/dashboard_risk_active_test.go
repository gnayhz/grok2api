package relational

import (
	"context"
	"testing"
	"time"
)

// TestDashboardActiveExcludesRiskFlagged locks the schedulable semantics of
// the dashboard active count: an enabled, healthy account carrying the
// rsc_denied flag can never be scheduled (selector excludes risk_status), so
// it must not be reported active (adversarial review P1: the count silently
// overstated capacity for flagged pools).
func TestDashboardActiveExcludesRiskFlagged(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewAccountRepository(database)
	rows := seedBatchUpdateAccounts(t, database, 2)
	ids := accountModelIDs(rows)

	// Flag one healthy enabled account as registration-risk.
	if err := repo.UpdateRiskStatus(ctx, ids[1], "rsc_denied"); err != nil {
		t.Fatal(err)
	}

	dashRepo := NewDashboardRepository(database)
	boundaries := testDashboardBoundaries(time.Now().UTC().Add(-24*time.Hour), 2*time.Hour, 12)
	snapshot, err := dashRepo.Snapshot(ctx, testDashboardWindow(boundaries), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Resources.ActiveAccounts != 1 {
		t.Fatalf("active = %d, want 1: risk-flagged accounts must not count as schedulable", snapshot.Resources.ActiveAccounts)
	}
	if snapshot.Resources.RiskAccounts != 1 {
		t.Fatalf("risk = %d, want 1", snapshot.Resources.RiskAccounts)
	}
}
