package model

import (
	"encoding/json"
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

func TestLegacyExperimentKeepsPersistedProfileContract(t *testing.T) {
	var spec ProbeExperiment
	if err := json.Unmarshal([]byte(`{"version":"reasoning-capability-v1","trigger_event_id":"fictional-event","baseline":{"id":"fictional-trigger","provider":"grok_build","model":"fictional-model","rule_version":"fictional-rule","profile":{"known":true,"protocol":"responses","reasoning_effort":"low"}},"sample":"inventory"}`), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.UnsupportedReason() != "" || spec.Profile().ReasoningEffort != "low" {
		t.Fatalf("legacy contract changed: %+v reason=%s", spec, spec.UnsupportedReason())
	}
	actual := spec.Baseline
	actual.Profile = spec.Profile()
	if !spec.Matches(actual) {
		t.Fatal("legacy measurement lost compatibility")
	}
	spec.Baseline.Profile.Tools = true
	if spec.UnsupportedReason() != "experiment_profile_unsupported" {
		t.Fatal("legacy experiment silently reinterpreted")
	}
}
