package court

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"reflect"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// The court freezes candidates, never their supposed health. The investigator
// uses the same adaptive planner, measurements and proof rules as manual checks.
func caseProofPolicy(defendant uint64, exit model.EpochKey, obs model.Observation, now time.Time, cfg Config) ExperimentPolicy {
	p := ExperimentPolicy{Version: model.CaseProofVersion, DeadlineAt: now.Add(cfg.InvestigationTimeout)}
	p.Experiment = model.NewProbeExperiment(obs)
	plan := &model.ResourceCheckPlan{Kind: "account", ResourceID: defendant, DeadlineAt: p.DeadlineAt, Seed: rand.Uint64() >> 1,
		Targets: []model.ResourceTarget{{Kind: "account", ResourceID: defendant}}, Accounts: []uint64{}, Nodes: []uint64{}}
	p.Experiment.ResourceCheck = plan
	if exit.NodeID != 0 {
		plan.Targets = append(plan.Targets, model.ResourceTarget{Kind: "node", ResourceID: exit.NodeID})
	}
	plan.MaxCalls = len(plan.Targets) + 4
	return p
}

func (s *Service) prepareCaseProof(ctx context.Context, defendant uint64, exit model.EpochKey, obs model.Observation, now time.Time) (ExperimentPolicy, error) {
	p := caseProofPolicy(defendant, exit, obs, now, s.Config())
	plan := p.Experiment.ResourceCheck
	if p.Experiment.UnsupportedReason() != "" {
		return p, nil
	}
	accounts, err := s.eligibleProbeAccounts(ctx, p.Experiment)
	if err != nil {
		return p, err
	}
	rand.Shuffle(len(accounts), func(i, j int) { accounts[i], accounts[j] = accounts[j], accounts[i] })
	for _, id := range accounts {
		if id != defendant && id != 0 {
			plan.Accounts = append(plan.Accounts, id)
		}
		if len(plan.Accounts) == model.ResourceCheckMaxAccounts {
			break
		}
	}
	source, err := s.nodeSource()
	if err != nil {
		return p, err
	}
	profiles, err := source.ListProfiles(ctx)
	if err != nil {
		return p, err
	}
	rand.Shuffle(len(profiles), func(i, j int) { profiles[i], profiles[j] = profiles[j], profiles[i] })
	baselineFixed := false
	for _, node := range profiles {
		fixed := node.Enabled && node.CanServeFixedTarget && !node.ProxyPool && (node.CooldownUntil == nil || !now.Before(*node.CooldownUntil))
		if node.ID == exit.NodeID {
			baselineFixed = fixed
			continue
		}
		if fixed && len(plan.Nodes) < model.ResourceCheckMaxNodes {
			plan.Nodes = append(plan.Nodes, node.ID)
		}
	}
	if exit.NodeID != 0 && !baselineFixed {
		plan.Targets[1].UnavailableReason = "path_unverified"
	}
	return p, nil
}

func assessCaseProof(tasks []model.ProbeTaskView, policy ExperimentPolicy, now time.Time) ExperimentReport {
	return assessCurrentCaseProof(tasks, policy, now, nil)
}

