package court

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestExperimentPolicyAndDeadlineSurviveSettingsChange(t *testing.T) {
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.InvestigationTimeout = time.Minute
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	defer s.Close(context.Background())
	id := openSimpleTestCase(t, s, b.registry)
	before, _, _ := b.registry.GetCase(context.Background(), id)
	saved := casePolicy(before, cfg)
	cfg.InvestigationTimeout = time.Hour
	s.SetConfig(cfg)
	views, err := s.LiveCaseViews(context.Background(), before.OpenedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got := views[0].Assessment.Policy; !reflect.DeepEqual(got, saved) {
		t.Fatalf("policy drifted: %v -> %v", saved, got)
	}
	if _, err := s.Evaluate(context.Background(), before.OpenedAt.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	closed, _, _ := b.registry.GetCase(context.Background(), id)
	if closed.Verdict != model.VerdictInsufficient {
		t.Fatal("settings change extended a hold")
	}
	var data struct {
		Assessment ExperimentReport `json:"assessment"`
		Trigger    string           `json:"trigger"`
	}
	if err := json.Unmarshal([]byte(closed.EvidenceJSON), &data); err != nil {
		t.Fatal(err)
	}
	if data.Trigger != "traffic_degraded" || !reflect.DeepEqual(data.Assessment.Policy, saved) {
		t.Fatalf("closure lost incident, policy, or interrupted measurements: %+v", data)
	}
}

func TestOneBrokenCaseDoesNotStarveAnotherDeadline(t *testing.T) {
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.InvestigationTimeout = time.Second
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	defer s.Close(context.Background())
	first := openSimpleTestCase(t, s, b.registry)
	if err := s.ReportDegraded(context.Background(), 8, model.EpochKey{NodeID: 4}); err != nil {
		t.Fatal(err)
	}
	if err := b.registry.DB().Exec(fmt.Sprintf("CREATE TRIGGER reject_one_case BEFORE UPDATE ON q_case WHEN OLD.id = %d BEGIN SELECT RAISE(ABORT, 'one case fails'); END", first)).Error; err != nil {
		t.Fatal(err)
	}

	if _, err := s.Evaluate(context.Background(), time.Now().Add(time.Minute)); err == nil {
		t.Fatal("failed case should be observable")
	}
	if !b.registry.AccountEligible(8) || !b.registry.ExitEligible(4) {
		t.Fatal("a failing earlier case starved an unrelated deadline")
	}
}

func TestManualReviewKeepsOriginalEvidence(t *testing.T) {
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	defer s.Close(context.Background())
	id := openSimpleTestCase(t, s, b.registry)
	if err := s.ReleaseAfterReview(context.Background(), id, "Matched controls were inconclusive; operator releases this case."); err != nil {
		t.Fatal(err)
	}
	record, _, _ := b.registry.GetCase(context.Background(), id)
	var payload struct {
		Manual struct {
			PreviousEvidence map[string]any `json:"previous_evidence"`
			Reason           string         `json:"reason"`
		} `json:"manual_review"`
	}
	if err := json.Unmarshal([]byte(record.EvidenceJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Manual.PreviousEvidence["trigger"] != "traffic_degraded" || payload.Manual.Reason == "" {
		t.Fatal("manual review erased original incident")
	}
	if !b.registry.AccountEligible(7) || !b.registry.ExitEligible(3) {
		t.Fatal("manual release failed")
	}
}
