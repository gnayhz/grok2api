package court

// The experiment protocol is a deterministic, replayable decision function.
// No clocks, database writes, network calls or uncalibrated probabilities live
// here. Every input is retained in the case and task records.
import (
	"encoding/json"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

type ExperimentPolicy struct {
	Experiment model.ProbeExperiment `json:"experiment,omitempty"`
	Version    string                `json:"version"`
	DeadlineAt time.Time             `json:"deadline_at"`
}

func casePolicy(record model.CaseRecord, cfg Config) ExperimentPolicy {
	var envelope struct {
		Policy ExperimentPolicy `json:"policy"`
	}
	if json.Unmarshal([]byte(record.EvidenceJSON), &envelope) == nil && envelope.Policy.Version != "" {
		return envelope.Policy
	}
	return ExperimentPolicy{DeadlineAt: record.OpenedAt.Add(cfg.normalized().InvestigationTimeout)}
}
