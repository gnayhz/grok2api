package court

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/investigator"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func caseProofObservation(spec model.ProbeExperiment, id int, a, n uint64, class string) model.ResourceObservation {
	actual := spec.Baseline
	actual.ID, actual.AccountID, actual.Profile = fmt.Sprintf("fictional-proof-%d", id), a, spec.Profile()
	actual.Path = attemptmeta.Path{NodeID: n, Status: attemptmeta.PathRegistered}
	s := model.ResourceSample{Sample: "token-short", Generated: true, Completed: true, UsageReported: true, IdentityVerified: true, PathVerified: true, PlainOutput: true, PathKey: fmt.Sprintf("fictional-exit-%d", n), PathFamily: 4, Attempt: actual, Outcome: model.MeasurementDegraded}
	if class == "A" {
		s.Thinking, s.Outcome = true, model.MeasurementClean
	}
	return model.ResourceObservation{ID: id, Window: 1, IdentityGroup: a, AccountID: a, NodeID: n, Class: class, Sample: s}
}
func caseProofReport(spec model.ProbeExperiment, now time.Time, observations []model.ResourceObservation) model.ResourceCheckReport {
	p := spec.ResourceCheck
	r := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Kind: p.Kind, ResourceID: p.ResourceID, Window: 1, WindowStartedAt: now, MaxCalls: p.MaxCalls, Calls: len(observations), Observations: observations, Outcome: "inconclusive", Reason: "insufficient_controls"}
	for _, target := range p.Targets {
		r.Results = append(r.Results, model.ResourceProof{ResourceTarget: target})
	}
	return model.AssessResourceProofs(r, now)
}

func TestCaseProofSharesRulesIncludingConflictsPartialAndExpiry(t *testing.T) {
	now := time.Now().UTC()
	spec := model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}, ResourceCheck: &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, MaxCalls: 6, Targets: []model.ResourceTarget{{Kind: "account", ResourceID: 7}, {Kind: "node", ResourceID: 3}}}}
	policy := ExperimentPolicy{Version: model.CaseProofVersion, Experiment: spec, DeadlineAt: now.Add(time.Minute)}
	obs := func(id int, a, n uint64, class string) model.ResourceObservation {
		return caseProofObservation(spec, id, a, n, class)
	}
	for _, tc := range []struct {
		name         string
		observations []model.ResourceObservation
		want         model.Verdict
		reason       string
	}{
		{"normal", []model.ResourceObservation{obs(1, 7, 3, "A")}, model.VerdictNone, "proved_normal"},
		{"account", []model.ResourceObservation{obs(1, 7, 3, "B"), obs(2, 8, 3, "A")}, model.VerdictAccountGuilty, "proved_account_bad"},
		{"exit", []model.ResourceObservation{obs(1, 7, 3, "B"), obs(2, 7, 4, "A")}, model.VerdictExitGuilty, "proved_exit_bad"},
		{"both", []model.ResourceObservation{obs(1, 8, 4, "A"), obs(2, 7, 4, "B"), obs(3, 8, 3, "B")}, model.VerdictBothGuilty, "proved_account_and_exit_bad"},
		{"all B", []model.ResourceObservation{obs(1, 7, 3, "B"), obs(2, 8, 3, "B")}, model.VerdictNone, "insufficient_controls"},
		{"account proved exit unknown", []model.ResourceObservation{obs(1, 7, 3, "B"), obs(2, 7, 4, "B"), obs(3, 8, 4, "A")}, model.VerdictAccountGuilty, "proved_account_bad"},
		{"normal counterexample blocks majority", []model.ResourceObservation{obs(1, 7, 4, "A"), obs(2, 7, 3, "B"), obs(3, 8, 3, "A")}, model.VerdictNone, "conflicting_samples"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proof := caseProofReport(spec, now, tc.observations)
			tasks := []model.ProbeTaskView{{Direction: model.ProbeCaseProof, Experiment: spec, State: model.ProbeDone, ResourceCheck: &proof}}
			got := assessCaseProof(tasks, policy, now)
			if got.Verdict != tc.want || got.Reason != tc.reason {
				t.Fatalf("got %+v", got)
			}
			if tc.name == "account proved exit unknown" && (got.ExitCleared || got.Proof.Results[1].Outcome != "inconclusive") {
				t.Fatal("unknown exit claimed normal")
			}
			if got := assessCaseProof(tasks, policy, now.Add(model.ResourceCheckWindow)); got.Verdict != model.VerdictNone {
				t.Fatal("expired proof convicted", got)
			}
			for _, state := range []model.ProbeTaskState{model.ProbeFailed, model.ProbeCancelled} {
				tasks[0].State = state
				got := assessCaseProof(tasks, policy, now)
				if got.Verdict != model.VerdictNone || got.AccountCleared || got.ExitCleared {
					t.Fatal("interrupted task made a resource decision", state, got)
				}
			}
			tasks[0].State = model.ProbeRunning
			if got := assessCaseProof(tasks, policy, now); got.Verdict != model.VerdictNone {
				t.Fatal("unfinished search convicted", got)
			}
		})
	}
}

