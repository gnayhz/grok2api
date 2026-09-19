// Package court implements the finite quality attribution loop.
//
// New Build incidents freeze a bounded resource-proof investigation shared with
// manual checks. Each party is decided by complete A/B observations and R1/R2/R3
// certificates, never vote counts. Historical cases retain their frozen policy.
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

// Config contains lifecycle bounds and historical comparison thresholds.
// New proof investigations use the shared resource protocol;
// EvaluateEvery is a process cadence, not a decision state.
type Config struct {
	EvaluateEvery time.Duration
	// InvestigationTimeout bounds one case from opening to settlement. A
	// missing probe worker or a permanently pending task must never leave an
	// account and exit remanded forever; once this deadline is reached the
	// evaluator closes using completed evidence without dispatching more work.
	InvestigationTimeout time.Duration

	// AccountNeedExits is the historical target of distinct comparison paths for a
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

// DefaultConfig returns lifecycle defaults and legacy comparison settings.
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

// DispatchSpec selects a shared proof task or historical comparison groups.
// 与 investigator.DispatchSpec 保持字段同步,组合根 quality_judicial.go 逐字段复制。
type DispatchSpec struct {
	Proof           bool
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
	SnapshotWindow(now time.Time) model.Snapshot
	CrossValidate(snapshot model.Snapshot) model.Estimate
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
	proofCurrent    func(context.Context, model.AccountCheckSample) (bool, error)
	sameExit        SameExit
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
func (s *Service) SetProofIdentityCheck(check func(context.Context, model.AccountCheckSample) (bool, error)) {
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

// SameExit answers the advisory exclusion question for comparison-exit
// selection: are these two nodes KNOWN to share one real egress? true means
// "do not spend a differential probe here" — never "the paths are
// admissible". The court layer holds no egress-address knowledge itself; the
// composition root adapts the egress snapshot into this seam. nil means "no
// information" and must never exclude anything, and the live per-node path
// verification remains the sole authority for admissibility.
type SameExit func(ctx context.Context, nodeIDa, nodeIDb uint64) bool

// SetSameExit installs the advisory same-exit exclusion set. nil restores the
// "no information" state, which excludes nothing.
func (s *Service) SetSameExit(check SameExit) {
	s.mu.Lock()
	s.sameExit = check
	s.mu.Unlock()
}

// excludesKnownSameExit consults the seam for one candidate against the
// baseline exit. A nil seam, a zero node ID, or any "unknown" answer from the
// seam keeps today's behaviour (the candidate stays). The baseline node is
// never excluded here: it is already filtered as a comparison target.
func (s *Service) excludesKnownSameExit(ctx context.Context, baselineNodeID, candidateNodeID uint64) bool {
	if baselineNodeID == 0 || candidateNodeID == 0 {
		return false
	}
	s.mu.RLock()
	check := s.sameExit
	s.mu.RUnlock()
	return check != nil && check(ctx, baselineNodeID, candidateNodeID)
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
