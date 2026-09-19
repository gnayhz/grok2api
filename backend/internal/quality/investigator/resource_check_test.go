package investigator

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type resourceMeasurements struct {
	calls               int
	badAccount, badNode uint64
	wrong, duplicate    bool
	cancel              context.CancelFunc
}

func (m *resourceMeasurements) MeasureResourceCheck(ctx context.Context, account, node uint64) model.ResourceSample {
	spec, _ := model.ProbeExperimentFromContext(ctx)
	m.calls++
	thinking := account != m.badAccount && node != m.badNode
	input := int64(100)
	actual := spec.Baseline
	actual.ID = fmt.Sprintf("fictional-%d", m.calls)
	actual.AccountID = account
	actual.Path = attemptmeta.Path{NodeID: node, Status: attemptmeta.PathRegistered}
	actual.Profile = spec.Profile()
	if m.wrong {
		actual.AccountID++
	}
	if m.duplicate {
		actual.ID = "fictional-duplicate"
	}
	outcome := model.MeasurementClean
	if !thinking {
		outcome = model.MeasurementDegraded
	}
	if m.cancel != nil {
		m.cancel()
	}
	return model.ResourceSample{IdentityVerified: true, PlainOutput: true, Generated: true, PathChecks: 2, Sample: spec.Sample, Attempt: actual, Outcome: outcome, Thinking: thinking, Completed: true, UsageReported: true, InputTokens: input, PathKey: fmt.Sprintf("fictional-path-%d", node), PathVerified: true, PathBinding: 1, PathFamily: 4}
}
func TestResourceCheckExecutorCrossesOneVariableAndBoundsCalls(t *testing.T) {
	for _, kind := range []string{"account", "node"} {
		for _, bad := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, bad), func(t *testing.T) {
				reg := resourceCheckRegistry(t)
				plan := &model.ResourceCheckPlan{Kind: kind, ResourceID: 7, MaxCalls: 5, Targets: []model.ResourceTarget{{Kind: kind, ResourceID: 7}}, Accounts: []uint64{101, 102, 103, 104}, Nodes: []uint64{11, 12, 13, 14}}
				task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: plan, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.6", RuleVersion: "fictional-rule"}}}
				m := &resourceMeasurements{}
				if kind == "account" {
					task.DefendantAccountID = 7
					if bad {
						m.badAccount = 7
					}
				} else {
					task.DefendantNodeID = 7
					if bad {
						m.badNode = 7
					}
				}
				e := NewProbeExecutor(reg, nil, nil)
				e.SetResourceChecks(m, &proofProgress{})
				result, err := e.Execute(context.Background(), task)
				if err != nil {
					t.Fatal(err)
				}
				want, calls := "healthy", 1
				if bad {
					want, calls = "degraded", 2
				}
				if result.ResourceCheck.Outcome != want || m.calls != calls {
					t.Fatalf("calls=%d report=%+v", m.calls, result.ResourceCheck)
				}
			})
		}
	}
}
func TestResourceCheckExecutorRejectsWrongIdentityAndCancellation(t *testing.T) {
	for _, tc := range []string{"wrong", "duplicate", "cancel", "bad-control"} {
		t.Run(tc, func(t *testing.T) {
			reg := resourceCheckRegistry(t)
			plan := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, MaxCalls: 5, Targets: []model.ResourceTarget{{Kind: "account", ResourceID: 7}}, Accounts: []uint64{101}, Nodes: []uint64{11, 12, 13, 14}}
			task := model.ProbeTask{Direction: model.ProbeResourceCheck, DefendantAccountID: 7, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: plan, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.6", RuleVersion: "fictional-rule"}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := &resourceMeasurements{badAccount: 7, wrong: tc == "wrong", duplicate: tc == "duplicate"}
			if tc == "cancel" {
				m.cancel = cancel
			}
			if tc == "bad-control" {
				m.badAccount = 7
				plan.Nodes = []uint64{11}
				m.badNode = 11
			}
			e := NewProbeExecutor(reg, nil, nil)
			e.SetResourceChecks(m, &proofProgress{})
			r, _ := e.Execute(ctx, task)
			if r.ResourceCheck.Outcome != "inconclusive" || m.calls > 5 {
				t.Fatalf("%+v", r.ResourceCheck)
			}
		})
	}
}

