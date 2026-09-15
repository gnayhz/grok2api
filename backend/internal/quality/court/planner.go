package court

import (
	"context"
	"encoding/json"
	"math/rand"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type planningProgress struct {
	JuryTasks, JuryTotal, JuryClean, JuryDegraded, JuryFailed                          int
	DiffTasks, DiffClean, DiffDegraded, DiffFailed, DiffTransportFailed, DiffSpanNodes int
}

// A replacement round reuses one read of ordinary eligibility for both
// directions and their matched controls. This snapshot is only a plan;
// execution still checks current account, identity and path eligibility.
type replacementCandidates struct {
	accountIDs    []uint64
	accountLoaded bool
	nodeIDs       map[uint64]bool
}

func (c *replacementCandidates) accounts(ctx context.Context, s *Service, experiment model.ProbeExperiment) ([]uint64, error) {
	if !c.accountLoaded {
		ids, err := s.eligibleProbeAccounts(ctx, experiment)
		if err != nil {
			return nil, err
		}
		c.accountIDs, c.accountLoaded = ids, true
	}
	return c.accountIDs, nil
}

func (c *replacementCandidates) nodes(ctx context.Context, s *Service) (map[uint64]bool, error) {
	if c.nodeIDs == nil {
		nodes, err := s.comparisonNodes(ctx)
		if err != nil {
			return nil, err
		}
		c.nodeIDs = nodes
	}
	return c.nodeIDs, nil
}

func summarizePlanningProgress(tasks []registry.ProbeTaskView) planningProgress {
	r := assessExperiment(tasks, policyFor(DefaultConfig(), time.Time{}))
	return planningProgressFor(r)
}
func planningProgressFor(r ExperimentReport) planningProgress {
	return planningProgress{JuryTasks: r.Exit.Attempts, JuryTotal: r.Exit.Clean + r.Exit.ConfirmedDegraded,
		JuryClean: r.Exit.Clean, JuryDegraded: r.Exit.ConfirmedDegraded, JuryFailed: r.Exit.Transport + r.Exit.Unavailable,
		DiffTasks: r.Account.Attempts, DiffClean: r.Account.Clean, DiffDegraded: r.Account.ConfirmedDegraded,
		DiffFailed: r.Account.Transport + r.Account.Unavailable, DiffTransportFailed: r.Account.Transport, DiffSpanNodes: r.AccountSpanNodes}
}
func maxDifferentialAttempts(target int) int {
	if target <= 0 {
		target = 1
	}
	return max(target*2, target+2)
}

func maxJuryAttempts(target int) int {
	if target <= 0 {
		target = 1
	}
	return max(target*2, target+2)
}

// replaceAccountComparisons replaces only unavailable differential paths.
// Every previous comparison node is excluded, including failed nodes and a
// node that already produced valid degraded evidence. The baseline is also
// excluded. This preserves the task history while ensuring a transport error
// does not consume the evidence slot forever.
func (s *Service) replaceAccountComparisons(ctx context.Context, record registry.CaseRecord, parties []registry.PartyRecord, tasks []registry.ProbeTaskView, summary planningProgress, cfg Config, available *replacementCandidates) (int, error) {
	if s.dispatcher == nil || summary.DiffTasks >= maxDifferentialAttempts(cfg.AccountNeedExits) {
		return 0, nil
	}
	valid := summary.DiffClean + summary.DiffDegraded
	needed := cfg.AccountNeedExits - valid
	if needed <= 0 {
		return 0, nil
	}
	baseline := caseBaseline(record, parties)
	if baseline.NodeID == 0 || !s.isCurrentExitEpoch(baseline) {
		return 0, nil
	}
	seenKeys := map[model.EpochKey]struct{}{baseline: {}}
	seenNodes := map[uint64]struct{}{baseline.NodeID: {}}
	for _, task := range tasks {
		if task.Direction != model.ProbeAccountDifferential || task.NodeID == 0 {
			continue
		}
		seenKeys[model.EpochKey{NodeID: task.NodeID, Epoch: task.Epoch}] = struct{}{}
		seenNodes[task.NodeID] = struct{}{}
	}
	maxAdditional := maxDifferentialAttempts(cfg.AccountNeedExits) - summary.DiffTasks
	if maxAdditional < needed {
		needed = maxAdditional
	}
	if needed <= 0 {
		return 0, nil
	}
	nodes, err := available.nodes(ctx, s)
	if err != nil {
		return 0, err
	}
	candidates := s.unimplicatedExitCandidates(nodes, seenKeys)
	replacements := make([]model.EpochKey, 0, needed)
	for _, candidate := range candidates {
		if _, seen := seenNodes[candidate.NodeID]; seen {
			continue
		}
		seenNodes[candidate.NodeID] = struct{}{}
		replacements = append(replacements, candidate)
		if len(replacements) == needed {
			break
		}
	}
	if len(replacements) == 0 {
		return 0, nil
	}
	replacementSpec := DispatchSpec{HealthyExits: replacements}
	randomizeDirectDispatch(&replacementSpec)
	spec, err := s.withControls(ctx, DispatchSpec{
		CaseID: record.ID, Defendant: caseDefendant(parties), BaselineExit: baseline,
		HealthyExits: replacementSpec.HealthyExits,
	}, baseline, casePolicy(record, cfg), available)
	if err != nil {
		return 0, err
	}
	dispatched, err := s.dispatcher.DispatchForCase(ctx, spec)
	if err != nil {
		return dispatched, err
	}
	if dispatched > 0 {
		s.logger.Info("court_differential_replacements_dispatched", "case", record.ID,
			"requested", len(replacements), "dispatched", dispatched,
			"attempts", summary.DiffTasks+dispatched,
			"attempt_limit", maxDifferentialAttempts(cfg.AccountNeedExits))
	}
	return dispatched, nil
}

// replaceExitComparisons replaces unavailable jury accounts in the same way as
// differential exits. This is not needed for a clean four-account jury, but it
// replaces incomplete controls without discarding the original measurements. Identity-group and quality eligibility rules are
// applied again, so a replacement is an independent Build witness.
func (s *Service) replaceExitComparisons(ctx context.Context, record registry.CaseRecord, parties []registry.PartyRecord, tasks []registry.ProbeTaskView, summary planningProgress, cfg Config, available *replacementCandidates) (int, error) {
	if s.dispatcher == nil || summary.JuryTasks >= maxJuryAttempts(cfg.ExitNeedN) || summary.JuryTotal >= cfg.ExitNeedN {
		return 0, nil
	}
	needed := cfg.ExitNeedN - summary.JuryTotal
	maxAdditional := maxJuryAttempts(cfg.ExitNeedN) - summary.JuryTasks
	if needed > maxAdditional {
		needed = maxAdditional
	}
	if needed <= 0 {
		return 0, nil
	}
	baseline := caseBaseline(record, parties)
	if baseline.NodeID == 0 || !s.isCurrentExitEpoch(baseline) {
		return 0, nil
	}
	defendant := caseDefendant(parties)
	defendantGroup, _ := s.registry.IdentityGroupOf(defendant)
	seenAccounts := map[uint64]struct{}{defendant: {}}
	seenGroups := map[uint64]struct{}{defendantGroup: {}}
	for _, task := range tasks {
		if task.Direction != model.ProbeExitJury || task.Juror == 0 {
			continue
		}
		seenAccounts[task.Juror] = struct{}{}
		groupID, _ := s.registry.IdentityGroupOf(task.Juror)
		seenGroups[groupID] = struct{}{}
	}
	accounts, err := available.accounts(ctx, s, casePolicy(record, cfg).Experiment)
	if err != nil {
		return 0, err
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(accounts), func(i, j int) { accounts[i], accounts[j] = accounts[j], accounts[i] })
	jurors := make([]uint64, 0, needed)
	for _, accountID := range accounts {
		if len(jurors) == needed {
			break
		}
		if _, seen := seenAccounts[accountID]; seen || !s.registry.AccountEligible(accountID) {
			continue
		}
		groupID, _ := s.registry.IdentityGroupOf(accountID)
		if _, seen := seenGroups[groupID]; seen {
			continue
		}
		seenAccounts[accountID] = struct{}{}
		seenGroups[groupID] = struct{}{}
		jurors = append(jurors, accountID)
	}
	if len(jurors) == 0 {
		return 0, nil
	}
	spec, err := s.withControls(ctx, DispatchSpec{
		CaseID: record.ID, Defendant: defendant, CoRemandedExits: []model.EpochKey{baseline}, Jurors: jurors,
	}, baseline, casePolicy(record, cfg), available)
	if err != nil {
		return 0, err
	}
	dispatched, err := s.dispatcher.DispatchForCase(ctx, spec)
	if err != nil {
		return dispatched, err
	}
	if dispatched > 0 {
		s.logger.Info("court_jury_replacements_dispatched", "case", record.ID,
			"requested", len(jurors), "dispatched", dispatched,
			"attempts", summary.JuryTasks+dispatched,
			"attempt_limit", maxJuryAttempts(cfg.ExitNeedN))
	}
	return dispatched, nil
}

func (s *Service) replaceMissingComparisons(ctx context.Context, record registry.CaseRecord, parties []registry.PartyRecord, tasks []registry.ProbeTaskView, summary planningProgress, cfg Config) (int, error) {
	available := &replacementCandidates{}
	differential, err := s.replaceAccountComparisons(ctx, record, parties, tasks, summary, cfg, available)
	if err != nil {
		return differential, err
	}
	jury, err := s.replaceExitComparisons(ctx, record, parties, tasks, summary, cfg, available)
	return differential + jury, err
}

// reassertInvestigationHolds restores only restrictions still owned by this
// case. Repeated observations must not re-freeze an already cleared party.
func (s *Service) reassertInvestigationHolds(ctx context.Context, defendant uint64, exit model.EpochKey, caseID uint64) error {
	parties, err := s.registry.ListParties(ctx, caseID)
	if err != nil {
		return err
	}
	accountHeld, exitHeld := false, false
	for _, p := range parties {
		if p.Disposition != model.DispositionRemanded {
			continue
		}
		accountHeld = accountHeld || (p.Kind == model.PartyAccount && p.AccountID == defendant)
		exitHeld = exitHeld || (p.Kind == model.PartyExit && p.NodeID == exit.NodeID && p.Epoch == exit.Epoch)
	}
	account := s.registry.AccountState(defendant)
	if accountHeld {
		switch account.State {
		case model.AccountActive:
			if err := s.registry.TransitionAccount(ctx, registry.AccountTransitionRequest{
				AccountID: defendant, To: model.AccountRemanded, CaseID: caseID,
			}); err != nil {
				return err
			}
		case model.AccountRemanded, model.AccountSentenced:
			// A serving account can be a defendant in a later case, but is not
			// remanded a second time. The party record remains evidence-linked.
		}
	}
	if !exitHeld || exit.NodeID == 0 {
		return nil
	}
	if s.registry.CurrentEpoch(exit.NodeID) != exit.Epoch {
		return nil
	}
	entry := s.registry.ExitStateOfCurrentEpoch(exit.NodeID)
	switch entry.State {
	case model.ExitAvailable:
		return s.registry.TransitionExit(ctx, registry.ExitTransitionRequest{
			NodeID: exit.NodeID, Epoch: exit.Epoch, To: model.ExitRemanded, CaseID: caseID,
		})
	case model.ExitRemanded:
		return nil
	case model.ExitBanned:
		return s.registry.UpdatePartyDisposition(ctx, caseID, model.PartyExit, 0,
			exit.NodeID, exit.Epoch, model.DispositionReleased)
	default:
		return nil
	}
}

// reassertExistingHolds reasserts the hold for a repeated observation of
// the same incident. OpenCaseForIncident guarantees that this case already
// owns the exact baseline exit; a different exit is deliberately a new case.
func (s *Service) reassertExistingHolds(ctx context.Context, caseID, defendant uint64, exit model.EpochKey) error {
	return s.reassertInvestigationHolds(ctx, defendant, exit, caseID)
}

func caseDefendant(parties []registry.PartyRecord) uint64 {
	for _, party := range parties {
		if party.Kind == model.PartyAccount && party.Role == model.RoleDefendant {
			return party.AccountID
		}
	}
	return 0
}

func partyBaseline(parties []registry.PartyRecord) model.EpochKey {
	for _, party := range parties {
		if party.Kind == model.PartyExit && party.NodeID != 0 {
			return model.EpochKey{NodeID: party.NodeID, Epoch: party.Epoch}
		}
	}
	return model.EpochKey{}
}

func caseBaseline(record registry.CaseRecord, parties []registry.PartyRecord) model.EpochKey {
	if record.EvidenceJSON != "" {
		var opening struct {
			Exit *struct {
				Node  uint64 `json:"node"`
				Epoch uint64 `json:"epoch"`
			} `json:"exit"`
		}
		if err := json.Unmarshal([]byte(record.EvidenceJSON), &opening); err == nil && opening.Exit != nil {
			return model.EpochKey{NodeID: opening.Exit.Node, Epoch: opening.Exit.Epoch}
		}
	}
	return partyBaseline(parties)
}
