package court

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestFilterIdentityGroupJurorsCountsOneSSOIdentityOnce(t *testing.T) {
	// This deliberately general graph tests the independence rule; real
	// Web/Build/Console links are one-to-one and contain at most one Build.
	bench := newBenchWithEvidenceConfig(t, qualitymodel.DefaultEvidenceConfig(), identityLinksStub{
		{AccountID: 100, RelatedAccountID: 7}, {AccountID: 100, RelatedAccountID: 8},
		{AccountID: 100, RelatedAccountID: 9}, {AccountID: 200, RelatedAccountID: 10},
		{AccountID: 200, RelatedAccountID: 11},
	})

	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := newTestCourt(bench, cfg)
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	jurors := service.filterIdentityGroupJurors(7, []uint64{8, 9, 10, 11, 1})
	if len(jurors) != 2 {
		t.Fatalf("same SSO identity must contribute one juror per group, got %v", jurors)
	}
	seen := map[uint64]bool{}
	for _, juror := range jurors {
		seen[juror] = true
	}
	if !seen[10] && !seen[11] {
		t.Fatalf("second SSO group should retain one juror, got %v", jurors)
	}
	if !seen[1] {
		t.Fatalf("unlinked juror should remain eligible, got %v", jurors)
	}
	if seen[8] || seen[9] {
		t.Fatalf("defendant identity group must be excluded entirely, got %v", jurors)
	}
}

type identityLinksStub []account.IdentityLink

func (links identityLinksStub) ListIdentityLinks(context.Context) ([]account.IdentityLink, error) {
	return links, nil
}

// requireHealthyExits compares the planned comparison exits as a set: the
// dispatcher shuffles them on purpose, so only membership is deterministic.
func requireHealthyExits(t *testing.T, exits []qualitymodel.EpochKey, want ...uint64) {
	t.Helper()
	got := make(map[uint64]int, len(exits))
	for _, exit := range exits {
		got[exit.NodeID]++
	}
	expected := make(map[uint64]int, len(want))
	for _, nodeID := range want {
		expected[nodeID]++
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("comparison exits = %v, want nodes %v", exits, want)
	}
}

// TestCourtKnownSameExitCandidatesAreSkippedBeforeTheComparisonCap 固定建议性
// 同出口排除缝的语义:已知与 baseline 共享真实出口的候选在封顶与洗牌之前被
// 跳过(有限比对额度花在可用候选上),nil 缝表示"无信息"、什么都不排除,
// 全部被排除时保持今天的行为——没有比对路径,而不是回填已被排除的候选。
func TestCourtKnownSameExitCandidatesAreSkippedBeforeTheComparisonCap(t *testing.T) {
	ctx := context.Background()
	bench := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	// AccountPaths = max(2, AccountNeedExits): with two slots, exactly two
	// usable candidates survive any cap; an unusable candidate must not
	// consume a slot.
	cfg.AccountNeedExits = 2
	service := newTestCourt(bench, cfg)
	t.Cleanup(func() { _ = service.Close(ctx) })
	baseline := qualitymodel.EpochKey{NodeID: 3, Epoch: bench.registry.CurrentEpoch(3)}
	policy := policyFor(cfg, time.Now())
	plan := func(nodes map[uint64]bool) DispatchSpec {
		return service.dispatchSpecFromCandidates(ctx, 1, 7, baseline, qualitymodel.Estimate{}, policy, nil, nodes)
	}
	// The baseline node is enabled too: it must never become a comparison
	// target, resolved or not.
	twoUsable := map[uint64]bool{1: true, 2: true, 3: true}

	// Without any seam, and with an explicitly nil one, nothing is excluded:
	// the plan keeps today's behaviour.
	requireHealthyExits(t, plan(twoUsable).HealthyExits, 1, 2)
	service.SetSameExit(nil)
	requireHealthyExits(t, plan(twoUsable).HealthyExits, 1, 2)
	// A seam that answers "unknown" everywhere must exclude nothing either.
	service.SetSameExit(func(context.Context, uint64, uint64) bool { return false })
	requireHealthyExits(t, plan(twoUsable).HealthyExits, 1, 2)

	// Node 1 is KNOWN to share the baseline's real egress: it is dropped.
	var queried []uint64
	service.SetSameExit(func(_ context.Context, baselineNodeID, candidateNodeID uint64) bool {
		if baselineNodeID != baseline.NodeID {
			t.Errorf("seam queried with %d as baseline, want %d", baselineNodeID, baseline.NodeID)
		}
		queried = append(queried, candidateNodeID)
		return candidateNodeID == 1
	})
	requireHealthyExits(t, plan(twoUsable).HealthyExits, 2)
	if len(queried) == 0 {
		t.Fatal("advisory same-exit seam was never consulted")
	}

	// Three candidates compete for two slots and one of them is known-same.
	// Both slots must go to the two usable candidates: the filter runs before
	// the cap and the shuffle. (Repeating the shuffle-sensitive plan makes a
	// post-cap filter fail rather than pass by chance.)
	threeCandidates := map[uint64]bool{1: true, 2: true, 3: true, 4: true}
	for range 4 {
		requireHealthyExits(t, plan(threeCandidates).HealthyExits, 2, 4)
	}

	// The same rule holds on the real planning entry over the whole enabled
	// fleet: when one node is the only exit not known to share the baseline,
	// the plan carries exactly that node instead of spending its slots on
	// excluded ones.
	service.SetSameExit(func(_ context.Context, _ uint64, candidateNodeID uint64) bool {
		return candidateNodeID != 1
	})
	spec, err := service.dispatchSpecFor(ctx, 1, 7, baseline, qualitymodel.Estimate{}, policy)
	if err != nil {
		t.Fatal(err)
	}
	requireHealthyExits(t, spec.HealthyExits, 1)

	// Every candidate excluded: the plan stays empty exactly like a case whose
	// traffic window has no healthy exit, and dropped candidates never return.
	service.SetSameExit(func(context.Context, uint64, uint64) bool { return true })
	for _, nodes := range []map[uint64]bool{twoUsable, threeCandidates} {
		if exits := plan(nodes).HealthyExits; len(exits) != 0 {
			t.Fatalf("excluded candidates were re-admitted: %v", exits)
		}
	}
}

