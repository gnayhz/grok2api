package investigator

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type resourceMeasurements struct {
	calls               int
	badAccount, badNode uint64
	wrong, duplicate    bool
	cancel              context.CancelFunc
}

func (m *resourceMeasurements) MeasureResourceCheck(ctx context.Context, account, node uint64) model.AccountCheckSample {
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
	return model.AccountCheckSample{IdentityVerified: true, PlainOutput: true, Generated: true, PathChecks: 2, Sample: spec.Sample, Attempt: actual, Outcome: outcome, Thinking: thinking, Completed: true, UsageReported: true, InputTokens: input, PathKey: fmt.Sprintf("fictional-path-%d", node), PathVerified: true, PathBinding: 1, PathFamily: 4}
}
func TestResourceCheckExecutorCrossesOneVariableAndBoundsCalls(t *testing.T) {
	for _, kind := range []string{"account", "node"} {
		for _, bad := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, bad), func(t *testing.T) {
				reg, _ := executorRegistries(t, "sqlite")
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
			reg, _ := executorRegistries(t, "sqlite")
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
	reg, _ := executorRegistries(t, "sqlite")
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
	reg, _ := executorRegistries(t, "sqlite")
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
	reg, _ := executorRegistries(t, "sqlite")
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
