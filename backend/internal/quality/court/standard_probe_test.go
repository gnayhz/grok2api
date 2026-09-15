package court

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestToolTriggerDispatchesStandardProbes(t *testing.T) {
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	store := registry.NewProbeTaskStore(b.registry)
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
	defer s.Close(context.Background())
	now := time.Now().UTC()
	obs := model.Observation{At: now, AccountID: 7, Exit: model.EpochKey{NodeID: 3}, EventID: "synthetic-event", Attempt: attemptmeta.Identity{ID: "synthetic-attempt", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "synthetic-rule", Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "xhigh", Tools: true}}}
	if err := s.ReportDegradedObservation(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	cases, err := b.registry.ListOpenCases(context.Background())
	if err != nil || len(cases) != 1 {
		t.Fatalf("cases=%d err=%v", len(cases), err)
	}
	tasks, err := store.ListProbeTasksForCase(context.Background(), cases[0].ID)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("tasks=%d err=%v", len(tasks), err)
	}
	for _, task := range tasks {
		if !task.Experiment.Baseline.Profile.Tools || task.Experiment.Profile().Tools || task.Experiment.Profile().ReasoningEffort != "low" {
			t.Fatalf("lost trigger or standard profile: %+v", task.Experiment)
		}
	}
	if _, err := s.Evaluate(context.Background(), now.Add(15*time.Second)); err != nil {
		t.Fatal(err)
	}
	c, _, err := b.registry.GetCase(context.Background(), cases[0].ID)
	if err != nil || c.Status.Closed() {
		t.Fatalf("pending probes closed: %+v %v", c, err)
	}
	claimed, err := store.ClaimPendingProbeTasks(context.Background(), 32)
	if err != nil || len(claimed) != len(tasks) {
		t.Fatalf("claim=%d err=%v", len(claimed), err)
	}
	for _, task := range claimed {
		actual := task.Experiment.Baseline
		actual.Profile = task.Experiment.Profile()
		actual.ID = fmt.Sprintf("fictional-probe-%d", task.ID)
		actual.AccountID = task.DefendantAccountID
		actual.Path = attemptmeta.Path{NodeID: task.DefendantNodeID, Epoch: task.DefendantEpoch, Status: attemptmeta.PathRegistered}
		control := actual
		control.ID += "/control"
		control.AccountID = task.ControlAccountID
		control.Path.NodeID, control.Path.Epoch = task.ControlNodeID, task.ControlEpoch
		result := model.ProbeTaskResult{Outcome: model.ProbeResultClean, VerifiedIPChange: true,
			ControlOutcome: model.ProbeResultClean, ControlVerified: true,
			PathKey: fmt.Sprintf("fictional-path-%d", task.DefendantNodeID), ControlPathKey: fmt.Sprintf("fictional-path-%d", task.ControlNodeID)}
		if task.Direction == model.ProbeExitJury {
			actual.AccountID = task.JurorAccountID
			result.Outcome = model.ProbeResultDegraded
		}
		result.Attempt, result.ControlAttempt = actual, control
		if err := store.CompleteProbeTask(context.Background(), task.ID, model.ProbeDone, result, now.Add(20*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Evaluate(context.Background(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	c, _, err = b.registry.GetCase(context.Background(), cases[0].ID)
	if err != nil || c.Verdict != model.VerdictExitGuilty {
		t.Fatalf("standard probes did not establish exit verdict: %+v %v", c, err)
	}
}

func TestReplacementProbesSurviveSlowCandidateRead(t *testing.T) {
	for _, slow := range []bool{false, true} {
		t.Run(fmt.Sprintf("slow=%v", slow), func(t *testing.T) {
			ctx := context.Background()
			b := newBench(t)
			store := registry.NewProbeTaskStore(b.registry)
			cfg := DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			cfg.AccountNeedExits = 2
			cfg.AccountSpanNodes = 2
			cfg.ExitNeedN = 3
			cfg.ExitNeedK = 2
			s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
			defer s.Close(ctx)
			id := openSimpleTestCase(t, s, b.registry)
			claimed, err := store.ClaimPendingProbeTasks(ctx, 32)
			if err != nil || len(claimed) != 5 {
				t.Fatalf("initial=%d err=%v", len(claimed), err)
			}
			jury := 0
			for _, task := range claimed {
				account := task.DefendantAccountID
				result := model.ProbeTaskResult{Outcome: model.ProbeResultClean, VerifiedIPChange: true, PathKey: fmt.Sprintf("synthetic-path-%d", task.DefendantNodeID)}
				if task.Direction == model.ProbeExitJury {
					jury++
					account = task.JurorAccountID
					result.Outcome = model.ProbeResultDegraded
					result.VerifiedIPChange = false
					result.ControlOutcome = model.ProbeResultClean
					if jury == 2 {
						result.ControlOutcome = model.ProbeResultError
					}
					if jury == 3 {
						result.ControlOutcome = model.ProbeResultDegraded
					}
					result.ControlVerified = true
					result.ControlPathKey = fmt.Sprintf("synthetic-path-%d", task.ControlNodeID)
					result.ControlAttempt = experimentIdentity(fmt.Sprintf("synthetic-control-%d", task.ID), task.ControlAccountID, task.ControlNodeID, task.ControlEpoch)
				}
				result.Attempt = experimentIdentity(fmt.Sprintf("synthetic-main-%d", task.ID), account, task.DefendantNodeID, task.DefendantEpoch)
				if err := store.CompleteProbeTask(ctx, task.ID, model.ProbeDone, result, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			tasks, _ := store.ListProbeTasksForCase(ctx, id)
			report := assessExperiment(tasks, policyFor(cfg, time.Now()))
			if report.Exit.ConfirmedDegraded != 1 || report.Account.Clean != 2 || report.Verdict != model.VerdictNone {
				t.Fatalf("assessment=%+v", report)
			}
			reads := 0
			s.SetProbeAccounts(probeAccountsFunc(func(c context.Context, _ model.ProbeExperiment) ([]uint64, error) {
				reads++
				if slow {
					timer := time.NewTimer(1100 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-c.Done():
						return nil, c.Err()
					case <-timer.C:
					}
				}
				return []uint64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, nil
			}))
			stats, err := s.Evaluate(ctx, time.Now().UTC())
			tasks, _ = store.ListProbeTasksForCase(ctx, id)
			if err != nil || stats.Retried != 2 || len(tasks) != 7 || reads != 1 {
				t.Fatalf("replacement: stats=%+v tasks=%d reads=%d err=%v", stats, len(tasks), reads, err)
			}
			c, _, err := b.registry.GetCase(ctx, id)
			if err != nil || c.Status.Closed() {
				t.Fatalf("replacement investigation closed: %+v %v", c, err)
			}
			if !b.registry.AccountEligible(7) {
				t.Fatal("clean defendant not released")
			}
			replacements, err := store.ClaimPendingProbeTasks(ctx, 32)
			if err != nil || len(replacements) != 2 {
				t.Fatalf("replacement claim=%d err=%v", len(replacements), err)
			}
			for _, task := range replacements {
				result := model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, ControlOutcome: model.ProbeResultClean, ControlVerified: true,
					PathKey: fmt.Sprintf("synthetic-path-%d", task.DefendantNodeID), ControlPathKey: fmt.Sprintf("synthetic-path-%d", task.ControlNodeID),
					Attempt:        experimentIdentity(fmt.Sprintf("synthetic-main-%d", task.ID), task.JurorAccountID, task.DefendantNodeID, task.DefendantEpoch),
					ControlAttempt: experimentIdentity(fmt.Sprintf("synthetic-control-%d", task.ID), task.ControlAccountID, task.ControlNodeID, task.ControlEpoch)}
				if err := store.CompleteProbeTask(ctx, task.ID, model.ProbeDone, result, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Evaluate(ctx, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			c, _, err = b.registry.GetCase(ctx, id)
			if err != nil || c.Verdict != model.VerdictExitGuilty {
				t.Fatalf("supplemental evidence did not settle case: %+v %v", c, err)
			}
		})
	}
}