// exitRecordingDispatcher records replacement plans without touching storage.
type exitRecordingDispatcher struct{ specs []DispatchSpec }

func (d *exitRecordingDispatcher) DispatchForCase(_ context.Context, spec DispatchSpec) (int, error) {
	d.specs = append(d.specs, spec)
	return len(spec.HealthyExits), nil
}

// TestCourtReplacementSkipsKnownSameExitCandidates 固定补派轮次的同一规则:
// 补派候选若已知与 baseline 共享真实出口,不占用有限的补派额度;nil 缝仍然
// 什么都不排除。
func TestCourtReplacementSkipsKnownSameExitCandidates(t *testing.T) {
	ctx := context.Background()
	bench := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.AccountNeedExits = 2
	dispatch := &exitRecordingDispatcher{}
	service := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, dispatch)
	t.Cleanup(func() { _ = service.Close(ctx) })
	baseline := qualitymodel.EpochKey{NodeID: 3, Epoch: bench.registry.CurrentEpoch(3)}
	record := qualitymodel.CaseRecord{ID: 1, OpenedAt: time.Now().UTC()}
	parties := []qualitymodel.PartyRecord{
		{Kind: qualitymodel.PartyExit, NodeID: baseline.NodeID, Epoch: baseline.Epoch},
		{Kind: qualitymodel.PartyAccount, Role: qualitymodel.RoleDefendant, AccountID: 7},
	}
	// Three attempts already spent, one replacement slot left.
	summary := planningProgress{DiffTasks: 3}
	replace := func() DispatchSpec {
		before := len(dispatch.specs)
		if _, err := service.replaceAccountComparisons(ctx, record, parties, nil, summary, cfg, &replacementCandidates{}); err != nil {
			t.Fatal(err)
		}
		if len(dispatch.specs) != before+1 {
			t.Fatalf("replacement rounds dispatched = %d, want one", len(dispatch.specs)-before)
		}
		return dispatch.specs[len(dispatch.specs)-1]
	}

	// Without the seam the lowest-ID unimplicated candidate is used.
	requireHealthyExits(t, replace().HealthyExits, 1)
	// Nodes other than 5 are known-same: the remaining slot goes to node 5.
	service.SetSameExit(func(_ context.Context, _ uint64, candidateNodeID uint64) bool {
		return candidateNodeID != 5
	})
	requireHealthyExits(t, replace().HealthyExits, 5)
	// Every candidate known-same: no replacement is dispatched at all, which
	// keeps the bounded replacement budget out of a probe that cannot count.
	service.SetSameExit(func(context.Context, uint64, uint64) bool { return true })
	if _, err := service.replaceAccountComparisons(ctx, record, parties, nil, summary, cfg, &replacementCandidates{}); err != nil {
		t.Fatal(err)
	}
	if got := len(dispatch.specs); got != 2 {
		t.Fatalf("excluded candidates still consumed a replacement slot: specs=%d", got)
	}
}
