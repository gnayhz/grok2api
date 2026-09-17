package court

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func TestCourtNodeTypeReadFailureDefersExitDisposition(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if err := b.registry.DB().Exec("UPDATE egress_nodes SET proxy_pool = 1 WHERE id = 3").Error; err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	t.Cleanup(func() { _ = s.Close(ctx) })
	id := openSimpleTestCase(t, s, b.registry)
	settleSimpleTestTasks(t, b.registry, id, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultClean}
	})
	if err := b.registry.DB().Exec("ALTER TABLE egress_nodes RENAME TO e12_unavailable_nodes").Error; err != nil {
		t.Fatal(err)
	}
	restored := false
	restore := func() {
		if !restored {
			if err := b.registry.DB().Exec("ALTER TABLE e12_unavailable_nodes RENAME TO egress_nodes").Error; err != nil {
				t.Error(err)
			}
			restored = true
		}
	}
	t.Cleanup(restore)
	if _, err := s.Evaluate(ctx, time.Now()); err == nil {
		t.Error("node type read failure was swallowed")
	}
	record, found, err := b.registry.GetCase(ctx, id)
	if err != nil || !found {
		t.Fatalf("case missing: %v", err)
	}
	if record.Status.Closed() || b.registry.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
		t.Errorf("unknown node type caused terminal disposition: status=%s exit=%s", record.Status, b.registry.ExitStateOfCurrentEpoch(3).State)
	}
	restore()
	if _, err := s.Evaluate(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	record, _, err = b.registry.GetCase(ctx, id)
	if err != nil || record.Verdict != model.VerdictExitGuilty || !record.Status.Closed() || b.registry.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
		t.Fatalf("restored pool facts not respected: %+v state=%s err=%v", record, b.registry.ExitStateOfCurrentEpoch(3).State, err)
	}
}

func TestCourtNodeCandidatesReadFailureDoesNotCloseEmptyCase(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, nil)
	t.Cleanup(func() { _ = s.Close(ctx) })
	id := openSimpleTestCase(t, s, b.registry)
	s.dispatcher = simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)}
	s.SetProbeAccounts(probeAccountsFunc(func(context.Context, model.ProbeExperiment) ([]uint64, error) { return nil, nil }))
	if err := b.registry.DB().Exec("ALTER TABLE egress_nodes RENAME TO e12_unavailable_nodes").Error; err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.registry.DB().Exec("ALTER TABLE e12_unavailable_nodes RENAME TO egress_nodes").Error })
	if _, err := s.Evaluate(ctx, time.Now()); err == nil {
		t.Error("node candidate read failure was swallowed")
	}
	record, _, err := b.registry.GetCase(ctx, id)
	if err != nil || record.Status.Closed() {
		t.Fatalf("unreadable candidates were treated as exhausted: status=%s err=%v", record.Status, err)
	}
}

