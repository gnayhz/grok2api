package court

// The experiment protocol is a deterministic, replayable decision function.
// No clocks, database writes, network calls or uncalibrated probabilities live
// here. Every input is retained in the case and task records.
import (
	"encoding/json"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

const ProtocolVersion = "controlled-comparison-v2"

type ExperimentPolicy struct {
	Experiment         model.ProbeExperiment `json:"experiment,omitempty"`
	Version            string                `json:"version"`
	AccountPaths       int                   `json:"account_paths"`
	AccountNodes       int                   `json:"account_nodes"`
	JurySize           int                   `json:"jury_size"`
	JuryDegraded       int                   `json:"jury_degraded"`
	TransportPaths     int                   `json:"transport_paths"`
	MaxAccountAttempts int                   `json:"max_account_attempts"`
	MaxJuryAttempts    int                   `json:"max_jury_attempts"`
	DeadlineAt         time.Time             `json:"deadline_at"`
}

func policyFor(cfg Config, opened time.Time) ExperimentPolicy {
	cfg = cfg.normalized()
	return ExperimentPolicy{Version: ProtocolVersion, AccountPaths: max(2, cfg.AccountNeedExits),
		AccountNodes: max(2, cfg.AccountSpanNodes), JurySize: max(2, cfg.ExitNeedN),
		JuryDegraded: max(2, cfg.ExitNeedK), TransportPaths: max(3, cfg.AccountNeedExits),
		MaxAccountAttempts: maxDifferentialAttempts(cfg.AccountNeedExits), MaxJuryAttempts: maxJuryAttempts(cfg.ExitNeedN),
		DeadlineAt: opened.Add(cfg.InvestigationTimeout)}
}

func casePolicy(record model.CaseRecord, cfg Config) ExperimentPolicy {
	var envelope struct {
		Policy ExperimentPolicy `json:"policy"`
	}
	if json.Unmarshal([]byte(record.EvidenceJSON), &envelope) == nil && envelope.Policy.Version != "" {
		return envelope.Policy
	}
	return policyFor(cfg, record.OpenedAt)
}
