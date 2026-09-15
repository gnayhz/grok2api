package court

import (
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestAttributionRequiresFrozenBaselineAndSample(t *testing.T) {
	for _, mismatch := range []string{"none", "model", "effort", "tools", "protocol", "sample", "revision", "task_spec", "control"} {
		t.Run(mismatch, func(t *testing.T) {
			tasks := experimentFixture(model.ProbeResultDegraded, model.ProbeResultClean)
			baseline := tasks[0].Attempt
			baseline.Profile = attemptmeta.Profile{Known: true, Protocol: "responses", ReasoningEffort: "xhigh", Tools: true}
			spec := model.NewProbeExperiment(model.Observation{EventID: "trigger/admission", Attempt: baseline})
			policy := policyFor(DefaultConfig(), time.Now().UTC())
			policy.Experiment = spec
			for i := range tasks {
				tasks[i].Experiment = spec
				profile := spec.Profile()
				tasks[i].Attempt.Profile, tasks[i].ControlAttempt.Profile = profile, profile
			}
			switch mismatch {
			case "model":
				tasks[0].Attempt.Model = "different-model"
			case "effort":
				tasks[0].Attempt.Profile.ReasoningEffort = "high"
			case "tools":
				tasks[0].Attempt.Profile.Tools = true
			case "protocol":
				tasks[0].Attempt.Profile.Protocol = "chat"
			case "sample":
				tasks[0].Attempt.Profile.Sample = "another-sample"
			case "revision":
				tasks[0].Attempt.Revision++
			case "task_spec":
				tasks[0].Experiment.Sample = "changed"
			case "control":
				tasks[0].ControlAttempt.Profile.Sample = "another-sample"
			}
			report := assessExperiment(tasks, policy)
			if mismatch == "none" && report.Verdict != model.VerdictAccountGuilty {
				t.Fatalf("valid experiment rejected: %+v", report)
			}
			if mismatch != "none" && report.Verdict == model.VerdictAccountGuilty {
				t.Fatalf("mismatched experiment convicted: %+v", report)
			}
		})
	}
}
