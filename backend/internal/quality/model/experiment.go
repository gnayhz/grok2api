package model

import (
	"context"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

// ProbeExperiment retains the trigger as provenance and freezes the shared
// full-response resource measurement contract.
type ProbeExperiment struct {
	ResourceCheck  *ResourceCheckPlan   `json:"resource_check,omitempty"`
	Version        string               `json:"version"`
	TriggerEventID string               `json:"trigger_event_id"`
	Baseline       attemptmeta.Identity `json:"baseline"`
	Sample         string               `json:"sample"`
}

func NewProbeExperiment(obs Observation) ProbeExperiment {
	return ProbeExperiment{Version: ResourceCheckVersion, TriggerEventID: obs.EventID, Baseline: obs.Attempt, Sample: "token-short"}
}

func (s ProbeExperiment) UnsupportedReason() string {
	if s.Version == ResourceCheckVersion {
		if s.Baseline.Provider != "grok_build" || s.Baseline.Model == "" || s.Baseline.RuleVersion == "" || s.Prompt() == "" {
			return "experiment_baseline_missing"
		}
		return ""
	}
	return "unsupported_experiment_version"
}

// Profile is the complete measurement contract shared by main and control
// probes. Baseline.Profile remains the unmodified production observation.
func (s ProbeExperiment) Profile() attemptmeta.Profile {
	p := attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "low"}
	p.Experiment, p.Sample = s.Version, s.Sample
	return p
}

func (s ProbeExperiment) Prompt() string {
	if s.Version == ResourceCheckVersion {
		switch s.Sample {
		case "token-short":
			return "Reply only OK. Data: " + strings.Repeat("a", 512)
		}
		return ""
	}
	return ""
}

func (s ProbeExperiment) Matches(actual attemptmeta.Identity) bool {
	b, p, expected := s.Baseline, actual.Profile, s.Profile()
	return s.UnsupportedReason() == "" && actual.Provider == b.Provider && actual.Model == b.Model && actual.Revision == b.Revision && actual.RuleVersion == b.RuleVersion &&
		p == expected
}

type probeExperimentKey struct{}

func WithProbeExperiment(ctx context.Context, spec ProbeExperiment) context.Context {
	return context.WithValue(ctx, probeExperimentKey{}, spec)
}

func ProbeExperimentFromContext(ctx context.Context) (ProbeExperiment, bool) {
	spec, ok := ctx.Value(probeExperimentKey{}).(ProbeExperiment)
	return spec, ok && spec.Version != ""
}