func assessCurrentCaseProof(tasks []model.ProbeTaskView, policy ExperimentPolicy, now time.Time, validate func(*model.ResourceCheckReport)) ExperimentReport {
	r := ExperimentReport{Policy: policy, Reason: "insufficient_controls", Phase: "ready", Limitations: []string{}}
	if policy.Version != model.CaseProofVersion || policy.Experiment.ResourceCheck == nil {
		return r
	}
	for _, task := range tasks {
		if task.Direction != model.ProbeCaseProof || !reflect.DeepEqual(task.Experiment, policy.Experiment) {
			continue
		}
		if task.State == model.ProbePending || task.State == model.ProbeRunning {
			r.Phase, r.Reason = "collecting", "awaiting_probes"
		}
		if task.ResourceCheck == nil {
			continue
		}
		proof := *task.ResourceCheck
		proof.Results = append([]model.ResourceProof{}, proof.Results...)
		proof = model.AssessResourceProofs(proof, now)
		// Old windows are useful history, not authority for a new restriction.
		for i := range proof.Results {
			p := &proof.Results[i]
			if task.State != model.ProbeDone && task.State != model.ProbeRunning {
				p.Outcome, p.Reason, p.Rule, p.Evidence = "inconclusive", "interrupted_tests", "", nil
			} else if !now.Before(p.ValidUntil) {
				p.Outcome, p.Reason, p.Rule, p.Evidence = "inconclusive", "window_expired", "", nil
			}
		}
		if validate != nil {
			validate(&proof)
		}
		r.Proof = &proof
		accountBad, exitBad := false, false
		for _, p := range proof.Results {
			if p.Outcome == "inconclusive" {
				r.Limitations = appendUnique(r.Limitations, p.Reason)
				continue
			}
			if p.Kind == "account" {
				r.AccountCleared = p.Outcome == "healthy"
				if p.Outcome == "degraded" {
					accountBad = true
				}
			} else {
				r.ExitCleared = p.Outcome == "healthy"
				if p.Outcome == "degraded" {
					exitBad = true
				}
			}
		}
		if r.Phase == "collecting" {
			return r
		}
		switch {
		case accountBad && exitBad:
			r.Verdict, r.Reason = model.VerdictBothGuilty, "proved_account_and_exit_bad"
		case accountBad:
			r.Verdict, r.Reason = model.VerdictAccountGuilty, "proved_account_bad"
		case exitBad:
			r.Verdict, r.Reason = model.VerdictExitGuilty, "proved_exit_bad"
		case r.AccountCleared && (len(proof.Results) == 1 || r.ExitCleared):
			r.Reason = "proved_normal"
		default:
			r.Reason = proof.Reason
			if model.ResourceEvidence(proof).Conflict {
				r.Reason = "conflicting_samples"
			}
		}
		return r
	}
	return r
}

