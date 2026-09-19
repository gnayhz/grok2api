// Package court implements the finite quality attribution loop.
//
// New Build incidents freeze a bounded resource-proof investigation shared with
// manual checks. Each party is decided by complete A/B observations and R1/R2/R3
// certificates, never vote counts. Retired protocols never execute or produce new verdicts.
package court

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

// Config contains investigation lifecycle bounds.
// New proof investigations use the shared resource protocol;
// EvaluateEvery is a process cadence, not a decision state.
type Config struct {
	EvaluateEvery time.Duration
	// InvestigationTimeout bounds one case from opening to settlement. A
	// missing probe worker or a permanently pending task must never leave an
	// account and exit remanded forever; once this deadline is reached the
	// evaluator closes using completed evidence without dispatching more work.
	InvestigationTimeout time.Duration

	Logger *slog.Logger
}

// DefaultConfig returns lifecycle defaults.
func DefaultConfig() Config {
	return Config{
		EvaluateEvery:        15 * time.Second,
		InvestigationTimeout: 10 * time.Minute,
	}
}

func (c Config) normalized() Config {
	if c.EvaluateEvery <= 0 {
		c.EvaluateEvery = 15 * time.Second
	}
	if c.InvestigationTimeout <= 0 {
		c.InvestigationTimeout = 10 * time.Minute
	}
	return c
}

// SetConfig hot-applies lifecycle bounds. The evaluation cadence and
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

// DispatchSpec identifies the parties for one bounded proof task.
type DispatchSpec struct {
	CaseID       uint64
	Defendant    uint64
	BaselineExit model.EpochKey
}

// EvidenceSource provides the traffic window used for fallback incident
// discovery and for selecting unimplicated probe targets. Probe conclusions
// are evaluated from the finite task rows, not from a rolling window.
type EvidenceSource interface {
	SnapshotWindow(now time.Time) model.Snapshot
}

// LedgerSink records a classified degradation against the observed epoch.
type LedgerSink interface {
	RecordDegradeEvent(ctx context.Context, nodeID, epoch uint64, at time.Time) error
}

// Service is the single direct attribution loop.
type Service struct {
	cfg      Config
	registry StateStore
	probes   ProbeReader
	evidence EvidenceSource

	probeAccounts   ProbeAccounts
	nodes           proxy.NodeSource
	dispatcher      Dispatcher
	ledgerSink      LedgerSink
	accountExists   AccountExists
	proofCurrent    func(context.Context, model.ResourceSample) (bool, error)
	accountReleased AccountReleased
	logger          *slog.Logger

	mu sync.RWMutex
	// evaluateMu serializes incident opening and settlement. Without this
	// boundary the request reporter and the periodic fallback
	// could observe the same degradation concurrently and create duplicate
	// cases or release a party while another goroutine is freezing it.
	evaluateMu evaluationLock

	cancel context.CancelFunc
	closed bool
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

// SetProofIdentityCheck supplies current account/path generations without a
// second upstream request. The source owns credential and binding facts.
func (s *Service) SetProofIdentityCheck(check func(context.Context, model.ResourceSample) (bool, error)) {
	s.mu.Lock()
	s.proofCurrent = check
	s.mu.Unlock()
}

// AccountReleased is called once after a court action actually transitioned an
// account out of this court's investigation hold. The court reports only the
// fact; what the account axis does with it belongs to the account application
// layer, which the composition root installs here. A failure is logged and must
// never invalidate a verdict that is already committed.
type AccountReleased func(ctx context.Context, accountID uint64) error

// SetAccountReleased installs the release notification. nil disables it.
func (s *Service) SetAccountReleased(notify AccountReleased) {
	s.mu.Lock()
	s.accountReleased = notify
	s.mu.Unlock()
}

// notifyAccountReleased reports one released account. Callers must already have
// established that this court action moved the account from held to eligible:
// an account still detained by a second case must not be reported, or releasing
// one case would lift a hold the other case still owns.
func (s *Service) notifyAccountReleased(ctx context.Context, accountID uint64) {
	if accountID == 0 {
		return
	}
	s.mu.RLock()
	notify := s.accountReleased
	s.mu.RUnlock()
	if notify == nil {
		return
	}
	if err := notify(ctx, accountID); err != nil && s.logger != nil {
		s.logger.Warn("court_account_release_notify_failed", "account", accountID, "error", err.Error())
	}
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
	incident := model.IncidentKey{AccountID: accountID, Exit: exit}
	lastClosed, err := registryStore.LastClosedAtForIncidents(ctx, []model.IncidentKey{incident})
	if err != nil {
		return err
	}
	if closedAt, found := lastClosed[incident]; found && !observedAt.After(closedAt) {
		return nil
	}
	now := time.Now().UTC()
	if caseID == 0 {
		_, err = s.openCase(ctx, accountID, exit, now, observations...)
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

// New constructs the service without starting any loop. Composition roots
// must call Run explicitly; constructing never launches background work.
func New(cfg Config, qualityRegistry StateStore, source EvidenceSource, dispatcher Dispatcher, probes ProbeReader) *Service {
	cfg = cfg.normalized()
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	service := &Service{
		cfg: cfg, registry: qualityRegistry, evidence: source, probes: probes,
		dispatcher: dispatcher, logger: logger, done: make(chan struct{}),
	}
	if service.evidence == nil || service.registry == nil {
		close(service.done)
	}
	return service
}

// Run starts the periodic evaluator and returns immediately. The first
// evaluation happens after EvaluateEvery. Repeated Run calls are ignored;
// Close prevents subsequent starts and joins the owned loop.
func (s *Service) Run(ctx context.Context) {
	s.mu.Lock()
	if s.cancel != nil || s.closed {
		s.mu.Unlock()
		return
	}
	// Inert construction (missing evidence/registry) never runs a loop.
	select {
	case <-s.done:
		s.mu.Unlock()
		return
	default:
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()
	go s.run(runCtx)
}

// ReviewNow forces the same finite evaluator used by the background loop.
// It is an operational trigger, not a second review state machine.
func (s *Service) ReviewNow(ctx context.Context) (EvalStats, error) {
	return s.Evaluate(ctx, time.Now().UTC())
}

func (s *Service) Close(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	cancel := s.cancel
	if !s.closed {
		s.closed = true
		if cancel == nil {
			// Serialize close-before-start with Run and other Close calls.
			select {
			case <-s.done:
			default:
				close(s.done)
			}
		}
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Every caller joins the same loop, including retries after a timeout.
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
