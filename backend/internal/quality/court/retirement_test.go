package court

import (
	"context"
	"encoding/json"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEvidenceAnnotationsPreserveFrozenLargeIntegers(t *testing.T) {
	policy := caseProofPolicy(9007199254740993, model.EpochKey{NodeID: 9007199254740995}, model.Observation{}, time.Now().UTC(), DefaultConfig())
	policy.Experiment.ResourceCheck.Seed = 9223372036854775783
	raw, err := json.Marshal(map[string]any{"policy": policy})
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		var envelope map[string]any
		if err := decodeEvidence(string(raw), &envelope); err != nil {
			t.Fatal(err)
		}
		envelope["early_releases"] = map[string]any{"account": map[string]any{"reason": "proved_normal"}}
		raw, err = json.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		got := casePolicy(model.CaseRecord{EvidenceJSON: string(raw)}, DefaultConfig())
		if !reflect.DeepEqual(got, policy) {
			t.Fatal("annotation changed frozen proof identity")
		}
	}
}

func TestRetiredOpenCaseClosesWithoutApplyingOldThresholds(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	store := registry.NewProbeTaskStore(b.registry)
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{store})
	defer s.Close(ctx)
	old := `{"policy":{"version":"controlled-comparison-v2","account_paths":1},"fictional_evidence":{"id":9007199254740993}}`
	id, err := b.registry.OpenInvestigation(ctx, 7, model.EpochKey{NodeID: 3}, time.Now().UTC(), old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	c, _, err := b.registry.GetCase(ctx, id)
	if err != nil || c.Verdict != model.VerdictInsufficient || !b.registry.AccountEligible(7) || !b.registry.ExitEligible(3) {
		t.Fatalf("retired case held resources: %+v %v", c, err)
	}
	if !strings.Contains(c.EvidenceJSON, "protocol_retired") || !strings.Contains(c.EvidenceJSON, "9007199254740993") {
		t.Fatal("retirement lost reason or original evidence")
	}
	tasks, err := store.ListProbeTasksForCase(ctx, id)
	if err != nil || len(tasks) != 0 {
		t.Fatal("retired protocol dispatched work", err)
	}
}
