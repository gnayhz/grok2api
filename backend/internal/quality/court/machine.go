package court

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// LiveCaseView is the read-only projection of a pending finite investigation
// lifecycle, including valid evidence, failed paths and replacement budget.
type LiveCaseView struct {
	Assessment *ExperimentReport `json:"assessment,omitempty"`
	CaseID     uint64            `json:"case_id"`
	Defendant  uint64            `json:"defendant"`

	PendingProbes     int       `json:"pending_probes"`
	WaitingReasonCode string    `json:"waiting_reason_code,omitempty"`
	DeadlineAt        time.Time `json:"deadline_at"`
	Expired           bool      `json:"expired"`
}

var ErrReadCaseParties = errors.New("read case parties")

// LiveCaseViews projects the same finite task rows consumed by the court.
func (s *Service) LiveCaseViews(ctx context.Context, now time.Time) ([]LiveCaseView, error) {
	if s == nil || s.registry == nil {
		return nil, nil
	}
	if s.probes == nil {
		// 与 registry/evidence 的惰性关闭约定一致:裸构造(仅测试)
		// 没有任务读取面时返回空视图,而不是 nil deref。
		return nil, nil
	}
	cases, err := s.registry.ListOpenCases(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.Config()
	taskStore := s.probes
	views := make([]LiveCaseView, 0, len(cases))
	for _, record := range cases {
		parties, err := s.registry.ListParties(ctx, record.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: case %d: %w", ErrReadCaseParties, record.ID, err)
		}
		policy := casePolicy(record, cfg)
		view := LiveCaseView{CaseID: record.ID}
		if !record.OpenedAt.IsZero() {
			view.DeadlineAt = policy.DeadlineAt
			view.Expired = !now.Before(view.DeadlineAt)
		}
		for _, party := range parties {
			if party.Kind == model.PartyAccount && party.Role == model.RoleDefendant {
				view.Defendant = party.AccountID
				break
			}
		}
		tasks, err := taskStore.ListProbeTasksForCase(ctx, record.ID)
		if err != nil {
			return nil, err
		}
		report := assessCaseProof(tasks, policy, now)
		view.Assessment = &report
		for _, task := range tasks {
			if task.State == model.ProbePending || task.State == model.ProbeRunning {
				view.PendingProbes++
			}
		}
		if view.Expired {
			view.WaitingReasonCode = "deadline_reached"
		} else if view.PendingProbes > 0 {
			view.WaitingReasonCode = "awaiting_probes"
		} else {
			view.WaitingReasonCode = "probes_settled"
		}
		views = append(views, view)
	}
	return views, nil
}

// EvalStats reports one evaluator pass.
type EvalStats struct {
	Opened    int
	Withdrawn int
	Convicted int
	Dismissed int
	Retried   int
}

// Evaluate runs the one direct loop. Opening and settlement are serialized
// with ReportDegraded so request-side and periodic discovery cannot race.
func (s *Service) Evaluate(ctx context.Context, now time.Time) (EvalStats, error) {
	if s == nil {
		return EvalStats{}, nil
	}
	if err := s.evaluateMu.Lock(ctx); err != nil {
		return EvalStats{}, err
	}
	defer s.evaluateMu.Unlock()
	if s.registry != nil {
		coordinated, release, err := s.registry.Coordinate(ctx, "court")
		if err != nil {
			return EvalStats{}, err
		}
		defer release()
		ctx = coordinated
		if err := s.registry.RefreshState(ctx); err != nil {
			return EvalStats{}, err
		}
	}

	s.mu.RLock()
	registryStore, source, cfg := s.registry, s.evidence, s.cfg
	s.mu.RUnlock()
	if registryStore == nil || source == nil {
		return EvalStats{}, nil
	}
	return s.advanceInvestigations(ctx, source, now, cfg)
}

// openCasesForDegraded consumes only traffic-source degradation. Probe
// observations are evidence about a defendant, never new incidents.
func (s *Service) openCasesForDegraded(ctx context.Context, snapshot model.Snapshot, now time.Time) (opened int, err error) {
	pairs := snapshot.TrafficDegradedPairs()
	incidents := make([]model.IncidentKey, 0, len(pairs))
	for _, traffic := range pairs {
		incidents = append(incidents, model.IncidentKey{AccountID: traffic.AccountID, Exit: traffic.Exit})
	}
	lastClosed, err := s.registry.LastClosedAtForIncidents(ctx, incidents)
	if err != nil {
		return 0, err
	}
	for _, traffic := range pairs {
		accountID, exit := traffic.AccountID, traffic.Exit
		if exit.NodeID != 0 && s.registry.CurrentEpoch(exit.NodeID) != exit.Epoch {
			continue
		}
		if closedAt, found := lastClosed[model.IncidentKey{AccountID: accountID, Exit: exit}]; found && !traffic.LastAt.After(closedAt) {
			continue
		}
		if s.accountExists != nil && !s.accountExists(ctx, accountID) {
			continue
		}
		caseID, err := s.registry.OpenCaseForIncident(ctx, accountID, exit)
		if err != nil {
			return opened, err
		}
		if caseID == 0 {
			if _, err := s.openCase(ctx, accountID, exit, now, traffic.Observation); err != nil {
				return opened, err
			}
			opened++
			continue
		}
		// OpenCaseForIncident only returns a case whose exact baseline exit is
		// still a live party. This is the idempotent repeat-event path; a new
		// exit for the same account has already taken the new-case branch above.
		if err := s.reassertExistingHolds(ctx, caseID, accountID, exit); err != nil {
			return opened, err
		}
	}
	return opened, nil
}

// openCase creates the case, freezes both parties, and dispatches the finite
// proof task. Candidate preparation can be retried within the fixed deadline.
func (s *Service) openCase(ctx context.Context, defendant uint64, exit model.EpochKey, now time.Time, observations ...model.Observation) (uint64, error) {
	evidenceJSON, err := marshalOpeningEvidence(defendant, exit)
	if err != nil {
		return 0, err
	}
	var opening map[string]any
	_ = decodeEvidence(evidenceJSON, &opening)
	var obs model.Observation
	if len(observations) > 0 {
		obs = observations[0]
	}
	policy := caseProofPolicy(defendant, exit, obs, now, s.Config())
	policy.Experiment.ResourceCheck.UnavailableReason = "candidates_unavailable"
	var preparationErr error
	opening["policy"] = policy
	raw, _ := json.Marshal(opening)
	caseID, err := s.registry.OpenInvestigation(ctx, defendant, exit, now, string(raw))
	if err != nil {
		return caseID, err
	}
	{
		prepared, err := s.prepareCaseProof(ctx, defendant, exit, obs, now)
		preparationErr = err
		if err == nil {
			prepared.DeadlineAt, prepared.Experiment.ResourceCheck.DeadlineAt = policy.DeadlineAt, policy.DeadlineAt
			policy = prepared
			opening["policy"] = policy
			raw, err = json.Marshal(opening)
			if err != nil {
				return caseID, err
			}
			if err := s.registry.UpdateInvestigationEvidence(ctx, caseID, string(raw)); err != nil {
				return caseID, err
			}
		}
	}
	if exit.NodeID != 0 && s.ledgerSink != nil {
		if err := s.ledgerSink.RecordDegradeEvent(ctx, exit.NodeID, exit.Epoch, now); err != nil {
			s.logger.Warn("court_ledger_write_failed", "case", caseID, "error", err)
		}
	}
	if preparationErr == nil && s.dispatcher != nil && (policy.Experiment.Version == "" || policy.Experiment.UnsupportedReason() == "") {
		spec := DispatchSpec{CaseID: caseID, Defendant: defendant, BaselineExit: exit}
		if dispatched, err := s.dispatcher.DispatchForCase(ctx, spec); err != nil {
			s.logger.Warn("court_dispatch_failed", "case", caseID, "error", err.Error())
		} else if dispatched == 0 {
			s.logger.Warn("court_probe_round_empty", "case", caseID)
		}
	}
	s.logger.Info("court_case_opened", "case", caseID, "account", defendant,
		"node", exit.NodeID, "epoch", exit.Epoch)
	return caseID, preparationErr
}