type caseMeasurements struct {
	badAccount, badNode uint64
	calls               int
}

func (m *caseMeasurements) MeasureResourceCheck(ctx context.Context, a, n uint64) model.ResourceSample {
	m.calls++
	spec, _ := model.ProbeExperimentFromContext(ctx)
	class := "A"
	if a == m.badAccount || n == m.badNode {
		class = "B"
	}
	return caseProofObservation(spec, m.calls, a, n, class).Sample
}

func TestCaseProofDurableQueueFourWorldsAndManualRelease(t *testing.T) {
	for _, tc := range []struct {
		name                string
		badAccount, badNode uint64
		verdict             model.Verdict
	}{
		{"normal", 0, 0, model.VerdictInsufficient}, {"account", 7, 0, model.VerdictAccountGuilty}, {"exit", 0, 3, model.VerdictExitGuilty}, {"both", 7, 3, model.VerdictBothGuilty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b := newBench(t)
			for id := uint64(1); id <= 8; id++ {
				if err := b.registry.RecordExitIdentity(ctx, id, model.ExitIdentity{IPv4: fmt.Sprintf("192.0.2.%d", id)}); err != nil {
					t.Fatal(err)
				}
			}
			store := registry.NewProbeTaskStore(b.registry)
			cfg := DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
			defer s.Close(ctx)
			obs := model.Observation{At: time.Now().UTC(), AccountID: 7, Exit: model.EpochKey{NodeID: 3}, EventID: "fictional-incident", Attempt: attemptmeta.Identity{ID: "fictional-trigger", Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}
			if err := s.ReportDegradedObservation(ctx, obs); err != nil {
				t.Fatal(err)
			}
			cases, err := b.registry.ListOpenCases(ctx)
			if err != nil || len(cases) != 1 {
				t.Fatal(cases, err)
			}
			m := &caseMeasurements{badAccount: tc.badAccount, badNode: tc.badNode}
			e := investigator.NewProbeExecutor(b.registry)
			e.SetResourceChecks(m, store)
			worker := investigator.New(store, b.evidence)
			if err := worker.RunDueOnce(ctx, e, 1); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Evaluate(ctx, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			c, _, err := b.registry.GetCase(ctx, cases[0].ID)
			if err != nil || c.Verdict != tc.verdict {
				t.Fatalf("case=%+v calls=%d err=%v", c, m.calls, err)
			}
			if m.calls > 6 || m.calls < 1 {
				t.Fatal("unbounded/empty measurements", m.calls)
			}
			if b.registry.AccountEligible(7) == tc.verdict.RestrictsAccount() || b.registry.ExitEligible(3) == tc.verdict.RestrictsExit() {
				t.Fatal("wrong party restriction")
			}
			if err := s.ReleaseAfterReview(ctx, c.ID, "fictional review"); err != nil {
				t.Fatal(err)
			}
			if !b.registry.AccountEligible(7) || !b.registry.ExitEligible(3) {
				t.Fatal("review failed to release both")
			}
		})
	}
}

func TestCaseProofEpochChangePreservesAccountFindingOnly(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if err := b.registry.RecordExitIdentity(ctx, 3, model.ExitIdentity{IPv4: "192.0.2.3"}); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	store := registry.NewProbeTaskStore(b.registry)
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
	defer s.Close(ctx)
	now := time.Now().UTC()
	obs := model.Observation{At: now, AccountID: 7, Exit: model.EpochKey{NodeID: 3}, EventID: "fictional-epoch-event", Attempt: attemptmeta.Identity{ID: "fictional-trigger", Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}
	if err := s.ReportDegradedObservation(ctx, obs); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.ClaimPendingProbeTasks(ctx, 1)
	if err != nil || len(tasks) != 1 {
		t.Fatal(tasks, err)
	}
	task := tasks[0]
	r := caseProofReport(task.Experiment, now, []model.ResourceObservation{caseProofObservation(task.Experiment, 1, 8, 4, "A"), caseProofObservation(task.Experiment, 2, 7, 4, "B"), caseProofObservation(task.Experiment, 3, 8, 3, "B")})
	for i := 1; i <= 3; i++ {
		r.Calls = i
		r.Revision = uint64(i)
		if err := store.SaveResourceCheckProgress(ctx, task.ID, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.CompleteProbeTask(ctx, task.ID, model.ProbeDone, model.ProbeTaskResult{ResourceCheck: &r}, now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.registry.AdvanceEpoch(ctx, 3, model.ExitIdentity{IPv4: "192.0.2.4"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	c, _, err := b.registry.GetCase(ctx, task.CaseID)
	if err != nil || c.Verdict != model.VerdictAccountGuilty || !b.registry.ExitEligible(3) || b.registry.AccountEligible(7) {
		t.Fatal("epoch change lost account proof or restricted replacement", c, err)
	}
}

func TestCaseProofChecksOnlyCurrentCertificateIdentities(t *testing.T) {
	for _, tc := range []struct {
		name        string
		changed     uint64
		unavailable bool
		want        model.Verdict
	}{
		{"current", 0, false, model.VerdictBothGuilty},
		{"defendant replaced", 7, false, model.VerdictExitGuilty},
		{"control replaced", 8, false, model.VerdictInsufficient},
		{"fact source unavailable", 0, true, model.VerdictNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			b := newBench(t)
			cfg := DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			store := registry.NewProbeTaskStore(b.registry)
			s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
			defer s.Close(ctx)
			s.SetProofIdentityCheck(func(_ context.Context, sample model.ResourceSample) (bool, error) {
				if tc.unavailable {
					return false, errors.New("fictional facts unavailable")
				}
				return sample.Attempt.AccountID != tc.changed, nil
			})
			now := time.Now().UTC()
			obs := model.Observation{At: now, AccountID: 7, Exit: model.EpochKey{NodeID: 3}, EventID: "fictional-current-event", Attempt: attemptmeta.Identity{ID: "fictional-trigger", Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}
			if err := s.ReportDegradedObservation(ctx, obs); err != nil {
				t.Fatal(err)
			}
			tasks, err := store.ClaimPendingProbeTasks(ctx, 1)
			if err != nil || len(tasks) != 1 {
				t.Fatal(tasks, err)
			}
			task := tasks[0]
			r := caseProofReport(task.Experiment, now, []model.ResourceObservation{caseProofObservation(task.Experiment, 1, 8, 4, "A"), caseProofObservation(task.Experiment, 2, 7, 4, "B"), caseProofObservation(task.Experiment, 3, 8, 3, "B")})
			for i := 1; i <= 3; i++ {
				r.Calls, r.Revision = i, uint64(i)
				if err := store.SaveResourceCheckProgress(ctx, task.ID, r); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.CompleteProbeTask(ctx, task.ID, model.ProbeDone, model.ProbeTaskResult{ResourceCheck: &r}, now); err != nil {
				t.Fatal(err)
			}
			_, err = s.Evaluate(ctx, time.Now().UTC())
			if (err != nil) != tc.unavailable {
				t.Fatal("unexpected facts error", err)
			}
			c, _, err := b.registry.GetCase(ctx, task.CaseID)
			if err != nil || c.Verdict != tc.want {
				t.Fatalf("case=%+v err=%v", c, err)
			}
			if tc.unavailable && c.Status != model.CaseInvestigating {
				t.Fatal("unavailable facts must defer until deadline")
			}
		})
	}
}