func (s *Service) advanceCaseProof(ctx context.Context, record model.CaseRecord, policy ExperimentPolicy, now time.Time) (model.Verdict, int, error) {
	parties, err := s.registry.ListParties(ctx, record.ID)
	if err != nil {
		return "", 0, err
	}
	tasks, err := s.probes.ListProbeTasksForCase(ctx, record.ID)
	if err != nil {
		return "", 0, err
	}
	defendant, baseline := caseDefendant(parties), caseBaseline(record, parties)
	expired := !now.Before(policy.DeadlineAt)
	if len(tasks) == 0 && !expired && policy.Experiment.ResourceCheck != nil && policy.Experiment.ResourceCheck.UnavailableReason == "candidates_unavailable" {
		prepared, err := s.prepareCaseProof(ctx, defendant, baseline, model.Observation{EventID: policy.Experiment.TriggerEventID, Attempt: policy.Experiment.Baseline}, record.OpenedAt)
		if err != nil {
			return "", 0, err
		}
		prepared.DeadlineAt, prepared.Experiment.ResourceCheck.DeadlineAt = policy.DeadlineAt, policy.DeadlineAt
		var envelope map[string]any
		if err := decodeEvidence(record.EvidenceJSON, &envelope); err != nil {
			return "", 0, err
		}
		envelope["policy"] = prepared
		raw, err := json.Marshal(envelope)
		if err != nil {
			return "", 0, err
		}
		if err := s.registry.UpdateInvestigationEvidence(ctx, record.ID, string(raw)); err != nil {
			return "", 0, err
		}
		policy = prepared
	}
	if len(tasks) == 0 && !expired && policy.Experiment.UnsupportedReason() == "" && s.dispatcher != nil {
		n, err := s.dispatcher.DispatchForCase(ctx, DispatchSpec{CaseID: record.ID, Defendant: defendant, BaselineExit: baseline})
		return "", n, err
	}
	if expired {
		for i := range tasks {
			if tasks[i].State == model.ProbePending || tasks[i].State == model.ProbeRunning {
				tasks[i].State = model.ProbeCancelled
			}
		}
	}
	s.mu.RLock()
	check := s.proofCurrent
	s.mu.RUnlock()
	var validationErr error
	r := assessCurrentCaseProof(tasks, policy, now, func(proof *model.ResourceCheckReport) {
		if check == nil {
			return
		}
		checked := map[int]string{}
		observations := map[int]model.ResourceObservation{}
		for _, o := range proof.Observations {
			observations[o.ID] = o
		}
		for i := range proof.Results {
			p := &proof.Results[i]
			for _, id := range p.Evidence {
				reason, done := checked[id]
				if !done {
					o, exists := observations[id]
					group, _ := s.registry.IdentityGroupOf(o.AccountID)
					if group == 0 {
						group = o.AccountID
					}
					switch {
					case !exists || group != o.IdentityGroup:
						reason = "identity_changed"
					case s.registry.CurrentEpoch(o.NodeID) != o.Sample.Attempt.Path.Epoch:
						reason = "path_changed"
					default:
						factsCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
						current, err := check(factsCtx, o.Sample)
						cancel()
						if err != nil {
							validationErr, reason = err, "measurement_unavailable"
						} else if !current {
							reason = "resource_changed"
						}
					}
					checked[id] = reason
				}
				if reason != "" {
					p.Outcome, p.Reason, p.Rule, p.Evidence = "inconclusive", reason, "", nil
					break
				}
			}
		}
	})
	if validationErr != nil && !expired {
		return "", 0, validationErr
	}
	if r.AccountCleared || r.ExitCleared {
		var envelope map[string]any
		_ = decodeEvidence(record.EvidenceJSON, &envelope)
		if envelope == nil {
			envelope = map[string]any{}
		}
		releases, _ := envelope["early_releases"].(map[string]any)
		if releases == nil {
			releases = map[string]any{}
		}
		for _, p := range parties {
			if p.Disposition == model.DispositionRemanded && (p.Kind == model.PartyAccount && r.AccountCleared || p.Kind == model.PartyExit && r.ExitCleared) {
				releases[string(p.Kind)] = map[string]any{"at": now, "reason": "proved_normal", "protocol": model.CaseProofVersion}
			}
		}
		envelope["early_releases"] = releases
		raw, err := json.Marshal(envelope)
		if err != nil {
			return "", 0, err
		}
		held := !s.registry.AccountEligible(defendant)
		if err := s.registry.ReleaseClearedParties(ctx, record.ID, r.AccountCleared, r.ExitCleared, string(raw), now); err != nil {
			return "", 0, err
		}
		if held && s.registry.AccountEligible(defendant) {
			s.notifyAccountReleased(ctx, defendant)
		}
		record.EvidenceJSON = string(raw)
	}
	if r.Phase == "collecting" && !expired {
		return "", 0, nil
	}
	if r.Verdict == model.VerdictNone {
		r.Verdict = model.VerdictInsufficient
	}
	if r.Verdict.RestrictsAccount() && (defendant == 0 || s.accountExists != nil && !s.accountExists(ctx, defendant)) {
		r.Verdict = withoutAccount(r.Verdict)
		r.Limitations = appendUnique(r.Limitations, "account_missing")
	}
	// A case's exit is the original epoch. A later successful measurement on
	// its replacement must never justify restricting that replacement.
	banExit := false
	if r.Verdict.RestrictsExit() {
		if !s.isCurrentExitEpoch(baseline) {
			r.Verdict = withoutExit(r.Verdict)
			r.Limitations = appendUnique(r.Limitations, "baseline_epoch_changed")
		} else {
			factsCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			var found bool
			banExit, found, err = s.exitBanApplicable(factsCtx, baseline.NodeID)
			cancel()
			if err != nil && !expired {
				return "", 0, err
			}
			if err != nil || !found {
				r.Verdict = withoutExit(r.Verdict)
				r.Limitations = appendUnique(r.Limitations, "node_facts_unavailable")
			}
		}
	}
	var envelope map[string]any
	_ = decodeEvidence(record.EvidenceJSON, &envelope)
	if envelope == nil {
		envelope = map[string]any{}
	}
	r.Phase = "closed"
	envelope["assessment"], envelope["policy"], envelope["rule"], envelope["reason"] = r, policy, r.Reason, r.Reason
	envelope["closure_reason"] = "proof_finished"
	if expired {
		envelope["closure_reason"] = "deadline_reached"
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", 0, err
	}
	held := !s.registry.AccountEligible(defendant)
	if err := s.registry.SettleInvestigation(ctx, record.ID, r.Verdict, string(raw), now, banExit); err != nil {
		return "", 0, err
	}
	if held && s.registry.AccountEligible(defendant) {
		s.notifyAccountReleased(ctx, defendant)
	}
	return r.Verdict, 0, nil
}

func withoutAccount(v model.Verdict) model.Verdict {
	if v == model.VerdictBothGuilty {
		return model.VerdictExitGuilty
	}
	return model.VerdictInsufficient
}
func withoutExit(v model.Verdict) model.Verdict {
	if v == model.VerdictBothGuilty {
		return model.VerdictAccountGuilty
	}
	return model.VerdictInsufficient
}
