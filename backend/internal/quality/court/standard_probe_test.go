package court

import (
	"context"

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
	if len(claimed) != 1 || claimed[0].Direction != model.ProbeCaseProof {
		t.Fatal("not one shared proof task", claimed)
	}
	task := claimed[0]
	report := caseProofReport(task.Experiment, now, []model.ResourceObservation{
		caseProofObservation(task.Experiment, 1, 7, 3, "B"),
		caseProofObservation(task.Experiment, 2, 7, 4, "A"),
	})
	report.Revision = 1
	// Persist the two physical reservations separately, as the executor does.
	report.Calls = 1
	if err := store.SaveResourceCheckProgress(context.Background(), task.ID, report); err != nil {
		t.Fatal(err)
	}
	report.Calls, report.Revision = 2, 2
	if err := store.SaveResourceCheckProgress(context.Background(), task.ID, report); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteProbeTask(context.Background(), task.ID, model.ProbeDone, model.ProbeTaskResult{ResourceCheck: &report}, now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Evaluate(context.Background(), now.Add(30*time.Second)); err != nil {
		t.Fatal(err)
	}
	c, _, err = b.registry.GetCase(context.Background(), cases[0].ID)
	if err != nil || c.Verdict != model.VerdictExitGuilty {
		t.Fatalf("standard probes did not establish exit verdict: %+v %v", c, err)
	}
}
