package model

import (
	"context"
	"hash/fnv"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

const ProbeExperimentVersion = "reasoning-capability-v1"

// ProbeExperiment freezes the trigger and normalized configuration. It measures
// reasoning-channel capability under a synthetic sample, not answer correctness
// or replay equivalence to private user content.
type ProbeExperiment struct {
	Version        string               `json:"version"`
	TriggerEventID string               `json:"trigger_event_id"`
	Baseline       attemptmeta.Identity `json:"baseline"`
	Sample         string               `json:"sample"`
}

func NewProbeExperiment(obs Observation) ProbeExperiment {
	h := fnv.New32a()
	_, _ = h.Write([]byte(obs.Attempt.ID))
	samples := [...]string{"inventory", "ordering", "distances"}
	return ProbeExperiment{Version: ProbeExperimentVersion, TriggerEventID: obs.EventID, Baseline: obs.Attempt, Sample: samples[h.Sum32()%uint32(len(samples))]}
}

func (s ProbeExperiment) UnsupportedReason() string {
	if s.Version != ProbeExperimentVersion {
		return "unsupported_experiment_version"
	}
	p := s.Baseline.Profile
	if s.TriggerEventID == "" || s.Baseline.ID == "" || s.Baseline.Provider != "grok_build" || s.Baseline.Model == "" || s.Baseline.RuleVersion == "" || !p.Known {
		return "experiment_baseline_missing"
	}
	if p.Protocol != "responses" || p.Tools {
		return "experiment_profile_unsupported"
	}
	if s.Prompt() == "" {
		return "experiment_sample_unknown"
	}
	return ""
}

func (s ProbeExperiment) Prompt() string {
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
	b, p := s.Baseline, actual.Profile
	return s.UnsupportedReason() == "" && actual.Provider == b.Provider && actual.Model == b.Model && actual.Revision == b.Revision && actual.RuleVersion == b.RuleVersion &&
		p.Known && p.Protocol == b.Profile.Protocol && p.ReasoningEffort == b.Profile.ReasoningEffort && p.Tools == b.Profile.Tools && p.Experiment == s.Version && p.Sample == s.Sample
}

type probeExperimentKey struct{}

func WithProbeExperiment(ctx context.Context, spec ProbeExperiment) context.Context {
	return context.WithValue(ctx, probeExperimentKey{}, spec)
}

func ProbeExperimentFromContext(ctx context.Context) (ProbeExperiment, bool) {
	spec, ok := ctx.Value(probeExperimentKey{}).(ProbeExperiment)
	return spec, ok && spec.Version != ""
}