type proofProgress struct {
	calls int
	fail  bool
}

func (p *proofProgress) SaveResourceCheckProgress(_ context.Context, _ uint64, r model.ResourceCheckReport) error {
	p.calls++
	if p.fail {
		return errors.New("fictional persistence failure")
	}
	return nil
}
func TestResourceCheckReservesBeforeCallingUpstream(t *testing.T) {
	reg := resourceCheckRegistry(t)
	p := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, Targets: []model.ResourceTarget{{Kind: "account", ResourceID: 7}}, MaxCalls: 5, Nodes: []uint64{11}}
	task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: p, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
	m := &resourceMeasurements{}
	e := NewProbeExecutor(reg, nil, nil)
	e.SetResourceChecks(m, &proofProgress{fail: true})
	if _, err := e.Execute(context.Background(), task); err == nil || m.calls != 0 {
		t.Fatal("sent an unreserved request")
	}
}

func TestResourceCheckBatchReusesAAndHandlesBadExit(t *testing.T) {
	reg := resourceCheckRegistry(t)
	for _, badNode := range []uint64{0, 11} {
		p := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, Targets: []model.ResourceTarget{{Kind: "account", ResourceID: 7}, {Kind: "account", ResourceID: 8}}, Accounts: []uint64{101}, Nodes: []uint64{11, 12}, MaxCalls: 6}
		task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: p, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
		m := &resourceMeasurements{badAccount: 7, badNode: badNode}
		e := NewProbeExecutor(reg, nil, nil)
		e.SetResourceChecks(m, &proofProgress{})
		result, err := e.Execute(context.Background(), task)
		if err != nil || result.ResourceCheck.Results[0].Outcome != "degraded" || result.ResourceCheck.Results[1].Outcome != "healthy" || m.calls > 5 {
			t.Fatalf("badNode=%d calls=%d results=%+v err=%v", badNode, m.calls, result.ResourceCheck.Results, err)
		}
	}
}

func TestResourceCheckFourWorldsAcrossPlannerSeeds(t *testing.T) {
	reg := resourceCheckRegistry(t)
	for seed := uint64(0); seed < 40; seed++ {
		for _, badAccount := range []uint64{0, 7} {
			for _, badNode := range []uint64{0, 11} {
				p := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, Targets: []model.ResourceTarget{{Kind: "account", ResourceID: 7}, {Kind: "node", ResourceID: 11}}, Accounts: []uint64{101}, Nodes: []uint64{12}, MaxCalls: 6, Seed: seed}
				task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: p, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
				m := &resourceMeasurements{badAccount: badAccount, badNode: badNode}
				e := NewProbeExecutor(reg, nil, nil)
				e.SetResourceChecks(m, &proofProgress{})
				result, err := e.Execute(context.Background(), task)
				wants := []string{"healthy", "healthy"}
				if badAccount != 0 {
					wants[0] = "degraded"
				}
				if badNode != 0 {
					wants[1] = "degraded"
				}
				if err != nil || result.ResourceCheck.Results[0].Outcome != wants[0] || result.ResourceCheck.Results[1].Outcome != wants[1] || m.calls > 4 {
					t.Fatalf("seed=%d account=%d exit=%d calls=%d report=%+v err=%v", seed, badAccount, badNode, m.calls, result.ResourceCheck, err)
				}
			}
		}
	}
}

func resourceCheckRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r, _ := executorRegistries(t, "sqlite")
	for _, id := range []uint64{7, 11, 12, 13, 14} {
		if err := r.RecordExitIdentity(context.Background(), id, model.ExitIdentity{IPv4: fmt.Sprintf("192.0.2.%d", id)}); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestResourceCheckUnregisteredExitDoesNotSpendGeneration(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		reg := resourceCheckRegistry(t)
		p := &model.ResourceCheckPlan{Kind: "node", ResourceID: 99, Accounts: []uint64{101, 102}, Nodes: []uint64{11}, Targets: []model.ResourceTarget{{Kind: "node", ResourceID: 99}}, MaxCalls: 5}
		if mixed {
			p.Targets = append(p.Targets, model.ResourceTarget{Kind: "node", ResourceID: 11})
			p.MaxCalls = 6
		}
		task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: p, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
		m := &resourceMeasurements{}
		e := NewProbeExecutor(reg, nil, nil)
		e.SetResourceChecks(m, &proofProgress{})
		result, err := e.Execute(context.Background(), task)
		wantCalls := 0
		if mixed {
			wantCalls = 1
		}
		if err != nil || m.calls != wantCalls || result.ResourceCheck.Results[0].Outcome != "inconclusive" || result.ResourceCheck.Results[0].Reason != "path_unregistered" {
			t.Fatalf("mixed=%v calls=%d report=%+v err=%v", mixed, m.calls, result.ResourceCheck, err)
		}
		if mixed && result.ResourceCheck.Results[1].Outcome != "healthy" {
			t.Fatal("unregistered exit prevented independent valid target")
		}
	}
}

type disappearingResourceEpoch struct {
	ProbeState
	reads int
}

func (s *disappearingResourceEpoch) CurrentEpochAt(context.Context, uint64) (uint64, bool, error) {
	s.reads++
	return 0, s.reads == 1, nil
}

func TestResourceCheckLosingRegisteredExitInvalidatesResponse(t *testing.T) {
	state := &disappearingResourceEpoch{ProbeState: resourceCheckRegistry(t)}
	p := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, Targets: []model.ResourceTarget{{Kind: "account", ResourceID: 7}}, Nodes: []uint64{11}, MaxCalls: 5}
	task := model.ProbeTask{Direction: model.ProbeResourceCheck, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: p, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
	m := &resourceMeasurements{}
	e := NewProbeExecutor(state, nil, nil)
	e.SetResourceChecks(m, &proofProgress{})
	result, err := e.Execute(context.Background(), task)
	if err != nil || m.calls != 1 || result.ResourceCheck.Outcome != "inconclusive" || result.ResourceCheck.Reason != "conflicting_samples" {
		t.Fatalf("calls=%d report=%+v err=%v", m.calls, result.ResourceCheck, err)
	}
}

type failingResourceProgress struct {
	store         *registry.ProbeTaskStore
	failAt, calls int
}

func (p *failingResourceProgress) SaveResourceCheckProgress(ctx context.Context, id uint64, report model.ResourceCheckReport) error {
	p.calls++
	if p.calls == p.failAt {
		return errors.New("fictional transient write failure")
	}
	return p.store.SaveResourceCheckProgress(ctx, id, report)
}

func TestResourceCheckWriteFailureSettlesAtLastPersistedRevision(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			ctx := context.Background()
			reg := resourceCheckRegistry(t)
			store := registry.NewProbeTaskStore(reg)
			plan := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, Nodes: []uint64{11}}
			task := model.ProbeTask{Direction: model.ProbeResourceCheck, DefendantAccountID: 7, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: plan, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}}}
			submitted, err := store.CreateResourceCheckBatch(ctx, []model.ProbeTask{task}, 32)
			if err != nil {
				t.Fatal(err)
			}
			tasks, err := store.ClaimPendingProbeTasks(ctx, 1)
			if err != nil || len(tasks) != 1 {
				t.Fatalf("claim: %v %v", tasks, err)
			}
			measurements := &resourceMeasurements{}
			executor := NewProbeExecutor(reg, nil, nil)
			executor.SetResourceChecks(measurements, &failingResourceProgress{store: store, failAt: failAt})
			result, err := executor.Execute(ctx, tasks[0])
			if err == nil || measurements.calls != failAt-1 {
				t.Fatalf("continued after failed write: %d %v", measurements.calls, err)
			}
			if err := store.CompleteProbeTask(ctx, submitted[0].ID, model.ProbeFailed, result, time.Now()); err != nil {
				t.Fatalf("failure could not settle: %v", err)
			}
			rows, err := store.ListResourceChecks(ctx, "account", []uint64{7})
			if err != nil || len(rows) != 1 || rows[0].State != model.ProbeFailed || rows[0].Report.Generations != failAt-1 {
				t.Fatalf("lost completion/evidence: %+v %v", rows, err)
			}
		})
	}
}
