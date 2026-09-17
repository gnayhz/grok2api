package court

// 受控对照实验的确定性评估器(纯规则):实验组/报告聚合与任务
// 判读。无时钟、无库写、无网络、无可校准概率——每个输入都保留在
// 案件与任务记录中,可重放。策略形状(policyFor/casePolicy)在
// protocol.go。
import (
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type ExperimentGroup struct {
	Attempts           int `json:"attempts"`
	Pending            int `json:"pending"`
	Cancelled          int `json:"cancelled"`
	Clean              int `json:"clean"`
	Degraded           int `json:"degraded"`
	Transport          int `json:"transport"`
	Unavailable        int `json:"unavailable"`
	ConfirmedDegraded  int `json:"confirmed_degraded"`
	ConfirmedTransport int `json:"confirmed_transport"`
}

type ExperimentReport struct {
	Policy           ExperimentPolicy `json:"policy"`
	Verdict          model.Verdict    `json:"verdict"`
	Reason           string           `json:"reason"`
	Account          ExperimentGroup  `json:"account"`
	Exit             ExperimentGroup  `json:"exit"`
	AccountSupport   []string         `json:"account_support"`
	ExitSupport      []string         `json:"exit_support"`
	Limitations      []string         `json:"limitations"`
	AccountSuspicion string           `json:"account_suspicion"`
	AccountSpanNodes int              `json:"account_span_nodes"`
	Phase            string           `json:"phase"`
	AccountCleared   bool             `json:"account_cleared"`
	ExitCleared      bool             `json:"exit_cleared"`
}

// assessExperiment keeps missing results, failures and confirmed repeated
// anomalies separate. Physical paths and independent identities count once.
func assessExperiment(tasks []model.ProbeTaskView, policy ExperimentPolicy) ExperimentReport {
	r := ExperimentReport{Policy: policy, AccountSupport: []string{}, ExitSupport: []string{}, Limitations: []string{},
		AccountSuspicion: "none", Phase: "collecting"}
	if policy.Version != ProtocolVersion {
		r.Reason, r.Phase = "unsupported_protocol", "ready"
		return r
	}
	seenJurors, seenPaths := map[uint64]bool{}, map[string]bool{}
	incidentPaths := map[string]bool{}
	nodes := map[uint64]bool{}
	cleanNodes := map[uint64]bool{}
	var comparisonPolicy attemptmeta.Identity
	mixedPolicy := false
	for _, task := range tasks {
		g := &r.Account
		if task.Direction == model.ProbeExitJury {
			g = &r.Exit
		}
		g.Attempts++
		switch task.State {
		case model.ProbePending, model.ProbeRunning:
			g.Pending++
			continue
		case model.ProbeCancelled:
			g.Cancelled++
			continue
		}
		accountID := task.Defendant
		if task.Direction == model.ProbeExitJury {
			accountID = task.Juror
		}
		if policy.Experiment.Version != "" && (task.Experiment != policy.Experiment || !policy.Experiment.Matches(task.Attempt)) {
			g.Unavailable++
			r.Limitations = appendUnique(r.Limitations, "experiment_mismatch")
			continue
		}
		if !model.ProbeIdentityMatches(task.Attempt, accountID, task.NodeID, task.Epoch) {
			g.Unavailable++
			r.Limitations = appendUnique(r.Limitations, "physical_identity_unverified")
			continue
		}
		if comparisonPolicy.ID == "" {
			comparisonPolicy = task.Attempt
		} else if !sameProbePolicy(comparisonPolicy, task.Attempt) {
			mixedPolicy = true
		}
		if task.Direction == model.ProbeExitJury {
			if task.Juror == 0 || seenJurors[task.Juror] {
				continue
			}
			seenJurors[task.Juror] = true
			if task.PathKey == "" {
				g.Unavailable++
				r.Limitations = appendUnique(r.Limitations, "incident_path_unverified")
				continue
			}
			incidentPaths[task.PathKey] = true
		} else if task.PathKey != "" {
			if seenPaths[task.PathKey] {
				r.Limitations = appendUnique(r.Limitations, "duplicate_exit_ip")
				// Deduplication removes support, never counterevidence. A clean
				// repeat through an alias must still block an account verdict.
				if task.State == model.ProbeDone && task.VerifiedIPChange {
					if task.Result == model.ProbeResultClean {
						g.Clean++
					}
					if task.Result == model.ProbeResultDegraded {
						g.Degraded++
					}
				}
				continue
			}
			seenPaths[task.PathKey] = true
		}
		controlled := task.ControlOutcome == model.ProbeResultClean && task.ControlVerified && task.PathKey != "" &&
			model.ProbeIdentityMatches(task.ControlAttempt, task.ControlAccountID, task.ControlNodeID, task.ControlEpoch) && sameProbePolicy(task.Attempt, task.ControlAttempt)
		if task.Direction == model.ProbeAccountDifferential {
			controlled = controlled && task.VerifiedIPChange && task.ControlAccountID != 0 &&
				task.ControlAccountID != task.Defendant && task.ControlNodeID == task.NodeID && task.ControlPathKey == task.PathKey
		} else {
			controlled = controlled && task.ControlAccountID == task.Juror && task.ControlNodeID != 0 &&
				task.ControlNodeID != task.NodeID && task.ControlPathKey != "" && task.ControlPathKey != task.PathKey
		}
		switch {
		case task.State == model.ProbeDone && task.Result == model.ProbeResultClean:
			if task.Direction == model.ProbeAccountDifferential && (!task.VerifiedIPChange || task.PathKey == "") {
				g.Unavailable++
				continue
			}
			g.Clean++
			if task.Direction == model.ProbeAccountDifferential && task.NodeID != 0 {
				cleanNodes[task.NodeID] = true
			}
		case task.State == model.ProbeDone && task.Result == model.ProbeResultDegraded:
			g.Degraded++
			if controlled {
				g.ConfirmedDegraded++
				if task.Direction == model.ProbeAccountDifferential {
					nodes[task.NodeID] = true
				}
			}
		case transportFailure(task.FailureKind):
			g.Transport++
			if controlled {
				g.ConfirmedTransport++
				if task.Direction == model.ProbeAccountDifferential {
					nodes[task.NodeID] = true
				}
			}
		default:
			g.Unavailable++
		}
		if !controlled && task.Result != model.ProbeResultClean {
			r.Limitations = appendUnique(r.Limitations, "missing_matched_control")
		}
	}
	if mixedPolicy {
		r.Phase, r.Reason = "ready", "insufficient_controls"
		r.Limitations = appendUnique(r.Limitations, "mixed_guard_policies")
		if r.Account.Pending+r.Exit.Pending > 0 {
			r.Phase, r.Reason = "collecting", "awaiting_probes"
		}
		return r
	}
	r.AccountSpanNodes = len(nodes)
	// Exoneration belongs to each completed comparison group, not the whole
	// case. Require a full, independently verified clean group; a single clean
	// result, unresolved failure or changed incident path is not enough.
	r.AccountCleared = r.Account.Clean >= policy.AccountPaths && len(seenPaths) >= policy.AccountPaths &&
		len(cleanNodes) >= policy.AccountNodes && r.Account.Attempts == r.Account.Clean
	r.ExitCleared = r.Exit.Clean >= policy.JurySize && r.Exit.Attempts == r.Exit.Clean && len(incidentPaths) == 1
	if r.Exit.Clean >= policy.JurySize {
		r.AccountSupport = append(r.AccountSupport, "incident_exit_serves_other_accounts")
	}
	if r.Account.ConfirmedDegraded > 0 {
		r.AccountSupport = append(r.AccountSupport, "account_degraded_on_healthy_paths")
		r.AccountSuspicion = "quality_pattern"
	}
	if r.Account.Transport >= 2 && r.Exit.Clean >= policy.JurySize {
		r.AccountSupport = append(r.AccountSupport, "repeated_account_response_failures")
		r.AccountSuspicion = "repeated_unresolved"
	}
	if r.Account.ConfirmedTransport > 0 && r.Account.ConfirmedTransport+r.Account.ConfirmedDegraded >= policy.TransportPaths {
		r.AccountSupport = append(r.AccountSupport, "matched_controls_exclude_path_outage")
	}
	if r.Exit.ConfirmedDegraded > 0 {
		r.ExitSupport = append(r.ExitSupport, "other_accounts_degrade_only_on_incident_exit")
	}
	if r.Account.Clean > 0 {
		r.ExitSupport = append(r.ExitSupport, "account_recovers_on_other_exit")
	}
	if r.Account.Clean > 0 && (r.Account.Degraded > 0 || r.Account.Transport > 0) {
		r.Limitations = appendUnique(r.Limitations, "account_results_conflict")
	}
	if r.Exit.Degraded > 0 && r.Account.Degraded > 0 {
		r.Limitations = appendUnique(r.Limitations, "both_directions_abnormal")
	}
	if r.Account.Cancelled+r.Exit.Cancelled > 0 {
		r.Limitations = appendUnique(r.Limitations, "interrupted_tests")
	}
	if r.Exit.Clean+r.Exit.Degraded < policy.JurySize {
		r.Limitations = appendUnique(r.Limitations, "exit_control_quorum_missing")
	}
	if r.Account.Pending+r.Exit.Pending > 0 {
		r.Reason = "awaiting_probes"
		return r
	}
	if len(incidentPaths) > 1 {
		r.Phase = "ready"
		r.Reason = "insufficient_controls"
		r.Limitations = appendUnique(r.Limitations, "incident_exit_changed")
		return r
	}
	r.Phase = "ready"
	// Negative controls must agree. A high count can never cancel a
	// contradictory valid measurement or substitute for path independence.
	cleanJury := r.Exit.Clean >= policy.JurySize && r.Exit.Degraded == 0
	if cleanJury && r.Account.Clean == 0 && len(nodes) >= policy.AccountNodes {
		if r.Account.ConfirmedDegraded >= policy.AccountPaths {
			r.Verdict, r.Reason = model.VerdictAccountGuilty, "account_quality_pattern"
		} else if r.Account.ConfirmedTransport >= policy.TransportPaths ||
			r.Account.ConfirmedTransport+r.Account.ConfirmedDegraded >= policy.TransportPaths {
			r.Verdict, r.Reason, r.AccountSuspicion = model.VerdictAccountGuilty, "account_availability_pattern", "confirmed_availability"
		}
	}
	if r.Exit.ConfirmedDegraded >= policy.JuryDegraded && r.Exit.Clean+r.Exit.ConfirmedDegraded >= policy.JurySize &&
		r.Account.Clean > 0 && r.Account.Degraded == 0 && r.Account.ConfirmedTransport == 0 {
		r.Verdict, r.Reason = model.VerdictExitGuilty, "exit_quality_pattern"
	}
	if r.Verdict == model.VerdictNone {
		r.Reason = "insufficient_controls"
		if r.Account.Clean > 0 && r.Account.Degraded == 0 && r.Account.Transport == 0 && r.Exit.Degraded == 0 && r.Exit.Clean >= policy.JurySize {
			r.Reason = "not_reproduced"
		}
		if r.AccountSuspicion == "repeated_unresolved" {
			r.Reason = "repeated_anomaly_unconfirmed"
		}
		if (r.Account.Degraded > 0 && r.Exit.Degraded > 0) || (r.Account.Clean > 0 && (r.Account.Degraded > 0 || r.Account.Transport > 0)) {
			r.Reason = "conflicting_evidence"
		}
	}
	return r
}

func transportFailure(kind string) bool {
	return model.ProbeFailure(kind).SupportsAvailability()
}

func appendUnique(values []string, value string) []string {
	for _, v := range values {
		if v == value {
			return values
		}
	}
	return append(values, value)
}

func sameProbePolicy(a, b attemptmeta.Identity) bool {
	return a.Profile == b.Profile && a.Revision == b.Revision && a.RuleVersion == b.RuleVersion && a.Provider == b.Provider && a.Model == b.Model
}
