package model

import (
	"context"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

const ProbeExperimentVersion = "thinking-presence-v2"
const LegacyProbeExperimentVersion = "reasoning-capability-v1"

// ProbeExperiment retains the trigger as provenance. New experiments measure
// account/path thinking-stream presence with a standard profile, independently of
// the trigger's tools, effort or client protocol. Legacy experiments retain
// their original matching rules.
type ProbeExperiment struct {
	ResourceCheck  *ResourceCheckPlan   `json:"resource_check,omitempty"`
	Version        string               `json:"version"`
	TriggerEventID string               `json:"trigger_event_id"`
	Baseline       attemptmeta.Identity `json:"baseline"`
	Sample         string               `json:"sample"`
}

func NewProbeExperiment(obs Observation) ProbeExperiment {
	return ProbeExperiment{Version: ProbeExperimentVersion, TriggerEventID: obs.EventID, Baseline: obs.Attempt, Sample: "brief-confirmation"}
}

func (s ProbeExperiment) UnsupportedReason() string {
	if s.Version == AccountCheckVersion || s.Version == ResourceCheckVersion {
		if s.Baseline.Provider != "grok_build" || s.Baseline.Model == "" || s.Baseline.RuleVersion == "" || s.Prompt() == "" {
			return "experiment_baseline_missing"
		}
		return ""
	}
	if s.Version != ProbeExperimentVersion && s.Version != LegacyProbeExperimentVersion {
		return "unsupported_experiment_version"
	}
	p := s.Baseline.Profile
	if s.TriggerEventID == "" || s.Baseline.ID == "" || s.Baseline.Provider != "grok_build" || s.Baseline.Model == "" || s.Baseline.RuleVersion == "" {
		return "experiment_baseline_missing"
	}
	if s.Version == LegacyProbeExperimentVersion && !p.Known {
		return "experiment_baseline_missing"
	}
	if s.Version == LegacyProbeExperimentVersion && (p.Protocol != "responses" || p.Tools) {
		return "experiment_profile_unsupported"
	}
	if s.Prompt() == "" {
		return "experiment_sample_unknown"
	}
	return ""
}

// Profile is the complete measurement contract shared by main and control
// probes. Baseline.Profile remains the unmodified production observation.
func (s ProbeExperiment) Profile() attemptmeta.Profile {
	p := attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "low"}
	if s.Version == LegacyProbeExperimentVersion {
		p = s.Baseline.Profile
	}
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
	if s.Version == AccountCheckVersion {
		switch s.Sample {
		case "brief-confirmation":
			return "Please reply with a brief greeting."
		case "repeated-as":
			return "Reply only OK. Data: " + strings.Repeat("a", 500)
		}
		return ""
	}
	if s.Version == ProbeExperimentVersion {
		if s.Sample == "brief-confirmation" {
			return "Please reply with a brief greeting."
		}
		return ""
	}
	switch s.Sample {
	case "inventory":
		return "Reason through this inventory problem step by step: a box starts with 17 red and 23 blue counters. Remove 5 red counters, add twice as many blue counters as the red counters remaining, then remove 9 blue counters. How many counters remain in total? Give the final count."
	case "ordering":
		return "Reason step by step: Ada, Bo, Cy and Di finish a race without ties. Ada finishes before Cy. Bo finishes immediately after Di. Cy does not finish last, and Di does not finish first. What is their finishing order?"
	case "distances":
		return "Reason step by step: a walker travels 7 km east, 3 km north, 2 km west, then 9 km south. What is the squared straight-line distance in square km from the starting point? Give the final number."
	default:
		return ""
	}
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
