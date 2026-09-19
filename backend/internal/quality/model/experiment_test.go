package model

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

func TestStandardExperimentSeparatesTriggerFromMeasurement(t *testing.T) {
	for _, profile := range []attemptmeta.Profile{
		{Known: true, Protocol: "responses", ReasoningEffort: "xhigh", Tools: true},
		{Known: true, Protocol: "chat.completions", ReasoningEffort: "low"},
		{},
	} {
		trigger := attemptmeta.Identity{ID: "fictional-trigger", Provider: "grok_build", Model: "fictional-model", Revision: 4, RuleVersion: "fictional-rule", Profile: profile}
		spec := NewProbeExperiment(Observation{EventID: "fictional-event", Attempt: trigger})
		if reason := spec.UnsupportedReason(); reason != "" {
			t.Fatalf("trigger profile %+v rejected: %s", profile, reason)
		}
		if spec.Baseline != trigger {
			t.Fatal("trigger provenance changed")
		}
		actual := trigger
		actual.Profile = attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "low", Experiment: spec.Version, Sample: spec.Sample}
		if !spec.Matches(actual) {
			t.Fatal("standard probe did not match")
		}
		for _, mutate := range []func(*attemptmeta.Identity){
			func(v *attemptmeta.Identity) { v.Profile.Tools = true },
			func(v *attemptmeta.Identity) { v.Profile.ReasoningEffort = "xhigh" },
			func(v *attemptmeta.Identity) { v.Profile.Sample = "other-sample" },
			func(v *attemptmeta.Identity) { v.Model = "other-model" },
			func(v *attemptmeta.Identity) { v.Revision++ },
			func(v *attemptmeta.Identity) { v.RuleVersion = "other-rule" },
		} {
			mismatch := actual
			mutate(&mismatch)
			if spec.Matches(mismatch) {
				t.Fatalf("accepted incomparable measurement: %+v", mismatch)
			}
		}
	}
}

func TestRetiredExperimentsAreRejected(t *testing.T) {
	for _, version := range []string{"reasoning-capability-v1", "thinking-presence-v2", "", "unknown"} {
		spec := NewProbeExperiment(Observation{Attempt: attemptmeta.Identity{Provider: "grok_build", Model: "fictional-model", RuleVersion: "fictional-rule"}})
		spec.Version = version
		if spec.UnsupportedReason() != "unsupported_experiment_version" {
			t.Fatalf("retired version accepted: %s", version)
		}
		actual := spec.Baseline
		actual.Profile = spec.Profile()
		if spec.Matches(actual) {
			t.Fatal("retired sample accepted")
		}
	}
}
