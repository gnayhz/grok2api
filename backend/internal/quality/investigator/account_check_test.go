package investigator

import (
	"context"
	"fmt"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type checkMeasurements struct {
	*recordedMeasurements
	outcomes     []model.MeasurementOutcome
	wrongAccount bool
	duplicate    bool
	cancel       context.CancelFunc
}

func (m *checkMeasurements) MeasureAccountCheck(ctx context.Context, id uint64) model.AccountCheckSample {
	spec, _ := model.ProbeExperimentFromContext(ctx)
	m.task.Experiment = spec
	x := m.measure(ctx, id, 0, 0)
	n := len(m.calls)
	x.Outcome = m.outcomes[n-1]
	if m.wrongAccount {
		x.Attempt.AccountID++
	}
	if m.duplicate {
		x.Attempt.ID = "fictional-duplicate"
	}
	if m.cancel != nil {
		m.cancel()
	}
	return model.AccountCheckSample{Sample: spec.Sample, Attempt: x.Attempt, Outcome: x.Outcome, Thinking: x.Outcome == model.MeasurementClean}
}

func TestAccountCheckRequiresConsistentDistinctPinnedMeasurements(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		outcomes                 []model.MeasurementOutcome
		wrong, duplicate, cancel bool
		want                     string
	}{
		{"clean", []model.MeasurementOutcome{"clean", "clean", "clean"}, false, false, false, "clean"},
		{"missing_thinking", []model.MeasurementOutcome{"degraded", "degraded", "degraded"}, false, false, false, "degraded"},
		{"mixed", []model.MeasurementOutcome{"degraded", "clean", "degraded"}, false, false, false, "inconclusive"},
		{"transport", []model.MeasurementOutcome{"degraded", "error", "degraded"}, false, false, false, "inconclusive"},
		{"wrong_account", []model.MeasurementOutcome{"clean", "clean", "clean"}, true, false, false, "inconclusive"},
		{"replayed_identity", []model.MeasurementOutcome{"clean", "clean", "clean"}, false, true, false, "inconclusive"},
		{"cancelled", []model.MeasurementOutcome{"clean"}, false, false, true, "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := executorRegistries(t, "sqlite")
			task := executorTask(model.ProbeAccountCheck)
			task.CaseID = 0
			task.Experiment.Version = model.AccountCheckVersion
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			m := &checkMeasurements{recordedMeasurements: &recordedMeasurements{t: t, task: task}, outcomes: tc.outcomes, wrongAccount: tc.wrong, duplicate: tc.duplicate}
			if tc.cancel {
				m.cancel = cancel
			}
			result, err := NewProbeExecutor(reg, m, nil).Execute(ctx, task)
			if (err != nil) != tc.cancel {
				t.Fatal(err)
			}
			report := result.AccountCheck
			if report == nil {
				t.Fatal("missing account check report")
			}
			if report.Outcome != tc.want || len(m.calls) != len(tc.outcomes) {
				t.Fatalf("report=%+v calls=%v", report, m.calls)
			}
			for i, sample := range report.Samples {
				if !tc.duplicate && sample.Attempt.ID != fmt.Sprintf("physical/%d", i+1) {
					t.Fatal("lost physical identity")
				}
			}
		})
	}
}
