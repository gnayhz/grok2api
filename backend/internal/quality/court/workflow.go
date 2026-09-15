package court

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// ReleaseAfterReview records the operator's reason and the complete previous
// decision before revoking this case's holds. Other cases remain authoritative.
func (s *Service) ReleaseAfterReview(ctx context.Context, id uint64, reason string) error {
	reason = strings.TrimSpace(reason)
	if len(reason) < 3 || len(reason) > 500 {
		return fmt.Errorf("review reason must contain 3 to 500 bytes")
	}
	if err := s.evaluateMu.Lock(ctx); err != nil {
		return err
	}
	defer s.evaluateMu.Unlock()
	if s.registry != nil {
		coordinated, release, err := s.registry.Coordinate(ctx, "court")
		if err != nil {
			return err
		}
		defer release()
		ctx = coordinated
		if err := s.registry.RefreshState(ctx); err != nil {
			return err
		}
	}

	record, found, err := s.registry.GetCase(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("case not found")
	}
	var prior map[string]any
	_ = json.Unmarshal([]byte(record.EvidenceJSON), &prior)
	if prior["manual_review"] != nil {
		return nil
	}
	payload := map[string]any{"manual_review": map[string]any{"reason": reason, "at": time.Now().UTC(), "previous_verdict": record.Verdict, "previous_evidence": prior}, "rule": "manual_release", "reason": "manual_release"}
	if prior != nil {
		payload["policy"] = prior["policy"]
		payload["trigger"] = prior["trigger"]
		payload["exit"] = prior["exit"]
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	parties, err := s.registry.ListParties(ctx, id)
	if err != nil {
		return err
	}
	if err := s.registry.SettleInvestigation(ctx, id, model.VerdictInsufficient, string(raw), time.Now().UTC(), true, true); err != nil {
		return err
	}
	for _, p := range parties {
		if p.Kind == model.PartyAccount && s.registry.AccountEligible(p.AccountID) {
			s.mu.RLock()
			hook := s.releaseHook
			s.mu.RUnlock()
			if hook != nil {
				hook(ctx, p.AccountID)
			}
		}
	}
	return nil
}

// advanceInvestigations orchestrates one finite experiment. A case failure is
// isolated: it must not starve the deadline or closure of later cases.
func (s *Service) advanceInvestigations(ctx context.Context, source EvidenceSource, now time.Time, cfg Config) (EvalStats, error) {
	stats := EvalStats{}
	var errs []error
	cases, err := s.registry.ListOpenCases(ctx)
	if err != nil {
		return stats, err
	}
	expiredCursor, activeCursor, err := s.registry.EvaluationCursors(ctx)
	if err != nil {
		return stats, err
	}
	var expired, active []registry.CaseRecord
	for _, record := range cases {
		if !now.Before(casePolicy(record, cfg).DeadlineAt) {
			expired = append(expired, record)
		} else {
			active = append(active, record)
		}
	}
	// Deadline closure runs before reconciliation, new discovery or replacement
	// planning. Each group resumes after the last attempted case across replicas.
	for group, records := range [][]registry.CaseRecord{rotateCases(expired, expiredCursor), rotateCases(active, activeCursor)} {
		for i, record := range records {
			if ctx.Err() != nil {
				return stats, errors.Join(append(errs, ctx.Err())...)
			}
			budget := evaluationCaseBudget(ctx, len(records)-i)
			if group == 1 {
				budget = activeCaseBudget(ctx, len(records)-i)
			}
			caseCtx, cancel := context.WithTimeout(ctx, budget)
			err := s.registry.AdvanceEvaluationCursor(caseCtx, record.ID, group == 0)
			var verdict model.Verdict
			var retried int
			if err == nil {
				verdict, retried, err = s.advanceExperiment(caseCtx, record, now, cfg)
			}
			cancel()
			stats.Retried += retried
			if verdict == model.VerdictInsufficient {
				stats.Dismissed++
			} else if verdict != model.VerdictNone {
				stats.Convicted++
			}
			if err != nil {
				errs = append(errs, err)
				s.logger.Warn("quality_case_advance_failed", "case", record.ID, "error", err)
				continue
			}
		}
	}
	maintenanceCtx, cancel := context.WithTimeout(ctx, evaluationCaseBudget(ctx, 2))
	n, err := s.registry.ReconcileStaleExitParties(maintenanceCtx)
	cancel()
	stats.Withdrawn = n
	if err != nil {
		errs = append(errs, err)
	}
	if ctx.Err() != nil {
		return stats, errors.Join(append(errs, ctx.Err())...)
	}
	// Traffic aggregation serves discovery only. Expired cases close from their
	// persisted experiments before this independent in-memory work can delay them.
	snapshot := source.SnapshotWindow(now)
	estimate := source.CrossValidate(snapshot)
	discoveryCtx, cancel := context.WithTimeout(ctx, evaluationCaseBudget(ctx, 1))
	n, err = s.openCasesForDegraded(discoveryCtx, snapshot, estimate, now)
	cancel()
	stats.Opened = n
	if err != nil {
		errs = append(errs, err)
	}
	return stats, errors.Join(errs...)
}

// Keep enough of the shared pass budget for other cases, with at most one
// second spent on any case. Persistent rotation handles more work than a pass
// can finish; shrinking the caller budget does not disable fault isolation.
func evaluationCaseBudget(ctx context.Context, remaining int) time.Duration {
	return boundedCaseBudget(ctx, remaining, time.Second)
}

// Candidate reads for active investigations may cover a large fleet. They
// have a separate finite budget after expired cases have had their turn.
func activeCaseBudget(ctx context.Context, remaining int) time.Duration {
	return boundedCaseBudget(ctx, remaining, 5*time.Second)
}

func boundedCaseBudget(ctx context.Context, remaining int, budget time.Duration) time.Duration {
	if deadline, ok := ctx.Deadline(); ok {
		share := time.Until(deadline) / time.Duration(min(remaining, 4)+1)
		if share < budget {
			budget = share
		}
	}
	return max(budget, time.Nanosecond)
}

func rotateCases(records []registry.CaseRecord, cursor uint64) []registry.CaseRecord {
	for i, record := range records {
		if record.ID > cursor {
			return append(append(make([]registry.CaseRecord, 0, len(records)), records[i:]...), records[:i]...)
		}
	}
	return records
}

func (s *Service) advanceExperiment(ctx context.Context, record registry.CaseRecord, now time.Time, cfg Config) (model.Verdict, int, error) {
	policy := casePolicy(record, cfg)
	// Upgrade old open cases once. Later settings changes cannot extend an
	// existing hold or move the evidence thresholds underneath an experiment.
	var envelope map[string]any
	_ = json.Unmarshal([]byte(record.EvidenceJSON), &envelope)
	if envelope == nil {
		envelope = map[string]any{}
	}
	if envelope["policy"] == nil {
		envelope["policy"] = policy
		envelope["migrated_from"] = "legacy_open_case"
		raw, err := json.Marshal(envelope)
		if err != nil {
			return "", 0, err
		}
		if err := s.registry.UpdateInvestigationEvidence(ctx, record.ID, string(raw)); err != nil {
			return "", 0, err
		}
		record.EvidenceJSON = string(raw)
	}
	parties, err := s.registry.ListParties(ctx, record.ID)
	if err != nil {
		return "", 0, err
	}
	store := registry.NewProbeTaskStore(s.registry)
	tasks, err := store.ListProbeTasksForCase(ctx, record.ID)
	if err != nil {
		return "", 0, err
	}
	report := assessExperiment(tasks, policy)
	expired := !now.Before(policy.DeadlineAt)
	closure := "evidence_complete"
	defendant := caseDefendant(parties)
	if defendant == 0 || (s.accountExists != nil && !s.accountExists(ctx, defendant)) {
		closure = "investigation_unavailable"
		report.Verdict = model.VerdictInsufficient
		report.Reason = "account_missing"
	} else if policy.Experiment.Version != "" && policy.Experiment.UnsupportedReason() != "" {
		closure = "experiment_unsupported"
		report.Verdict = model.VerdictInsufficient
		report.Reason = policy.Experiment.UnsupportedReason()
		report.Limitations = appendUnique(report.Limitations, report.Reason)
	} else if policy.Version != ProtocolVersion {
		closure = "experiment_unsupported"
		report.Verdict = model.VerdictInsufficient
		report.Reason = "unsupported_protocol"
	} else {
		// Commit each completed clean group before considering unfinished work.
		if report.AccountCleared || report.ExitCleared {
			releases, _ := envelope["early_releases"].(map[string]any)
			if releases == nil {
				releases = map[string]any{}
			}
			for _, p := range parties {
				if p.Disposition != model.DispositionRemanded {
					continue
				}
				if (p.Kind == model.PartyAccount && report.AccountCleared) || (p.Kind == model.PartyExit && report.ExitCleared) {
					key := string(p.Kind)
					if releases[key] == nil {
						releases[key] = map[string]any{"at": now, "reason": key + "_controls_clean", "protocol": "clearance-v1"}
					}
				}
			}
			envelope["early_releases"] = releases
			raw, err := json.Marshal(envelope)
			if err != nil {
				return "", 0, err
			}
			wasHeld := !s.registry.AccountEligible(defendant)
			if err := s.registry.ReleaseClearedParties(ctx, record.ID, report.AccountCleared, report.ExitCleared, string(raw), now); err != nil {
				return "", 0, err
			}
			if wasHeld && s.registry.AccountEligible(defendant) {
				s.notifyAccountRelease(ctx, defendant)
			}
		}
		if report.Account.Pending+report.Exit.Pending > 0 && !expired {
			return "", 0, nil
		}
		if expired {
			// Evaluate only completed measurements. Cancellation is committed
			// with the verdict; it never becomes a failed quality measurement.
			for i := range tasks {
				if tasks[i].State == model.ProbePending || tasks[i].State == model.ProbeRunning {
					tasks[i].State = model.ProbeCancelled
				}
			}
			report = assessExperiment(tasks, policy)
			closure = "deadline_reached"
		}
		if report.Verdict == model.VerdictNone && !expired {
			caseCfg := cfg
			caseCfg.AccountNeedExits = policy.AccountPaths
			caseCfg.AccountSpanNodes = policy.AccountNodes
			caseCfg.ExitNeedN = policy.JurySize
			caseCfg.ExitNeedK = policy.JuryDegraded
			n, err := s.replaceMissingComparisons(ctx, record, parties, tasks, planningProgressFor(report), caseCfg)
			if err != nil || n > 0 {
				return "", n, err
			}
			closure = "candidates_exhausted"
		}
		if report.Verdict == model.VerdictNone {
			report.Verdict = model.VerdictInsufficient
		}
	}
	baseline := caseBaseline(record, parties)
	banExit := false
	var factErr error
	if report.Verdict == model.VerdictExitGuilty && s.isCurrentExitEpoch(baseline) {
		var found bool
		factCtx := ctx
		cancelFacts := func() {}
		if expired {
			// Reserve time for the independent quality write. A blocked node
			// source must not consume every expired case's entire pass budget.
			budget := 250 * time.Millisecond
			if deadline, ok := ctx.Deadline(); ok {
				budget = min(budget, max(time.Until(deadline)/2, time.Nanosecond))
			}
			factCtx, cancelFacts = context.WithTimeout(ctx, budget)
		}
		banExit, found, factErr = s.exitBanApplicable(factCtx, baseline.NodeID)
		cancelFacts()
		if factErr != nil && !expired {
			return "", 0, factErr
		}
		if factErr != nil || !found {
			// The finite experiment still closes at its deadline. Keep the
			// measurements but release this case's holds when the exit type
			// cannot be established; never invent a ban or orphan a hold.
			report.Verdict = model.VerdictInsufficient
			report.Reason = "baseline_node_missing"
			if factErr != nil {
				report.Reason = "node_facts_unavailable"
			}
			report.Limitations = appendUnique(report.Limitations, report.Reason)
		}
	}
	report.Phase = "closed"
	envelope["assessment"] = report
	envelope["policy"] = policy
	envelope["closure_reason"] = closure
	envelope["rule"] = report.Reason
	envelope["reason"] = report.Reason
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", 0, err
	}
	wasHeld := s.registry.AccountState(defendant).State == model.AccountRemanded
	err = s.registry.SettleInvestigation(ctx, record.ID, report.Verdict, string(raw), now, banExit)
	if err != nil {
		return "", 0, err
	}
	if wasHeld && s.registry.AccountEligible(defendant) {
		s.mu.RLock()
		hook := s.releaseHook
		s.mu.RUnlock()
		if hook != nil {
			hook(ctx, defendant)
		}
	}
	return report.Verdict, 0, factErr
}

func (s *Service) withControls(ctx context.Context, spec DispatchSpec, baseline model.EpochKey, policy ExperimentPolicy, candidates *replacementCandidates) (DispatchSpec, error) {
	if !s.isCurrentExitEpoch(baseline) {
		return spec, nil
	}
	snapshot := s.evidence.SnapshotWindow(time.Now().UTC())
	accounts, err := candidates.accounts(ctx, s, policy.Experiment)
	if err != nil {
		return spec, err
	}
	nodes, err := candidates.nodes(ctx, s)
	if err != nil {
		return spec, err
	}
	controls := s.dispatchSpecFromCandidates(spec.CaseID, spec.Defendant, baseline, s.evidence.CrossValidate(snapshot), policy, accounts, nodes)
	spec.ControlAccounts = controls.Jurors
	spec.ControlExits = controls.HealthyExits
	return spec, nil
}

func (s *Service) notifyAccountRelease(ctx context.Context, defendant uint64) {
	s.mu.RLock()
	hook := s.releaseHook
	s.mu.RUnlock()
	if hook != nil {
		hook(ctx, defendant)
	}
}
