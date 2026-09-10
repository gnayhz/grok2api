package court

import (
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

func experimentFixture(diff model.ProbeResult, jury model.ProbeResult) []registry.ProbeTaskView {
	var tasks []registry.ProbeTaskView
	for i := uint64(1); i <= 3; i++ {
		key := fmt.Sprintf("exit-%d", i)
		tasks = append(tasks, registry.ProbeTaskView{ID: i, Direction: model.ProbeAccountDifferential, Defendant: 7, NodeID: i, State: model.ProbeDone,
			Result: diff, VerifiedIPChange: true, PathKey: key, FailureKind: string(model.ProbeFailureCreatedTimeout), ControlAccountID: 50 + i, ControlNodeID: i,
			ControlOutcome: model.ProbeResultClean, ControlVerified: true, ControlPathKey: key})
	}
	for i := uint64(1); i <= 4; i++ {
		tasks = append(tasks, registry.ProbeTaskView{ID: i + 3, Direction: model.ProbeExitJury, Defendant: 7, Juror: 50 + i, NodeID: 10, State: model.ProbeDone,
			Result: jury, PathKey: "incident", ControlAccountID: 50 + i, ControlNodeID: i, ControlOutcome: model.ProbeResultClean,
			ControlVerified: true, ControlPathKey: fmt.Sprintf("exit-%d", i)})
	}
	for i := range tasks {
		accountID := tasks[i].Defendant
		if tasks[i].Direction == model.ProbeExitJury {
			accountID = tasks[i].Juror
		}
		tasks[i].Attempt = experimentIdentity(fmt.Sprintf("main-%d", i), accountID, tasks[i].NodeID, tasks[i].Epoch)
		tasks[i].ControlAttempt = experimentIdentity(fmt.Sprintf("control-%d", i), tasks[i].ControlAccountID, tasks[i].ControlNodeID, tasks[i].ControlEpoch)
	}
	return tasks
}

func TestControlledAttributionScenarios(t *testing.T) {
	policy := policyFor(DefaultConfig(), time.Now())
	tests := []struct {
		name       string
		diff, jury model.ProbeResult
		edit       func([]registry.ProbeTaskView) []registry.ProbeTaskView
		verdict    model.Verdict
		reason     string
	}{
		{"account quality", model.ProbeResultDegraded, model.ProbeResultClean, nil, model.VerdictAccountGuilty, "account_quality_pattern"},
		{"account repeated transport", model.ProbeResultError, model.ProbeResultClean, nil, model.VerdictAccountGuilty, "account_availability_pattern"},
		{"exit quality", model.ProbeResultClean, model.ProbeResultDegraded, nil, model.VerdictExitGuilty, "exit_quality_pattern"},
		{"both healthy", model.ProbeResultClean, model.ProbeResultClean, nil, model.VerdictNone, "not_reproduced"},
		{"both degraded", model.ProbeResultDegraded, model.ProbeResultDegraded, nil, model.VerdictNone, "conflicting_evidence"},
		{"one account anomaly", model.ProbeResultDegraded, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView { return append(v[:1], v[3:]...) }, model.VerdictNone, "insufficient_controls"},
		{"single transport", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView { return append(v[:1], v[3:]...) }, model.VerdictNone, "insufficient_controls"},
		{"same physical IP", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 0; i < 3; i++ {
				v[i].PathKey = "shared"
				v[i].ControlPathKey = "shared"
			}
			return v
		}, model.VerdictNone, "insufficient_controls"},
		{"late clean alias cannot be hidden", model.ProbeResultDegraded, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			alias := v[0]
			alias.NodeID = 98
			alias.Attempt.Path.NodeID = 98
			alias.Result = model.ProbeResultClean
			return append(v, alias)
		}, model.VerdictNone, "conflicting_evidence"},
		{"unobserved paths still suspicious", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 0; i < 3; i++ {
				v[i].VerifiedIPChange = false
			}
			return v
		}, model.VerdictNone, "repeated_anomaly_unconfirmed"},
		{"path outage controls fail", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 0; i < 3; i++ {
				v[i].ControlOutcome = model.ProbeResultError
			}
			return v
		}, model.VerdictNone, "repeated_anomaly_unconfirmed"},
		{"credential failure is not transport", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 0; i < 3; i++ {
				v[i].FailureKind = "credential_unavailable"
			}
			return v
		}, model.VerdictNone, "insufficient_controls"},
		{"contradictory clean defendant", model.ProbeResultDegraded, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			v[0].Result = model.ProbeResultClean
			return v
		}, model.VerdictNone, "conflicting_evidence"},
		{"missing jury calibration", model.ProbeResultClean, model.ProbeResultDegraded, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 3; i < len(v); i++ {
				v[i].ControlOutcome = ""
			}
			return v
		}, model.VerdictNone, "insufficient_controls"},
		{"jury quorum missing", model.ProbeResultClean, model.ProbeResultDegraded, func(v []registry.ProbeTaskView) []registry.ProbeTaskView { return v[:6] }, model.VerdictNone, "insufficient_controls"},
		{"rotating incident exit", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView { v[3].PathKey = "different-ip"; return v }, model.VerdictNone, "insufficient_controls"},
		{"duplicate juror", model.ProbeResultClean, model.ProbeResultDegraded, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 3; i < len(v); i++ {
				v[i].Juror = 99
				v[i].Attempt.AccountID = 99
				v[i].ControlAttempt.AccountID = 99
				v[i].ControlAccountID = 99
			}
			return v
		}, model.VerdictNone, "insufficient_controls"},
		{"pending is not failure", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 0; i < 3; i++ {
				v[i].State = model.ProbePending
			}
			return v
		}, model.VerdictNone, "awaiting_probes"},
		{"worker cancelled is not failure", model.ProbeResultError, model.ProbeResultClean, func(v []registry.ProbeTaskView) []registry.ProbeTaskView {
			for i := 0; i < 3; i++ {
				v[i].State = model.ProbeCancelled
			}
			return v
		}, model.VerdictNone, "insufficient_controls"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tasks := experimentFixture(test.diff, test.jury)
			if test.edit != nil {
				tasks = test.edit(tasks)
			}
			got := assessExperiment(tasks, policy)
			if got.Verdict != test.verdict || got.Reason != test.reason {
				t.Fatalf("got %+v, want %s / %s", got, test.verdict, test.reason)
			}
			if got.Account.Pending+got.Account.Cancelled > 0 && got.Account.Transport != 0 {
				t.Fatal("uncompleted tasks became response failures")
			}
		})
	}
}

