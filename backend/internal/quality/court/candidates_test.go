package court

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type probeAccountsFunc func(context.Context, model.ProbeExperiment) ([]uint64, error)

func (f probeAccountsFunc) QualityProbeAccounts(ctx context.Context, p model.ProbeExperiment) ([]uint64, error) {
	return f(ctx, p)
}

type candidateDispatcher struct {
	store *registry.ProbeTaskStore
	specs []DispatchSpec
}

func (d *candidateDispatcher) DispatchForCase(ctx context.Context, spec DispatchSpec) (int, error) {
	d.specs = append(d.specs, spec)
	return (simpleTaskDispatcher{store: d.store}).DispatchForCase(ctx, spec)
}

func TestCourtCandidateFailurePreservesCaseAndFrozenExperiment(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	dispatch := &candidateDispatcher{store: registry.NewProbeTaskStore(b.registry)}
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, dispatch)
	t.Cleanup(func() { _ = s.Close(ctx) })
	fault := errors.New("account facts unavailable")
	fail := true
	experiment := model.NewProbeExperiment(model.Observation{EventID: "incident", Attempt: attemptmeta.Identity{ID: "attempt", Provider: "grok_build", Model: "grok-4.6", RuleVersion: "v1", Profile: attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "high"}}})
	s.SetProbeAccounts(probeAccountsFunc(func(_ context.Context, got model.ProbeExperiment) ([]uint64, error) {
		if got.Baseline != experiment.Baseline || got.TriggerEventID != experiment.TriggerEventID || got.Version != model.ResourceCheckVersion || got.Sample != "token-short" {
			t.Errorf("experiment changed: %+v", got)
		}
		if fail {
			return nil, fault
		}
		return []uint64{7, 2, 3, 4, 5}, nil
	}))
	obs := model.Observation{At: time.Now(), AccountID: 7, Exit: model.EpochKey{NodeID: 3}, EventID: experiment.TriggerEventID, Attempt: experiment.Baseline}
	if err := s.ReportDegradedObservation(ctx, obs); !errors.Is(err, fault) {
		t.Fatalf("initial failure swallowed: %v", err)
	}
	records, err := b.registry.ListOpenCases(ctx)
	if err != nil || len(records) != 1 {
		t.Fatalf("case lost: %+v %v", records, err)
	}
	id := records[0].ID
	if b.registry.AccountEligible(7) {
		t.Fatal("read failure released defendant")
	}
	tasks, err := dispatch.store.ListProbeTasksForCase(ctx, id)
	if err != nil || len(tasks) != 0 || len(dispatch.specs) != 0 {
		t.Fatal("read failure consumed a task")
	}
	if _, err := s.Evaluate(ctx, time.Now()); !errors.Is(err, fault) {
		t.Fatalf("replacement failure swallowed: %v", err)
	}
	record, found, err := b.registry.GetCase(ctx, id)
	if err != nil || !found || record.Status.Closed() {
		t.Fatalf("read failure closed investigation: %+v %v", record, err)
	}
	if record.EvidenceJSON != records[0].EvidenceJSON {
		t.Fatal("read failure rewrote evidence")
	}
	// Live settings may change after opening. Candidates and controls retain the
	// original model/effort and the saved finite plan limits.
	changed := s.Config()
	changed.ExitNeedN = 8
	changed.AccountNeedExits = 6
	s.SetConfig(changed)
	fail = false
	if stats, err := s.Evaluate(ctx, time.Now()); err != nil || stats.Retried == 0 {
		t.Fatalf("candidate read recovery did not dispatch: %+v %v", stats, err)
	}
	if len(dispatch.specs) != 1 || !dispatch.specs[0].Proof {
		t.Fatalf("replacement directions=%d", len(dispatch.specs))
	}
	for _, spec := range dispatch.specs {
		if len(spec.ControlAccounts) > cfg.ExitNeedN || len(spec.ControlExits) > cfg.AccountNeedExits {
			t.Fatalf("live settings enlarged controls: %+v", spec)
		}
		for _, id := range append(append([]uint64{}, spec.Jurors...), spec.ControlAccounts...) {
			if id < 2 || id > 5 {
				t.Fatalf("nonindependent/unoffered account: %d", id)
			}
		}
	}
	tasks, err = dispatch.store.ListProbeTasksForCase(ctx, id)
	if err != nil || len(tasks) == 0 {
		t.Fatalf("no persisted recovery work: %v", err)
	}
}
