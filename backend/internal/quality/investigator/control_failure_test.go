package investigator

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"testing"
)

func TestExecutorSkipsUnusableControlsButPreservesAvailabilityComparison(t *testing.T) {
	for _, tc := range []struct {
		kind  model.ProbeFailure
		calls int
	}{
		{model.ProbeFailureCapacity, 1}, {model.ProbeFailureIdentity, 1}, {model.ProbeFailurePersistence, 1}, {model.ProbeFailureExperiment, 1},
		{model.ProbeFailureCreatedTimeout, 2}, {model.ProbeFailureHTTPServer, 2},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			r, _ := executorRegistries(t, "sqlite")
			task := executorTask(model.ProbeAccountDifferential)
			calls := &recordedMeasurements{t: t, task: task, change: func(n int, v *model.ProbeMeasurement) {
				if n == 1 {
					v.Outcome = model.MeasurementError
					v.Failure = tc.kind
				}
			}}
			result, err := NewProbeExecutor(r, calls, nil).Execute(context.Background(), task)
			if err != nil || len(calls.calls) != tc.calls || result.FailureKind != string(tc.kind) || result.Outcome != model.ProbeResultError {
				t.Fatalf("calls=%d result=%+v err=%v", len(calls.calls), result, err)
			}
		})
	}
}
