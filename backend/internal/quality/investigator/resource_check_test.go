package investigator

import (
	"context"
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
	input, delta := int64(100), int64(180)
	if !thinking {
		input, delta = 110, 90
	}
	if spec.Sample == "token-long" {
		input += delta
	}
	actual := spec.Baseline
	actual.ID = fmt.Sprintf("fictional-%d", m.calls)
	actual.AccountID = account
	actual.Path = attemptmeta.Path{NodeID: node, Epoch: 1}
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
	return model.AccountCheckSample{Sample: spec.Sample, Attempt: actual, Outcome: outcome, Thinking: thinking, Completed: true, UsageReported: true, InputTokens: input, PathKey: fmt.Sprintf("fictional-path-%d", node), PathVerified: true, PathBinding: 1}
}
func TestResourceCheckExecutorCrossesOneVariableAndBoundsCalls(t *testing.T) {
	for _, kind := range []string{"account", "node"} {
		for _, bad := range []bool{false, true} {
			t.Run(fmt.Sprint(kind, bad), func(t *testing.T) {
				reg, _ := executorRegistries(t, "sqlite")
				plan := &model.ResourceCheckPlan{Kind: kind, ResourceID: 7, Accounts: []uint64{101, 102, 103, 104}, Nodes: []uint64{11, 12, 13, 14}}
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
				e.SetResourceChecks(m, nil)
				result, err := e.Execute(context.Background(), task)
				if err != nil {
					t.Fatal(err)
				}
				want, calls := "healthy", 14
				if bad {
					want, calls = "degraded", 21
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
			plan := &model.ResourceCheckPlan{Kind: "account", ResourceID: 7, Accounts: []uint64{101}, Nodes: []uint64{11, 12, 13, 14}}
			task := model.ProbeTask{Direction: model.ProbeResourceCheck, DefendantAccountID: 7, Experiment: model.ProbeExperiment{Version: model.ResourceCheckVersion, Sample: "token-short", ResourceCheck: plan, Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.6", RuleVersion: "fictional-rule"}}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := &resourceMeasurements{wrong: tc == "wrong", duplicate: tc == "duplicate"}
			if tc == "cancel" {
				m.cancel = cancel
			}
			if tc == "bad-control" {
				m.badAccount = 101
			}
			e := NewProbeExecutor(reg, nil, nil)
			e.SetResourceChecks(m, nil)
			r, _ := e.Execute(ctx, task)
			if r.ResourceCheck.Outcome != "inconclusive" || m.calls > model.ResourceCheckMaxCalls {
				t.Fatalf("%+v", r.ResourceCheck)
			}
		})
	}
}
