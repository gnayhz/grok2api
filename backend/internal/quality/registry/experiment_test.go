package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestExperimentTransactionsAndLateResults(t *testing.T) {
	r := openTestRegistry(t)
	ctx := context.Background()
	now := time.Now().UTC()
	// Fail after the case and account hold were written. No partial state may survive.
	if err := r.db.Exec("CREATE TRIGGER reject_party BEFORE INSERT ON q_case_party BEGIN SELECT RAISE(ABORT, 'injected open failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 3}, now, `{}`); err == nil {
		t.Fatal("expected injected failure")
	}
	var count int64
	r.db.Table("q_case").Count(&count)
	if count != 0 || !r.AccountEligible(7) || !r.ExitEligible(3) {
		t.Fatal("partial opening survived rollback")
	}
	r.db.Exec("DROP TRIGGER reject_party")
	id, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 3}, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	store := NewProbeTaskStore(r)
	taskID, err := store.CreateProbeTask(ctx, model.ProbeTask{CaseID: id, Direction: model.ProbeAccountDifferential, DefendantAccountID: 7, DefendantNodeID: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ClaimPendingProbeTasks(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.db.Exec("CREATE TRIGGER reject_close BEFORE UPDATE ON q_probe_task BEGIN SELECT RAISE(ABORT, 'injected close failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictInsufficient, `{}`, now, true); err == nil {
		t.Fatal("expected injected failure")
	}
	c, _, _ := r.GetCase(ctx, id)
	if c.Status != model.CaseInvestigating || r.AccountEligible(7) || r.ExitEligible(3) {
		t.Fatal("partial settlement escaped transaction")
	}
	r.db.Exec("DROP TRIGGER reject_close")
	if err := r.SettleInvestigation(ctx, id, model.VerdictInsufficient, `{"reason":"deadline"}`, now, true); err != nil {
		t.Fatal(err)
	}
	if !r.AccountEligible(7) || !r.ExitEligible(3) {
		t.Fatal("successful settlement did not release both holds")
	}
	if err := store.CompleteProbeTask(ctx, taskID, model.ProbeDone, model.ProbeTaskResult{Outcome: model.ProbeResultDegraded}, now); !errors.Is(err, model.ErrProbeAlreadySettled) {
		t.Fatalf("late result accepted: %v", err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictAccountGuilty, `{}`, now, true); err != nil {
		t.Fatal(err)
	}
	c, _, _ = r.GetCase(ctx, id)
	if c.Verdict != model.VerdictInsufficient {
		t.Fatal("replay rewrote an existing verdict")
	}
}

func TestManualReleasePreservesOtherConvictionsAndAuditHistory(t *testing.T) {
	r := openTestRegistry(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 3}, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, first, model.VerdictAccountGuilty, `{}`, now, true); err != nil {
		t.Fatal(err)
	}
	second, err := r.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 4}, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, second, model.VerdictAccountGuilty, `{}`, now, true); err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, second, model.VerdictInsufficient, `{"manual_review":{}}`, now, true, true); err != nil {
		t.Fatal(err)
	}
	if r.AccountEligible(7) || r.AccountState(7).CurrentCaseID != first {
		t.Fatal("manual release erased another conviction")
	}
	if _, _, _, err := r.CleanExpiredCaseHistory(ctx, now.Add(40*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := r.GetCase(ctx, first); !found {
		t.Fatal("restriction outlived its evidence")
	}
	if err := r.SettleInvestigation(ctx, first, model.VerdictInsufficient, `{"manual_review":{}}`, now, true, true); err != nil {
		t.Fatal(err)
	}
	if !r.AccountEligible(7) {
		t.Fatal("last reviewed conviction was not released")
	}
}
