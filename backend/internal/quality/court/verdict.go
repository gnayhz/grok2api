package court

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"sort"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func (s *Service) isCurrentExitEpoch(key model.EpochKey) bool {
	return key.NodeID != 0 && s.registry.CurrentEpoch(key.NodeID) == key.Epoch
}

// marshalOpeningEvidence stores only stable identifiers and the original
// epoch. Raw IPs, credentials, and provider responses never enter the case.
func marshalOpeningEvidence(defendant uint64, exit model.EpochKey, estimate model.Estimate) (string, error) {
	subject := estimate.Accounts[defendant]
	payload := map[string]any{
		"trigger":   "traffic_degraded",
		"defendant": defendant,
		"exit":      map[string]uint64{"node": exit.NodeID, "epoch": exit.Epoch},
		"account_est": map[string]int{
			"witness_exits":  subject.Witnesses,
			"degraded_exits": subject.DegradedUnits,
			"span_nodes":     subject.SpanNodes,
		},
	}
	return marshalEvidence(payload)
}

func marshalEvidence(payload map[string]any) (string, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("serialize quality evidence: %w", err)
	}
	return string(raw), nil
}

// dispatchSpecFor builds the finite core round. Targets are selected from
// currently usable, unimplicated exits and accounts, then shuffled so the
// same low IDs are not repeatedly used as witnesses. Replacement rounds use
// the same dispatch boundary with an explicit, already-filtered candidate set.
func (s *Service) dispatchSpecFor(ctx context.Context, caseID, defendant uint64, baseline model.EpochKey, estimate model.Estimate, policy ExperimentPolicy) (DispatchSpec, error) {
	if !s.isCurrentExitEpoch(baseline) {
		return DispatchSpec{CaseID: caseID, Defendant: defendant}, nil
	}
	nodes, err := s.comparisonNodes(ctx)
	if err != nil {
		return DispatchSpec{}, err
	}
	accounts, err := s.eligibleProbeAccounts(ctx, policy.Experiment)
	if err != nil {
		return DispatchSpec{}, err
	}
	return s.dispatchSpecFromCandidates(ctx, caseID, defendant, baseline, estimate, policy, accounts, nodes), nil
}

func (s *Service) dispatchSpecFromCandidates(ctx context.Context, caseID, defendant uint64, baseline model.EpochKey, estimate model.Estimate, policy ExperimentPolicy, accounts []uint64, nodes map[uint64]bool) DispatchSpec {
	spec := DispatchSpec{CaseID: caseID, Defendant: defendant, BaselineExit: baseline}
	seenNodes := map[uint64]struct{}{baseline.NodeID: {}}
	seenKeys := map[model.EpochKey]struct{}{baseline: {}}
	for key, subject := range estimate.Exits {
		if !nodes[key.NodeID] || !s.isCurrentExitEpoch(key) || !s.registry.ExitEligible(key.NodeID) || subject.DegradedUnits != 0 {
			continue
		}
		if _, duplicate := seenNodes[key.NodeID]; duplicate {
			continue
		}
		seenNodes[key.NodeID] = struct{}{}
		seenKeys[key] = struct{}{}
		spec.HealthyExits = append(spec.HealthyExits, key)
	}
	for _, key := range s.unimplicatedExitCandidates(nodes, seenKeys) {
		if _, duplicate := seenNodes[key.NodeID]; duplicate {
			continue
		}
		seenNodes[key.NodeID] = struct{}{}
		seenKeys[key] = struct{}{}
		spec.HealthyExits = append(spec.HealthyExits, key)
	}
	// Advisory pre-filter: a comparison exit KNOWN to share the baseline's
	// real egress would only burn a real upstream probe and lose one
	// admissible evidence item — with bounded replacement rounds that can end
	// the case as "insufficient evidence". Unknown never excludes, and an
	// all-excluded plan simply has no comparison path, exactly like a case
	// whose traffic window has no healthy exit at all.
	spec.HealthyExits = s.filterKnownSameExitCandidates(ctx, baseline.NodeID, spec.HealthyExits)

	accountIDs := make([]uint64, 0, len(estimate.Accounts))
	for accountID, subject := range estimate.Accounts {
		if accountID != defendant && subject.Degraded == 0 {
			accountIDs = append(accountIDs, accountID)
		}
	}
	spec.Jurors = s.filterBuildJurors(accountIDs, accounts)
	spec.Jurors = s.filterIdentityGroupJurors(defendant, spec.Jurors)
	s.topUpJurorsFromFleet(&spec, accounts, policy.JurySize)

	spec.CoRemandedExits = []model.EpochKey{baseline}
	randomizeDirectDispatch(&spec)
	// The case protocol caps the plan before the worker applies its execution
	// ceilings. A separately raised worker setting cannot exceed case budgets.
	if len(spec.HealthyExits) > policy.AccountPaths {
		spec.HealthyExits = spec.HealthyExits[:policy.AccountPaths]
	}
	if len(spec.Jurors) > policy.JurySize {
		spec.Jurors = spec.Jurors[:policy.JurySize]
	}
	return spec
}

