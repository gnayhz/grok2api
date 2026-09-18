package investigator

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type accountCheckMeasurements interface {
	MeasureAccountCheck(context.Context, uint64) model.AccountCheckSample
}

func (e *ProbeExecutor) executeAccountCheck(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	measurements, ok := e.measurements.(accountCheckMeasurements)
	if !ok || e.state == nil || task.CaseID != 0 || task.DefendantAccountID == 0 || task.Experiment.Version != model.AccountCheckVersion || task.Experiment.UnsupportedReason() != "" {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailureExperiment, "account_check_unavailable"), nil
	}
	samples := make([]model.AccountCheckSample, 0, model.AccountCheckSampleCount)
	for index := range model.AccountCheckSampleCount {
		if ctx.Err() != nil {
			break
		}
		spec := task.Experiment
		if index == 1 {
			spec.Sample = "repeated-as"
		}
		measureCtx := model.WithProbeExperiment(ctx, spec)
		sample := measurements.MeasureAccountCheck(e.identityContext(measureCtx, task.DefendantAccountID), task.DefendantAccountID)
		if sample.Outcome == model.MeasurementClean || sample.Outcome == model.MeasurementDegraded {
			if sample.Attempt.ID == "" || sample.Attempt.AccountID != task.DefendantAccountID || !spec.Matches(sample.Attempt) {
				sample.Outcome, sample.Failure = model.MeasurementError, model.ProbeFailureIdentity
			}
			for _, previous := range samples {
				if previous.Attempt.ID != "" && previous.Attempt.ID == sample.Attempt.ID {
					sample.Outcome, sample.Failure = model.MeasurementError, model.ProbeFailureIdentity
				}
			}
		}
		samples = append(samples, sample)
	}
	report := model.AssessAccountCheck(samples)
	result := model.ProbeTaskResult{Outcome: model.ProbeResultError, Detail: report.Reason, AccountCheck: &report}
	if report.Outcome == "clean" {
		result.Outcome = model.ProbeResultClean
	}
	if report.Outcome == "degraded" {
		result.Outcome = model.ProbeResultDegraded
	}
	return result, ctx.Err()
}