func experimentIdentity(id string, accountID, nodeID, epoch uint64) attemptmeta.Identity {
	return attemptmeta.Identity{ID: id, AccountID: accountID, Provider: "grok_build", Model: "grok-4.6", Revision: 1, RuleVersion: "r1", Path: attemptmeta.Path{NodeID: nodeID, Epoch: epoch, Status: attemptmeta.PathRegistered}}
}

func TestExperimentCannotAttributeWithoutComparablePhysicalEvidence(t *testing.T) {
	for _, scenario := range []string{"legacy", "control policy changed", "main policy changed", "old epoch", "rotating"} {
		t.Run(scenario, func(t *testing.T) {
			tasks := experimentFixture(model.ProbeResultDegraded, model.ProbeResultClean)
			for i := 0; i < 3; i++ {
				switch scenario {
				case "legacy":
					tasks[i].Attempt = attemptmeta.Identity{}
				case "control policy changed":
					tasks[i].ControlAttempt.Revision++
				case "main policy changed":
					tasks[i].Attempt.Revision++
					tasks[i].ControlAttempt.Revision++
				case "old epoch":
					tasks[i].Attempt.Path.Epoch++
				case "rotating":
					tasks[i].Attempt.Path.Rotating = true
				}
			}
			got := assessExperiment(tasks, policyFor(DefaultConfig(), time.Now()))
			if got.Verdict != model.VerdictNone || got.AccountCleared {
				t.Fatalf("unverified physical facts produced attribution: %+v", got)
			}
		})
	}
}
