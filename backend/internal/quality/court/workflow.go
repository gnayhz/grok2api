package court

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
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
	_ = decodeEvidence(record.EvidenceJSON, &prior)
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
	// The party list is what identifies the accounts this revocation may have
	// freed; it is read before the settle because afterwards the case's own
	// disposition no longer distinguishes "freed by this call" from "already
	// free".
	parties, err := s.registry.ListParties(ctx, id)
	if err != nil {
		return err
	}
	// Record the custody state before settling: only an account that was
	// detained and became eligible counts as freed by this action. Without
	// the before/after comparison an account already released by an earlier
	// disposition would be re-reported and could clear quality marks set
	// after that release.
	heldBefore := make(map[uint64]bool, len(parties))
	for _, p := range parties {
		if p.Kind == model.PartyAccount && !s.registry.AccountEligible(p.AccountID) {
			heldBefore[p.AccountID] = true
		}
	}
	if err := s.registry.SettleInvestigation(ctx, id, model.VerdictInsufficient, string(raw), time.Now().UTC(), true, true); err != nil {
		return err
	}
	// Report every account this revocation actually freed. An account still
	// detained by another case stays ineligible and is not reported.
	for _, p := range parties {
		if p.Kind == model.PartyAccount && heldBefore[p.AccountID] && s.registry.AccountEligible(p.AccountID) {
			s.notifyAccountReleased(ctx, p.AccountID)
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
	var expired, active []model.CaseRecord
	for _, record := range cases {
		if !now.Before(casePolicy(record, cfg).DeadlineAt) {
			expired = append(expired, record)
		} else {
			active = append(active, record)
		}
	}
	// Deadline closure runs before reconciliation, new discovery or replacement
	// planning. Each group resumes after the last attempted case across replicas.
	for group, records := range [][]model.CaseRecord{rotateCases(expired, expiredCursor), rotateCases(active, activeCursor)} {
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
	discoveryCtx, cancel := context.WithTimeout(ctx, evaluationCaseBudget(ctx, 1))
	n, err = s.openCasesForDegraded(discoveryCtx, snapshot, now)
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

func rotateCases(records []model.CaseRecord, cursor uint64) []model.CaseRecord {
	for i, record := range records {
		if record.ID > cursor {
			return append(append(make([]model.CaseRecord, 0, len(records)), records[i:]...), records[:i]...)
		}
	}
	return records
}

// Unsupported investigations cannot keep running a retired decision protocol.
// Preserve their evidence and close without attributing a resource fault.
func (s *Service) advanceExperiment(ctx context.Context, record model.CaseRecord, now time.Time, cfg Config) (model.Verdict, int, error) {
	policy := casePolicy(record, cfg)
	if policy.Version == model.CaseProofVersion {
		return s.advanceCaseProof(ctx, record, policy, now)
	}
	parties, err := s.registry.ListParties(ctx, record.ID)
	if err != nil {
		return "", 0, err
	}
	var envelope map[string]any
	if decodeEvidence(record.EvidenceJSON, &envelope) != nil || envelope == nil {
		envelope = map[string]any{"previous_evidence": record.EvidenceJSON}
	}
	envelope["closure_reason"], envelope["reason"] = "protocol_retired", "unsupported_protocol"
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", 0, err
	}
	defendant := caseDefendant(parties)
	held := !s.registry.AccountEligible(defendant)
	if err := s.registry.SettleInvestigation(ctx, record.ID, model.VerdictInsufficient, string(raw), now, false); err != nil {
		return "", 0, err
	}
	if held && s.registry.AccountEligible(defendant) {
		s.notifyAccountReleased(ctx, defendant)
	}
	return model.VerdictInsufficient, 0, nil
}