// filterKnownSameExitCandidates drops comparison-exit candidates the advisory
// seam already knows to share the baseline's real egress. It runs before the
// plan cap and the shuffle so the finite comparison budget is spent on usable
// candidates, and it never re-admits a dropped candidate: when every candidate
// is excluded the plan keeps today's behaviour for a case with no healthy
// exit — an empty comparison set.
func (s *Service) filterKnownSameExitCandidates(ctx context.Context, baselineNodeID uint64, candidates []model.EpochKey) []model.EpochKey {
	if baselineNodeID == 0 || len(candidates) == 0 {
		return candidates
	}
	kept := candidates[:0]
	for _, candidate := range candidates {
		if s.excludesKnownSameExit(ctx, baselineNodeID, candidate.NodeID) {
			continue
		}
		kept = append(kept, candidate)
	}
	return kept
}

func randomizeDirectDispatch(spec *DispatchSpec) {
	if spec == nil {
		return
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(spec.HealthyExits), func(i, j int) {
		spec.HealthyExits[i], spec.HealthyExits[j] = spec.HealthyExits[j], spec.HealthyExits[i]
	})
	rng.Shuffle(len(spec.Jurors), func(i, j int) {
		spec.Jurors[i], spec.Jurors[j] = spec.Jurors[j], spec.Jurors[i]
	})
}

// filterIdentityGroupJurors prevents one SSO identity from being counted as
// several independent witnesses.
func (s *Service) filterIdentityGroupJurors(defendant uint64, jurors []uint64) []uint64 {
	if defendant == 0 || len(jurors) == 0 {
		return jurors
	}
	defendantGroup, _ := s.registry.IdentityGroupOf(defendant)
	seenGroups := make(map[uint64]struct{}, len(jurors)+1)
	seenGroups[defendantGroup] = struct{}{}
	kept := jurors[:0]
	for _, juror := range jurors {
		groupID, _ := s.registry.IdentityGroupOf(juror)
		if _, seen := seenGroups[groupID]; seen {
			continue
		}
		seenGroups[groupID] = struct{}{}
		kept = append(kept, juror)
	}
	return kept
}

// filterBuildJurors intersects traffic witnesses with the scheduler-supplied
// candidates and Court's independent quality eligibility.
func (s *Service) filterBuildJurors(jurors, buildIDs []uint64) []uint64 {
	allowed := make(map[uint64]struct{}, len(buildIDs))
	for _, id := range buildIDs {
		allowed[id] = struct{}{}
	}
	kept := jurors[:0]
	for _, juror := range jurors {
		if _, ok := allowed[juror]; ok && s.registry.AccountEligible(juror) {
			kept = append(kept, juror)
		}
	}
	return kept
}

// topUpJurorsFromFleet samples the full current Build fleet rather than a
// fixed low-ID prefix, so each finite round can choose independent witnesses.
func (s *Service) topUpJurorsFromFleet(spec *DispatchSpec, accounts []uint64, target int) {
	if len(spec.Jurors) >= target {
		return
	}
	seen := map[uint64]struct{}{spec.Defendant: {}}
	seenGroups := map[uint64]struct{}{}
	if spec.Defendant != 0 {
		groupID, members := s.registry.IdentityGroupOf(spec.Defendant)
		seenGroups[groupID] = struct{}{}
		for _, member := range members {
			seen[member] = struct{}{}
		}
	}
	for _, juror := range spec.Jurors {
		seen[juror] = struct{}{}
		groupID, _ := s.registry.IdentityGroupOf(juror)
		seenGroups[groupID] = struct{}{}
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	rng.Shuffle(len(accounts), func(i, j int) {
		accounts[i], accounts[j] = accounts[j], accounts[i]
	})
	for _, accountID := range accounts {
		if len(spec.Jurors) >= target {
			break
		}
		groupID, _ := s.registry.IdentityGroupOf(accountID)
		if _, duplicate := seen[accountID]; duplicate {
			continue
		}
		if _, duplicate := seenGroups[groupID]; duplicate || !s.registry.AccountEligible(accountID) {
			continue
		}
		seen[accountID] = struct{}{}
		seenGroups[groupID] = struct{}{}
		spec.Jurors = append(spec.Jurors, accountID)
	}
}

// unimplicatedExitCandidates fills comparison targets from the enabled base
// fleet when the traffic window has too few observed healthy exits.
func (s *Service) unimplicatedExitCandidates(nodes map[uint64]bool, seen map[model.EpochKey]struct{}) []model.EpochKey {
	implicated := s.registry.CurrentExitStates()
	candidates := make([]model.EpochKey, 0, len(nodes))
	for id := range nodes {
		if id == 0 {
			continue
		}
		if _, implicated := implicated[id]; implicated {
			continue
		}
		key := model.EpochKey{NodeID: id, Epoch: s.registry.CurrentEpoch(id)}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		candidates = append(candidates, key)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].NodeID != candidates[j].NodeID {
			return candidates[i].NodeID < candidates[j].NodeID
		}
		return candidates[i].Epoch < candidates[j].Epoch
	})
	return candidates
}
