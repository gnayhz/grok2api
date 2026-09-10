package court

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestClearanceRequiresCompletedIndependentCleanGroup(t *testing.T) {
	policy := policyFor(DefaultConfig(), time.Now())
	for _, name := range []string{"exit while account pending", "account while exit pending", "single clean", "unresolved jury", "changed incident IP", "duplicate IP", "same node", "conflicting account"} {
		t.Run(name, func(t *testing.T) {
			tasks := experimentFixture(model.ProbeResultClean, model.ProbeResultClean)
			wantAccount, wantExit := false, false
			switch name {
			case "exit while account pending":
				for i := 0; i < 3; i++ {
					tasks[i].State = model.ProbePending
				}
				wantExit = true
			case "account while exit pending":
				for i := 3; i < len(tasks); i++ {
					tasks[i].State = model.ProbeRunning
				}
				wantAccount = true
			case "single clean":
				tasks = append(tasks[:1], tasks[3:4]...)
			case "unresolved jury":
				tasks[3].Result = model.ProbeResultError
				tasks[3].FailureKind = "transport"
				wantAccount = true
			case "changed incident IP":
				tasks[3].PathKey = "changed"
				wantAccount = true
			case "duplicate IP":
				for i := 0; i < 3; i++ {
					tasks[i].PathKey = "shared"
				}
				wantExit = true
			case "same node":
				for i := 0; i < 3; i++ {
					tasks[i].NodeID = 1
					tasks[i].Attempt.Path.NodeID = 1
				}
				wantExit = true
			case "conflicting account":
				tasks[0].Result = model.ProbeResultDegraded
				wantExit = true
			}
			got := assessExperiment(tasks, policy)
			if got.AccountCleared != wantAccount || got.ExitCleared != wantExit {
				t.Fatalf("clearance account=%v exit=%v; want %v %v", got.AccountCleared, got.ExitCleared, wantAccount, wantExit)
			}
		})
	}
}

func TestCompletedGroupReleasesBeforeOtherGroupAndIsNotReheld(t *testing.T) {
	for _, direction := range []model.ProbeDirection{model.ProbeExitJury, model.ProbeAccountDifferential} {
		t.Run(string(direction), func(t *testing.T) {
			b := newBench(t)
			cfg := DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			store := registry.NewProbeTaskStore(b.registry)
			s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
			defer s.Close(context.Background())
			id := openSimpleTestCase(t, s, b.registry)
			calls := 0
			s.SetAccountReleaseHook(func(context.Context, uint64) { calls++ })
			tasks, err := store.ClaimPendingProbeTasks(context.Background(), 32)
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range tasks {
				if task.Direction != direction {
					continue
				}
				accountID := task.DefendantAccountID
				if task.Direction == model.ProbeExitJury {
					accountID = task.JurorAccountID
				}
				err = store.CompleteProbeTask(context.Background(), task.ID, model.ProbeDone, model.ProbeTaskResult{Attempt: experimentIdentity(fmt.Sprintf("main-%d", task.ID), accountID, task.DefendantNodeID, task.DefendantEpoch), Outcome: model.ProbeResultClean, VerifiedIPChange: true, PathKey: fmt.Sprintf("path-%d", task.DefendantNodeID)}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				if _, err = s.Evaluate(context.Background(), time.Now()); err != nil {
					t.Fatal(err)
				}
				if err = s.ReportDegraded(context.Background(), 7, model.EpochKey{NodeID: 3}); err != nil {
					t.Fatal(err)
				}
				c, _, err := b.registry.GetCase(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				if c.Status != model.CaseInvestigating || c.ClosedAt != nil {
					t.Fatal("unfinished group was closed")
				}
				if b.registry.AccountEligible(7) != (direction == model.ProbeAccountDifferential) || b.registry.ExitEligible(3) != (direction == model.ProbeExitJury) {
					t.Fatal("clear party held or pending party released")
				}
			}
			if direction == model.ProbeAccountDifferential && calls != 1 {
				t.Fatalf("release hook calls=%d", calls)
			}
			if _, err = s.Evaluate(context.Background(), time.Now().Add(cfg.InvestigationTimeout+time.Minute)); err != nil {
				t.Fatal(err)
			}
			if !b.registry.AccountEligible(7) || !b.registry.ExitEligible(3) {
				t.Fatal("deadline failed to release remaining party")
			}
		})
	}
}
