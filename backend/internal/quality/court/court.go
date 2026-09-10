// Package court implements the finite quality attribution loop.
//
// A classified Build degradation is one case. The case freezes the account
// and the observed exit, dispatches one jury group on that exit and one
// differential group on other exits. Failed/inadmissible paths may receive a
// bounded replacement from an untested candidate. The case then closes with
// an exit verdict, an account verdict, or insufficient evidence. Transport
// failures never become quality votes.
package court

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// Config contains the decision thresholds for one finite investigation
// lifecycle. A lifecycle may include bounded replacement probes;
// EvaluateEvery is a process cadence, not a decision state.
type Config struct {
	EvaluateEvery time.Duration
	// InvestigationTimeout bounds one case from opening to settlement. A
	// missing probe worker or a permanently pending task must never leave an
	// account and exit remanded forever; once this deadline is reached the
	// evaluator closes using completed evidence without dispatching more work.
	InvestigationTimeout time.Duration

	// AccountNeedExits is the target number of distinct comparison paths for a
	// case. Missing controls are replaced within a finite attempt budget.
	AccountNeedExits int
	// AccountSpanNodes prevents several logical exits sharing one node from
	// being treated as independent account evidence.
	AccountSpanNodes int
	// ExitNeedN is the number of independent Build jurors required for an exit
	// verdict; ExitNeedK is the degraded majority threshold.
	ExitNeedN int
	ExitNeedK int
	// JurorCount is the number of distinct Build accounts the dispatcher may
	// use for the jury. It lets the court provide enough candidates when the
	// investigator's jury setting is raised above quorum.
	JurorCount int

	Logger *slog.Logger
}

// DefaultConfig returns the production direct-loop policy.
func DefaultConfig() Config {
	return Config{
		EvaluateEvery:        15 * time.Second,
		InvestigationTimeout: 10 * time.Minute,
		AccountNeedExits:     3,
		AccountSpanNodes:     2,
		ExitNeedN:            4,
		ExitNeedK:            3,
		JurorCount:           4,
	}
}

func (c Config) normalized() Config {
	if c.EvaluateEvery <= 0 {
		c.EvaluateEvery = 15 * time.Second
	}
	if c.InvestigationTimeout <= 0 {
		c.InvestigationTimeout = 10 * time.Minute
	}
	if c.AccountNeedExits <= 0 {
		c.AccountNeedExits = 3
	}
	if c.AccountSpanNodes <= 0 {
		c.AccountSpanNodes = 2
	}
	if c.ExitNeedN <= 0 {
		c.ExitNeedN = 4
	}
	if c.ExitNeedK <= 0 {
		c.ExitNeedK = 3
	}
	if c.JurorCount <= 0 {
		c.JurorCount = 4
	}
	if c.ExitNeedK > c.ExitNeedN {
		c.ExitNeedK = c.ExitNeedN
	}
	if c.AccountSpanNodes > c.AccountNeedExits {
		c.AccountSpanNodes = c.AccountNeedExits
	}
	return c
}

// SetConfig hot-applies decision thresholds. The evaluation cadence and
// logger are process wiring and remain fixed after construction.
func (s *Service) SetConfig(cfg Config) {
	cfg = cfg.normalized()
	s.mu.Lock()
	cfg.EvaluateEvery = s.cfg.EvaluateEvery
	cfg.Logger = s.cfg.Logger
	s.cfg = cfg
	s.mu.Unlock()
}

// Config returns the current decision policy.
func (s *Service) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Dispatcher is the investigation queue boundary. The court owns the
// dispatch specification; the investigator owns task persistence/execution.
type Dispatcher interface {
	DispatchForCase(ctx context.Context, spec DispatchSpec) (int, error)
}

// DispatchSpec describes the two probe groups for one case.
type DispatchSpec struct {
	ControlAccounts []uint64
	ControlExits    []model.EpochKey
	CaseID          uint64
	Defendant       uint64

	// BaselineExit is the exit captured with the original degraded request.
	// HealthyExits are comparison targets for the defendant account.
	BaselineExit model.EpochKey
	HealthyExits []model.EpochKey

	// CoRemandedExits is the observed exit under jury examination. In the
	// direct loop it contains at most the baseline exit.
	CoRemandedExits []model.EpochKey
	// Jurors are eligible Build accounts sampled for the jury group.
	Jurors []uint64
}

// EvidenceSource provides the traffic window used for fallback incident
// discovery and for selecting unimplicated probe targets. Probe conclusions
// are evaluated from the finite task rows, not from a rolling window.
type EvidenceSource interface {
	SnapshotWindow(now time.Time) evidence.Snapshot
	CrossValidate(snapshot evidence.Snapshot) evidence.Estimate
}

// LedgerSink records a classified degradation against the observed epoch.
type LedgerSink interface {
	RecordDegradeEvent(ctx context.Context, nodeID, epoch uint64, at time.Time) error
}

// Service is the single direct attribution loop.
type Service struct {
	cfg      Config
	registry *registry.Registry
	evidence EvidenceSource

	probeAccounts ProbeAccounts
	nodes         proxy.NodeSource
	dispatcher    Dispatcher
	ledgerSink    LedgerSink
	accountExists AccountExists
	releaseHook   AccountReleaseHook
	logger        *slog.Logger

	mu sync.RWMutex
	// evaluateMu serializes incident opening and settlement. Without this
	// boundary the request reporter and the periodic fallback
	// could observe the same degradation concurrently and create duplicate
	// cases or release a party while another goroutine is freezing it.
	evaluateMu evaluationLock

	cancel context.CancelFunc
	done   chan struct{}
}

