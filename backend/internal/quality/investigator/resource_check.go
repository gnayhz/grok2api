package investigator

import (
	"context"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type ResourceCheckMeasurements interface {
	MeasureResourceCheck(context.Context, uint64, uint64) model.AccountCheckSample
}
type ResourceCheckProgress interface {
	SaveResourceCheckProgress(context.Context, uint64, model.ResourceCheckReport) error
}

func (e *ProbeExecutor) SetResourceChecks(m ResourceCheckMeasurements, p ResourceCheckProgress) {
	e.resourceMeasurements, e.resourceProgress = m, p
}

func (e *ProbeExecutor) executeResourceCheck(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	p := task.Experiment.ResourceCheck
	r := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Outcome: "inconclusive", Reason: "insufficient_controls", Groups: []model.ResourceCheckGroup{}, Observations: []model.ResourceObservation{}, Window: 1, WindowStartedAt: time.Now().UTC()}
	finish := func(reason string, err error) (model.ProbeTaskResult, error) {
		if reason != "" {
			r.Reason = reason
			for i := range r.Results {
				if r.Results[i].Outcome == "inconclusive" && r.Results[i].UnavailableReason == "" {
					r.Results[i].Reason = reason
				}
			}
		}
		if p != nil {
			r = model.ResourceReportFor(r, p.Kind, p.ResourceID)
		}
		return model.ProbeTaskResult{ResourceCheck: &r, Outcome: model.ProbeResultError, Detail: r.Reason}, err
	}
	if p == nil || len(p.Targets) == 0 || task.CaseID != 0 || task.Experiment.Version != model.ResourceCheckVersion || task.Experiment.UnsupportedReason() != "" || e.resourceMeasurements == nil || e.resourceProgress == nil || e.state == nil {
		return finish("unsupported_experiment_version", nil)
	}
	r.Kind, r.ResourceID, r.MaxCalls = p.Kind, p.ResourceID, p.MaxCalls
	if p.MaxCalls != len(p.Targets)+4 || len(p.Targets) > 32 {
		return finish("invalid_budget", nil)
	}
	for _, t := range p.Targets {
		r.Results = append(r.Results, model.ResourceProof{ResourceTarget: t, Outcome: "inconclusive", Reason: t.UnavailableReason})
	}
	save := func() error { r.Revision++; return e.resourceProgress.SaveResourceCheckProgress(ctx, task.ID, r) }
	groupOf := func(id uint64) uint64 {
		g, _ := e.state.IdentityGroupOf(id)
		if g == 0 {
			return id
		}
		return g
	}
	for ctx.Err() == nil && r.Calls < r.MaxCalls {
		now := time.Now().UTC()
		if !now.Before(r.WindowStartedAt.Add(model.ResourceCheckWindow)) {
			r.Window++
			r.WindowStartedAt = now
		}
		identityChanged := false
		for i := range r.Results {
			proof := &r.Results[i]
			if proof.Kind != "account" || proof.Window != 0 && proof.Window != r.Window && proof.Outcome != "inconclusive" {
				continue
			}
			group := groupOf(proof.ResourceID)
			if proof.Window == r.Window && proof.IdentityGroup != 0 && proof.IdentityGroup != group {
				identityChanged = true
			}
			proof.IdentityGroup = group
		}
		if identityChanged {
			for i := range r.Results {
				if r.Results[i].Window == r.Window {
					r.Results[i].Outcome, r.Results[i].Reason, r.Results[i].Rule, r.Results[i].Evidence = "inconclusive", "identity_changed", "", []int{}
				}
			}
			return finish("identity_changed", nil)
		}
		r = model.AssessResourceProofs(r, now)
		if model.ResourceEvidence(r).Conflict {
			return finish("conflicting_samples", nil)
		}
		pair, ok := nextResourcePair(*p, r, groupOf)
		if !ok {
			for _, o := range r.Observations {
				if o.Window == r.Window && o.Sample.Failure == model.ProbeFailurePathRegistration {
					return finish("path_unregistered", nil)
				}
			}
			return finish("", nil)
		}
		g := groupOf(pair.account)
		beforeEpoch, registered, err := e.state.CurrentEpochAt(ctx, pair.node)
		if err != nil {
			return finish("measurement_unavailable", err)
		}
		r.Calls++
		o := model.ResourceObservation{ID: r.Calls, Window: r.Window, AccountID: pair.account, NodeID: pair.node, IdentityGroup: g, Purpose: pair.purpose, Class: "pending", StartedAt: now}
		r.Observations = append(r.Observations, o)
		if err := save(); err != nil {
			return finish("persistence_failed", err)
		}
		s := model.AccountCheckSample{Sample: task.Experiment.Sample, Outcome: model.MeasurementError, Failure: model.ProbeFailurePathRegistration}
		// A new node may await the independent epoch observer. No generation
		// can become admissible yet; preserve the missing prerequisite and skip
		// this node for the rest of the window instead of spending controls.
		if registered {
			s = e.resourceMeasurements.MeasureResourceCheck(e.identityContext(model.WithProbeExperiment(ctx, task.Experiment), pair.account), pair.account, pair.node)
		}
		o.FinishedAt = time.Now().UTC()
		afterEpoch, _, epochErr := e.state.CurrentEpochAt(ctx, pair.node)
		if g != groupOf(pair.account) || epochErr != nil || beforeEpoch != afterEpoch || s.Attempt.ID != "" && (s.Attempt.Path.Epoch != beforeEpoch || s.Attempt.AccountID != pair.account || s.Attempt.Path.NodeID != pair.node || !task.Experiment.Matches(s.Attempt)) {
			s.Conflict = s.Conflict || g != groupOf(pair.account) || epochErr == nil && beforeEpoch != afterEpoch
			s.IdentityVerified = false
			s.Outcome = model.MeasurementError
			s.Failure = model.ProbeFailureIdentity
			if s.Attempt.ID != "" && !task.Experiment.Matches(s.Attempt) {
				s.Failure = model.ProbeFailureExperiment
			}
		}
		o.Sample = s
		o.Class = model.ClassifyResourceSample(s)
		if !o.FinishedAt.Before(r.WindowStartedAt.Add(model.ResourceCheckWindow)) {
			o.Class = "unknown"
			// Keep certificates completed before this call as closed history.
			// Neither the late measurement nor old anchors enter the new window.
			r.Window++
			r.WindowStartedAt = o.FinishedAt
		}
		r.Observations[len(r.Observations)-1] = o
		if s.Generated {
			r.Generations++
		}
		r.PathChecks += s.PathChecks
		r = model.AssessResourceProofs(r, o.FinishedAt)
		if err := save(); err != nil {
			return finish("persistence_failed", err)
		}
		if model.ResourceEvidence(r).Conflict {
			return finish("conflicting_samples", nil)
		}
		if epochErr != nil {
			return finish("measurement_unavailable", epochErr)
		}
		if s.Failure == model.ProbeFailureExperiment {
			return finish("measurement_unavailable", nil)
		}
	}
	if ctx.Err() != nil {
		return finish("interrupted", ctx.Err())
	}
	return finish("budget_exhausted", nil)
}
