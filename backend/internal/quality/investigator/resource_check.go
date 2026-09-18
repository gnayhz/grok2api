package investigator

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type ResourceCheckMeasurements interface {
	MeasureResourceCheck(context.Context, uint64, uint64) model.AccountCheckSample
}
type ResourceCheckProgress interface {
	SaveResourceCheckProgress(context.Context, uint64, model.ResourceCheckReport) error
}

func (e *ProbeExecutor) SetResourceChecks(measure ResourceCheckMeasurements, progress ResourceCheckProgress) {
	e.resourceMeasurements, e.resourceProgress = measure, progress
}

func (e *ProbeExecutor) executeResourceCheck(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	plan := task.Experiment.ResourceCheck
	report := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Outcome: "inconclusive", Reason: "insufficient_controls", MaxCalls: model.ResourceCheckMaxCalls, Groups: []model.ResourceCheckGroup{}}
	finish := func(err error) (model.ProbeTaskResult, error) {
		report = model.AssessResourceCheck(report)
		result := model.ProbeTaskResult{ResourceCheck: &report, Outcome: model.ProbeResultError, Detail: report.Reason}
		if report.Outcome == "healthy" {
			result.Outcome = model.ProbeResultClean
		}
		if report.Outcome == "degraded" {
			result.Outcome = model.ProbeResultDegraded
		}
		return result, err
	}
	if plan == nil || task.CaseID != 0 || task.Experiment.Version != model.ResourceCheckVersion || task.Experiment.UnsupportedReason() != "" || e.resourceMeasurements == nil || e.state == nil {
		return finish(nil)
	}
	report.Kind, report.ResourceID = plan.Kind, plan.ResourceID
	if plan.UnavailableReason == "path_unverified" {
		report.Reason = plan.UnavailableReason
		return finish(nil)
	}
	if plan.Kind != "account" && plan.Kind != "node" {
		return finish(nil)
	}
	if plan.Kind == "account" && task.DefendantAccountID != plan.ResourceID || plan.Kind == "node" && task.DefendantNodeID != plan.ResourceID {
		return finish(nil)
	}
	groupOf := func(id uint64) uint64 {
		group, _ := e.state.IdentityGroupOf(id)
		if group == 0 {
			return id
		}
		return group
	}
	accounts := []uint64{}
	seenGroups := map[uint64]bool{}
	if plan.Kind == "account" {
		seenGroups[groupOf(plan.ResourceID)] = true
	}
	for _, id := range plan.Accounts {
		group := groupOf(id)
		if id != 0 && !seenGroups[group] {
			accounts = append(accounts, id)
			seenGroups[group] = true
		}
	}
	if len(accounts) == 0 || len(plan.Nodes) == 0 {
		return finish(nil)
	}
	seenAttempts := map[string]bool{}
	var writeErr error
	measure := func(account, node uint64, sampleName string) model.AccountCheckSample {
		spec := task.Experiment
		spec.Sample = sampleName
		sample := e.resourceMeasurements.MeasureResourceCheck(e.identityContext(model.WithProbeExperiment(ctx, spec), account), account, node)
		report.Calls++
		if (sample.Outcome == model.MeasurementClean || sample.Outcome == model.MeasurementDegraded) && (sample.Sample != sampleName || sample.Attempt.ID == "" || sample.Attempt.AccountID != account || sample.Attempt.Path.NodeID != node || !spec.Matches(sample.Attempt) || seenAttempts[sample.Attempt.ID]) {
			sample.Outcome, sample.Failure = model.MeasurementError, model.ProbeFailureIdentity
		}
		if sample.Attempt.ID != "" {
			seenAttempts[sample.Attempt.ID] = true
		}
		return sample
	}
	save := func() {
		if e.resourceProgress != nil && writeErr == nil {
			writeErr = e.resourceProgress.SaveResourceCheckProgress(ctx, task.ID, model.AssessResourceCheck(report))
		}
	}
	for index := 0; index < model.ResourceCheckMaxGroups && report.Calls < model.ResourceCheckMaxCalls && ctx.Err() == nil && writeErr == nil; index++ {
		if plan.Kind == "account" && index >= len(plan.Nodes) || plan.Kind == "node" && index >= len(accounts) {
			break
		}
		controlAccount := accounts[index%len(accounts)]
		controlNode := plan.Nodes[index%len(plan.Nodes)]
		g := model.ResourceCheckGroup{ControlAccount: controlAccount, ControlNode: controlNode, AccountID: controlAccount, NodeID: controlNode, IdentityGroup: groupOf(controlAccount), Control: []model.AccountCheckSample{}, Samples: []model.AccountCheckSample{}, Outcome: "inconclusive", Reason: "control_unavailable"}
		if plan.Kind == "account" {
			g.AccountID = plan.ResourceID
		} else {
			g.NodeID = plan.ResourceID
		}
		report.Groups = append(report.Groups, g)
		pos := len(report.Groups) - 1
		for _, name := range []string{"token-short", "token-long", "token-short"} {
			if ctx.Err() != nil || writeErr != nil {
				break
			}
			g.Control = append(g.Control, measure(controlAccount, controlNode, name))
			report.Groups[pos] = g
			save()
			if g.Control[len(g.Control)-1].Outcome != model.MeasurementClean {
				break
			}
		}
		_, class, valid := model.ResourceFingerprint(g.Control)
		if !valid || class != "A" {
			continue
		}
		for _, name := range []string{"token-short", "token-long", "token-short"} {
			if ctx.Err() != nil || writeErr != nil {
				break
			}
			g.Samples = append(g.Samples, measure(g.AccountID, g.NodeID, name))
			report.Groups[pos] = g
			save()
			if g.Samples[len(g.Samples)-1].Outcome == model.MeasurementError {
				break
			}
		}
		if ctx.Err() == nil && writeErr == nil {
			s := measure(controlAccount, controlNode, "token-short")
			g.After = &s
		}
		g = model.AssessResourceGroup(plan.Kind, g)
		if g.IdentityGroup != groupOf(controlAccount) || plan.Kind == "account" && groupOf(controlAccount) == groupOf(plan.ResourceID) {
			g.Outcome, g.Reason = "inconclusive", "identity_changed"
		}
		report.Groups[pos] = g
		report = model.AssessResourceCheck(report)
		save()
		if report.Outcome != "inconclusive" || report.Reason == "conflicting_samples" || report.Reason == "calibration_changed" {
			break
		}
	}
	if writeErr != nil {
		return finish(writeErr)
	}
	return finish(ctx.Err())
}
