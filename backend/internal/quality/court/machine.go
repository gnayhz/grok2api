package court

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// LiveCaseView is the read-only projection of a pending finite investigation
// lifecycle, including valid evidence, failed paths and replacement budget.
type LiveCaseView struct {
	Assessment *ExperimentReport `json:"assessment,omitempty"`
	CaseID     uint64            `json:"case_id"`
	Defendant  uint64            `json:"defendant"`

	AccountDegradedExits int `json:"account_degraded_exits"`
	AccountNeedExits     int `json:"account_need_exits"`
	AccountSpanNodes     int `json:"account_span_nodes"`
	AccountNeedSpanNodes int `json:"account_need_span_nodes"`

	ExitWitnesses int `json:"exit_witnesses"`
	ExitNeedN     int `json:"exit_need_n"`
	ExitDegraded  int `json:"exit_degraded"`
	ExitNeedK     int `json:"exit_need_k"`

	AccountScore                float64 `json:"account_score,omitempty"`
	ExitScore                   float64 `json:"exit_score,omitempty"`
	AccountTransportSupport     float64 `json:"account_transport_support,omitempty"`
	AccountConfidence           float64 `json:"account_confidence,omitempty"`
	ExitConfidence              float64 `json:"exit_confidence,omitempty"`
	ConfidenceThreshold         float64 `json:"confidence_threshold,omitempty"`
	UncertaintyPenalty          float64 `json:"uncertainty_penalty,omitempty"`
	DifferentialValid           int     `json:"differential_valid"`
	DifferentialFailed          int     `json:"differential_failed"`
	DifferentialTransportFailed int     `json:"differential_transport_failed"`
	DifferentialAttempts        int     `json:"differential_attempts"`
	DifferentialAttemptLimit    int     `json:"differential_attempt_limit"`
	JuryFailed                  int     `json:"jury_failed"`

	PendingProbes int `json:"pending_probes"`
	// WaitingReason is a human-readable line kept for backward compatibility;
	// WaitingReasonCode is the stable machine vocabulary the panel translates
	// (the backend text is single-language and must not leak into other
	// locales).
	WaitingReason     string    `json:"waiting_reason"`
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
	cases, err := s.registry.ListOpenCases(ctx)
	if err != nil {
		return nil, err
	}
	cfg := s.Config()
	taskStore := registry.NewProbeTaskStore(s.registry)
	views := make([]LiveCaseView, 0, len(cases))
	for _, record := range cases {
		parties, err := s.registry.ListParties(ctx, record.ID)
		if err != nil {
			return nil, fmt.Errorf("%w: case %d: %w", ErrReadCaseParties, record.ID, err)
		}
		policy := casePolicy(record, cfg)
		view := LiveCaseView{
			CaseID: record.ID, AccountNeedExits: policy.AccountPaths,
			AccountNeedSpanNodes: policy.AccountNodes, ExitNeedN: policy.JurySize, ExitNeedK: policy.JuryDegraded,
		}
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
		report := assessExperiment(tasks, policy)
		summary := planningProgressFor(report)
		view.AccountDegradedExits = summary.DiffDegraded
		view.AccountSpanNodes = summary.DiffSpanNodes
		view.ExitWitnesses = summary.JuryTotal
		view.ExitDegraded = summary.JuryDegraded
		view.Assessment = &report
		view.DifferentialValid = summary.DiffClean + summary.DiffDegraded
		view.DifferentialFailed = summary.DiffFailed
		view.DifferentialTransportFailed = summary.DiffTransportFailed
		view.DifferentialAttempts = summary.DiffTasks
		view.DifferentialAttemptLimit = policy.MaxAccountAttempts
		view.JuryFailed = summary.JuryFailed
		for _, task := range tasks {
			if task.State == model.ProbePending || task.State == model.ProbeRunning {
				view.PendingProbes++
			}
		}
		if view.Expired {
			view.WaitingReason = "调查期限已到，未完成探针将被取消并收口"
			view.WaitingReasonCode = "deadline_reached"
		} else if view.PendingProbes > 0 {
			view.WaitingReason = fmt.Sprintf("等待本轮取证探针结论(%d 个在飞)", view.PendingProbes)
			view.WaitingReasonCode = "awaiting_probes"
		} else if summary.DiffClean+summary.DiffDegraded < policy.AccountPaths &&
			summary.DiffTasks < policy.MaxAccountAttempts {
			if summary.DiffFailed > 0 {
				view.WaitingReason = fmt.Sprintf("已排除 %d 个差分失败出口,等待补充未测试出口", summary.DiffFailed)
				view.WaitingReasonCode = "replacing_failed_exits"
			} else {
				view.WaitingReason = fmt.Sprintf("有效差分不足(%d/%d),等待补充未测试出口", summary.DiffClean+summary.DiffDegraded, policy.AccountPaths)
				view.WaitingReasonCode = "awaiting_supplement"
			}
		} else {
			view.WaitingReason = "对照测试已结束，等待按本案规则结案"
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
func (s *Service) openCasesForDegraded(ctx context.Context, snapshot evidence.Snapshot, estimate evidence.Estimate, now time.Time) (opened int, err error) {
	pairs := snapshot.TrafficDegradedPairs()
	incidents := make([]registry.IncidentKey, 0, len(pairs))
	for _, traffic := range pairs {
		incidents = append(incidents, registry.IncidentKey{AccountID: traffic.AccountID, Exit: traffic.Exit})
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
		if closedAt, found := lastClosed[registry.IncidentKey{AccountID: accountID, Exit: exit}]; found && !traffic.LastAt.After(closedAt) {
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
			if _, err := s.openCase(ctx, accountID, exit, estimate, now, traffic.Observation); err != nil {
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
// jury/differential core round. The court may later append bounded replacements
// for terminal paths that produced no admissible evidence.
func (s *Service) openCase(ctx context.Context, defendant uint64, exit model.EpochKey, estimate evidence.Estimate, now time.Time, observations ...model.Observation) (uint64, error) {
	evidenceJSON, err := marshalOpeningEvidence(defendant, exit, estimate)
	if err != nil {
		return 0, err
	}
	var opening map[string]any
	_ = json.Unmarshal([]byte(evidenceJSON), &opening)
	policy := policyFor(s.Config(), now)
	if len(observations) > 0 {
		policy.Experiment = model.NewProbeExperiment(observations[0])
	}
	opening["policy"] = policy
	raw, _ := json.Marshal(opening)
	caseID, err := s.registry.OpenInvestigation(ctx, defendant, exit, now, string(raw))
	if err != nil {
		return caseID, err
	}
	if exit.NodeID != 0 && s.ledgerSink != nil {
		if err := s.ledgerSink.RecordDegradeEvent(ctx, exit.NodeID, exit.Epoch, now); err != nil {
			s.logger.Warn("court_ledger_write_failed", "case", caseID, "error", err)
		}
	}
	if s.dispatcher != nil && (policy.Experiment.Version == "" || policy.Experiment.UnsupportedReason() == "") {
		spec, err := s.dispatchSpecFor(ctx, caseID, defendant, exit, estimate, policy)
		if err != nil {
			return caseID, err
		}
		if dispatched, err := s.dispatcher.DispatchForCase(ctx, spec); err != nil {
			s.logger.Warn("court_dispatch_failed", "case", caseID, "error", err.Error())
		} else if dispatched == 0 {
			s.logger.Warn("court_probe_round_empty", "case", caseID)
		}
	}
	s.logger.Info("court_case_opened", "case", caseID, "account", defendant,
		"node", exit.NodeID, "epoch", exit.Epoch)
	return caseID, nil
}