// ProbeAccounts supplies ordinary eligibility for the frozen measurement model.
// Listing must not reserve slots, refresh credentials, or claim quota recovery.
type ProbeAccounts interface {
	QualityProbeAccounts(context.Context, model.ProbeExperiment) ([]uint64, error)
}

func (s *Service) SetProbeAccounts(source ProbeAccounts) {
	s.mu.Lock()
	s.probeAccounts = source
	s.mu.Unlock()
}

func (s *Service) eligibleProbeAccounts(ctx context.Context, experiment model.ProbeExperiment) ([]uint64, error) {
	s.mu.RLock()
	source := s.probeAccounts
	s.mu.RUnlock()
	if source == nil {
		return nil, errors.New("probe account source unavailable")
	}
	return source.QualityProbeAccounts(ctx, experiment)
}

// AccountExists checks whether the defendant still exists in the account
// store. A deleted account cannot be probed and is immediately dismissed.
type AccountExists func(ctx context.Context, accountID uint64) bool

func (s *Service) SetAccountExists(check AccountExists) {
	s.mu.Lock()
	s.accountExists = check
	s.mu.Unlock()
}

// AccountReleaseHook clears request-path cooldown after an exit verdict has
// exonerated the account.
type AccountReleaseHook func(ctx context.Context, accountID uint64)

func (s *Service) SetAccountReleaseHook(hook AccountReleaseHook) {
	s.mu.Lock()
	s.releaseHook = hook
	s.mu.Unlock()
}

// ReportDegraded is the synchronous case-opening boundary behind the
// request-path reporter. It freezes both parties and queues the finite probe
// round before returning. Its persistence and lock wait contribute to the
// interval before the next routing attempt; the upstream is already closed.
func (s *Service) ReportDegraded(ctx context.Context, accountID uint64, exit model.EpochKey) error {
	return s.ReportDegradedAt(ctx, accountID, exit, time.Now().UTC())
}

// ReportDegradedAt is the timestamp-preserving incident entry used by the
// request reporter. Keeping the original observation time
// prevents a delayed reporter from opening a duplicate case after the
// periodic fallback has already settled that same event.
func (s *Service) ReportDegradedAt(ctx context.Context, accountID uint64, exit model.EpochKey, observedAt time.Time) error {
	return s.reportDegraded(ctx, accountID, exit, observedAt)
}

func (s *Service) ReportDegradedObservation(ctx context.Context, obs model.Observation) error {
	return s.reportDegraded(ctx, obs.AccountID, obs.Exit, obs.At, obs)
}

func (s *Service) reportDegraded(ctx context.Context, accountID uint64, exit model.EpochKey, observedAt time.Time, observations ...model.Observation) error {
	if s == nil || accountID == 0 {
		return nil
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

	s.mu.RLock()
	registryStore, source, exists := s.registry, s.evidence, s.accountExists
	s.mu.RUnlock()
	if registryStore == nil || source == nil {
		return nil
	}
	if exists != nil && !exists(ctx, accountID) {
		return nil
	}
	if exit.NodeID != 0 && registryStore.CurrentEpoch(exit.NodeID) != exit.Epoch {
		return nil
	}
	caseID, err := registryStore.OpenCaseForIncident(ctx, accountID, exit)
	if err != nil {
		return err
	}
	if observedAt.IsZero() {
		observedAt = time.Now().UTC()
	}
	incident := registry.IncidentKey{AccountID: accountID, Exit: exit}
	lastClosed, err := registryStore.LastClosedAtForIncidents(ctx, []registry.IncidentKey{incident})
	if err != nil {
		return err
	}
	if closedAt, found := lastClosed[incident]; found && !observedAt.After(closedAt) {
		return nil
	}
	now := time.Now().UTC()
	snapshot := source.SnapshotWindow(now)
	estimate := source.CrossValidate(snapshot)
	if caseID == 0 {
		_, err = s.openCase(ctx, accountID, exit, estimate, now, observations...)
		return err
	}
	return s.reassertExistingHolds(ctx, caseID, accountID, exit)
}

func (s *Service) SetLedgerSink(sink LedgerSink) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ledgerSink = sink
	s.mu.Unlock()
}

// New constructs the direct loop and starts its periodic fallback evaluator.
func New(cfg Config, qualityRegistry *registry.Registry, source EvidenceSource, dispatcher Dispatcher) *Service {
	cfg = cfg.normalized()
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	service := &Service{
		cfg: cfg, registry: qualityRegistry, evidence: source,
		dispatcher: dispatcher, logger: logger, cancel: cancel, done: make(chan struct{}),
	}
	if service.evidence != nil && service.registry != nil {
		go service.run(ctx)
	} else {
		close(service.done)
	}
	return service
}

// ReviewNow forces the same finite evaluator used by the background loop.
// It is an operational trigger, not a second review state machine.
func (s *Service) ReviewNow(ctx context.Context) (EvalStats, error) {
	return s.Evaluate(ctx, time.Now().UTC())
}

func (s *Service) Close(ctx context.Context) error {
	if s == nil || s.cancel == nil {
		return nil
	}
	s.cancel()
	select {
	case <-s.done:
		return nil
	default:
	}
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) run(parent context.Context) {
	defer close(s.done)
	evaluateEvery := s.Config().EvaluateEvery
	ticker := time.NewTicker(evaluateEvery)
	defer ticker.Stop()
	for {
		select {
		case <-parent.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(parent, evaluateEvery)
			if _, err := s.Evaluate(ctx, time.Now().UTC()); err != nil && parent.Err() == nil {
				s.logger.Warn("court_evaluate_failed", "error", err.Error())
			}
			cancel()
		}
	}
}