func TestCourtObservedCandidateRespectsCurrentNodeState(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.AccountNeedExits = 8
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, nil)
	t.Cleanup(func() { _ = s.Close(ctx) })
	if err := b.registry.DB().Exec("UPDATE egress_nodes SET enabled = 0 WHERE id = 1").Error; err != nil {
		t.Fatal(err)
	}
	// Include only the stale observation. Fleet top-up must not mask the fact
	// that this exact disabled node was selected from the historical window.
	spec, err := s.dispatchSpecFor(ctx, 1, 7, model.EpochKey{NodeID: 3}, model.Estimate{Exits: map[model.EpochKey]model.SubjectEstimate{{NodeID: 1}: {}}}, policyFor(cfg, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range spec.HealthyExits {
		if key.NodeID == 1 {
			t.Fatal("historical healthy observation bypassed administrator disabled state")
		}
	}
}

func TestCourtNodeFailurePreservesIndependentRelease(t *testing.T) {
	for _, scenario := range []string{"deadline_incomplete", "deadline_exit_guilty", "account_guilty", "exit_deleted", "epoch_changed", "node_source_missing"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			b := newBench(t)
			cfg := DefaultConfig()
			cfg.EvaluateEvery = time.Hour
			s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
			t.Cleanup(func() { _ = s.Close(ctx) })
			id := openSimpleTestCase(t, s, b.registry)
			if scenario != "deadline_incomplete" {
				settleSimpleTestTasks(t, b.registry, id, func(task model.ProbeTaskView) model.ProbeTaskResult {
					degraded := task.Direction == model.ProbeExitJury
					if scenario == "account_guilty" {
						degraded = !degraded
					}
					if degraded {
						return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, VerifiedIPChange: true}
					}
					return model.ProbeTaskResult{Outcome: model.ProbeResultClean}
				})
			}
			switch scenario {
			case "exit_deleted":
				if err := b.registry.DB().Exec("DELETE FROM egress_nodes WHERE id = 3").Error; err != nil {
					t.Fatal(err)
				}
			case "node_source_missing":
				s.SetNodes(nil)
			default:
				if scenario == "epoch_changed" {
					if _, _, err := b.registry.AdvanceEpoch(ctx, 3, model.ExitIdentityFromAggregate("192.0.2.9")); err != nil {
						t.Fatal(err)
					}
				}
				if err := b.registry.DB().Exec("ALTER TABLE egress_nodes RENAME TO e12_unavailable_nodes").Error; err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = b.registry.DB().Exec("ALTER TABLE e12_unavailable_nodes RENAME TO egress_nodes").Error })
			}
			now := time.Now()
			if strings.HasPrefix(scenario, "deadline_") {
				now = now.Add(24 * time.Hour)
			}
			_, err := s.Evaluate(ctx, now)
			wantError := scenario == "deadline_exit_guilty" || scenario == "node_source_missing"
			if (err != nil) != wantError {
				t.Fatalf("evaluation error=%v wantError=%t", err, wantError)
			}
			record, found, err := b.registry.GetCase(ctx, id)
			if err != nil || !found {
				t.Fatal(err)
			}
			if scenario == "node_source_missing" {
				if record.Status.Closed() || b.registry.ExitStateOfCurrentEpoch(3).State != model.ExitRemanded {
					t.Fatal("missing source committed a disposition")
				}
				return
			}
			if !record.Status.Closed() || b.registry.ExitStateOfCurrentEpoch(3).State != model.ExitAvailable {
				t.Fatalf("independent release blocked: %+v exit=%s", record, b.registry.ExitStateOfCurrentEpoch(3).State)
			}
			wantVerdict := model.VerdictInsufficient
			if scenario == "account_guilty" {
				wantVerdict = model.VerdictAccountGuilty
			}
			if scenario == "epoch_changed" {
				wantVerdict = model.VerdictExitGuilty
			}
			if record.Verdict != wantVerdict {
				t.Fatalf("verdict=%s want=%s", record.Verdict, wantVerdict)
			}
			if b.registry.AccountEligible(7) == (scenario == "account_guilty") {
				t.Fatal("independent account disposition changed")
			}
			if scenario == "deadline_exit_guilty" && !strings.Contains(record.EvidenceJSON, "node_facts_unavailable") {
				t.Fatal("missing type limitation lost")
			}
			if scenario == "exit_deleted" && !strings.Contains(record.EvidenceJSON, "baseline_node_missing") {
				t.Fatal("deleted node reason lost")
			}
		})
	}
}

type blockedNodeProfile struct{ proxy.NodeSource }

func (blockedNodeProfile) Profile(ctx context.Context, _ uint64) (proxy.NodeProfile, bool, error) {
	<-ctx.Done()
	return proxy.NodeProfile{}, false, ctx.Err()
}

func TestCourtExpiredNodeReadLeavesTimeForRelease(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	t.Cleanup(func() { _ = s.Close(ctx) })
	id := openSimpleTestCase(t, s, b.registry)
	settleSimpleTestTasks(t, b.registry, id, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultClean}
	})
	s.SetNodes(blockedNodeProfile{fixtureNodes{b.registry}})
	passCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	stats, err := s.Evaluate(passCtx, time.Now().Add(24*time.Hour))
	if !errors.Is(err, context.DeadlineExceeded) || passCtx.Err() != nil || stats.Dismissed != 1 {
		t.Fatalf("node read consumed release budget/result: %+v %v pass=%v", stats, err, passCtx.Err())
	}
	record, _, err := b.registry.GetCase(ctx, id)
	if err != nil || record.Verdict != model.VerdictInsufficient || !b.registry.AccountEligible(7) || b.registry.ExitStateOfCurrentEpoch(3).State != model.ExitAvailable {
		t.Fatalf("blocked type source orphaned restriction: %+v %v", record, err)
	}
}
