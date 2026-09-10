package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestEarlyReleaseAtomicReplayRestartAndOtherHolds(t *testing.T) {
	ctx := context.Background()
	opts := Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")}
	r, err := Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 3}, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.OpenInvestigation(ctx, 8, model.EpochKey{NodeID: 3}, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	store := NewProbeTaskStore(r)
	if _, err = store.CreateProbeTask(ctx, model.ProbeTask{CaseID: first, Direction: model.ProbeAccountDifferential, DefendantAccountID: 7, DefendantNodeID: 4}); err != nil {
		t.Fatal(err)
	}
	if err = r.db.Exec("CREATE TRIGGER reject_clearance BEFORE UPDATE ON q_case BEGIN SELECT RAISE(ABORT, 'clearance write failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err = r.ReleaseClearedParties(ctx, first, true, true, `{"early_releases":{}}`, now); err == nil {
		t.Fatal("failure not reported")
	}
	p, _ := r.ListParties(ctx, first)
	for _, v := range p {
		if v.Disposition != model.DispositionRemanded {
			t.Fatal("partial release escaped transaction")
		}
	}
	if r.AccountEligible(7) || r.ExitEligible(3) {
		t.Fatal("snapshot escaped rollback")
	}
	r.db.Exec("DROP TRIGGER reject_clearance")
	if err = r.ReleaseClearedParties(ctx, first, true, true, `{"early_releases":{"exit":{"reason":"exit_controls_clean"}}}`, now); err != nil {
		t.Fatal(err)
	}
	if !r.AccountEligible(7) || r.ExitEligible(3) || r.ExitStateOfCurrentEpoch(3).CurrentCaseID != second {
		t.Fatal("other hold lost")
	}
	if err = r.ReleaseClearedParties(ctx, first, true, true, `{"replayed":true}`, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	c, _, _ := r.GetCase(ctx, first)
	if c.EvidenceJSON == `{"replayed":true}` {
		t.Fatal("replay changed audit")
	}
	tasks, _ := store.ListProbeTasksForCase(ctx, first)
	if len(tasks) != 1 || tasks[0].State != model.ProbePending {
		t.Fatal("release cancelled unfinished tests")
	}
	if err = r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(ctx, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if !r.AccountEligible(7) || r.ExitEligible(3) {
		t.Fatal("restart changed resource eligibility")
	}
	if err = r.SettleInvestigation(ctx, second, model.VerdictInsufficient, `{}`, now, true); err != nil {
		t.Fatal(err)
	}
	if !r.ExitEligible(3) {
		t.Fatal("last hold not released")
	}
}
