package investigator

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ProbeState exposes facts, not network policy. CurrentEpochAt must read the
// persistent authority, including changes committed by another instance.
type ProbeState interface {
	CurrentEpochAt(context.Context, uint64) (uint64, bool, error)
	CaseEvidence(context.Context, uint64) (string, bool, error)
	IdentityGroupOf(uint64) (uint64, []uint64)
}

// Measurements performs exactly one physical generation per call, retaining
// its identity and failure provenance. Account/network admission remains with
// the gateway and its underlying owners.
type Measurements interface {
	ProbeAccountDifferentialOnPath(context.Context, uint64, uint64, uint64) model.ProbeMeasurement
	ProbeExitJury(context.Context, uint64, uint64) model.ProbeMeasurement
}

type ProbeExecutor struct {
	resourceMeasurements ResourceCheckMeasurements
	resourceProgress     ResourceCheckProgress
	state                ProbeState
	measurements         Measurements
	logger               *slog.Logger
}

func NewProbeExecutor(state ProbeState, measurements Measurements, logger *slog.Logger) *ProbeExecutor {
	if logger == nil {
		logger = slog.Default()
	}
	return &ProbeExecutor{state: state, measurements: measurements, logger: logger}
}

func (e *ProbeExecutor) Execute(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return interrupted(model.ProbeTaskResult{}), err
	}
	if task.Direction == model.ProbeAccountCheck {
		return e.executeAccountCheck(ctx, task)
	}
	if task.Direction == model.ProbeResourceCheck {
		return e.executeResourceCheck(ctx, task)
	}
	if task.Direction != model.ProbeAccountDifferential && task.Direction != model.ProbeExitJury {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailureExperiment, "unsupported_probe_direction"), ErrInadmissible
	}
	if reason := task.Experiment.UnsupportedReason(); reason != "" {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailureExperiment, reason), nil
	}
	if e.state == nil || e.measurements == nil {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailureConfiguration, "probe_executor_unconfigured"), nil
	}
	resolved, ok, err := e.resolveBaseline(ctx, task)
	if err != nil {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailurePersistence, "baseline_read_failed"), err
	}
	if !ok {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailurePath, "baseline_missing"), nil
	}
	task = resolved
	if detail, err := e.checkPaths(ctx, task); detail != "" || err != nil {
		return pathFailure(model.ProbeTaskResult{}, detail, err), err
	}
	ctx = model.WithProbeExperiment(ctx, task.Experiment)
	subject := task.DefendantAccountID
	if task.Direction == model.ProbeExitJury {
		subject = task.JurorAccountID
	}
	if subject == 0 {
		return rejected(model.ProbeTaskResult{}, model.ProbeFailureAccount, "probe_account_missing"), nil
	}
	var sample model.ProbeMeasurement
	if task.Direction == model.ProbeAccountDifferential {
		sample = e.measurements.ProbeAccountDifferentialOnPath(e.identityContext(ctx, subject), subject, task.BaselineNodeID, task.DefendantNodeID)
	} else {
		sample = e.measurements.ProbeExitJury(e.identityContext(ctx, subject), subject, task.DefendantNodeID)
	}
	result := sample.TaskResult()
	if err := ctx.Err(); err != nil {
		return interrupted(result), err
	}
	if detail, err := e.checkPaths(ctx, task); detail != "" || err != nil {
		return pathFailure(result, detail, err), err
	}
	if result.Outcome != model.ProbeResultError {
		if !model.ProbeIdentityMatches(sample.Attempt, subject, task.DefendantNodeID, task.DefendantEpoch) {
			result = rejected(result, model.ProbeFailureIdentity, "probe_identity_unverified")
		} else if !task.Experiment.Matches(sample.Attempt) {
			result = rejected(result, model.ProbeFailureExperiment, "probe_experiment_mismatch")
		}
	}
	// A control changes one variable, never overwrites the primary sample and
	// never retries it. The court decides how much evidence is sufficient.
	if needsMatchedControl(result) && task.ControlAccountID != 0 && task.ControlNodeID != 0 {
		detail, err := e.checkEpoch(ctx, task.ControlNodeID, task.ControlEpoch, "control")
		if err != nil {
			return pathFailure(result, detail, err), err
		}
		if detail != "" {
			result.ControlOutcome, result.ControlDetail = model.ProbeResultError, detail
		} else {
			controlAccount := task.ControlAccountID
			var control model.ProbeMeasurement
			if task.Direction == model.ProbeAccountDifferential {
				control = e.measurements.ProbeExitJury(e.identityContext(ctx, controlAccount), controlAccount, task.ControlNodeID)
			} else {
				controlAccount = subject
				control = e.measurements.ProbeAccountDifferentialOnPath(e.identityContext(ctx, subject), subject, task.DefendantNodeID, task.ControlNodeID)
			}
			projected := control.TaskResult()
			result.ControlAttempt, result.ControlOutcome, result.ControlDetail, result.ControlPathKey = control.Attempt, projected.Outcome, projected.Detail, control.PathKey
			if err := ctx.Err(); err != nil {
				return interrupted(result), err
			}
			if detail, err := e.checkPaths(ctx, task); detail != "" || err != nil {
				return pathFailure(result, detail, err), err
			}
			detail, err = e.checkEpoch(ctx, task.ControlNodeID, task.ControlEpoch, "control")
			if err != nil {
				return pathFailure(result, detail, err), err
			}
			result.ControlVerified = detail == "" &&
				model.ProbeIdentityMatches(sample.Attempt, subject, task.DefendantNodeID, task.DefendantEpoch) &&
				model.ProbeIdentityMatches(control.Attempt, controlAccount, task.ControlNodeID, task.ControlEpoch) &&
				task.Experiment.Matches(sample.Attempt) && task.Experiment.Matches(control.Attempt) &&
				sample.Attempt.ID != control.Attempt.ID && sample.PathKey != "" && control.PathKey != "" &&
				((task.Direction == model.ProbeAccountDifferential && control.PathKey == sample.PathKey) ||
					(task.Direction == model.ProbeExitJury && control.VerifiedIPChange && control.PathKey != sample.PathKey))
			if detail != "" {
				result.ControlDetail = detail
			}
		}
	}
	e.logger.Info("quality_probe_executed", "task", task.ID, "case", task.CaseID, "direction", task.Direction, "outcome", result.Outcome, "detail", result.Detail)
	return result, nil
}

// Local identity, policy, capacity and persistence failures cannot participate
// in attribution. Spending a control request cannot repair that missing primary
// evidence. Upstream availability failures still receive the matched control.
func needsMatchedControl(result model.ProbeTaskResult) bool {
	return result.Outcome == model.ProbeResultDegraded ||
		result.Outcome == model.ProbeResultError && model.ProbeFailure(result.FailureKind).SupportsAvailability()
}

func (e *ProbeExecutor) identityContext(ctx context.Context, accountID uint64) context.Context {
	group, members := e.state.IdentityGroupOf(accountID)
	if len(members) > 1 {
		return model.WithProbeIdentity(ctx, group, true)
	}
	return model.WithProbeIdentity(ctx, accountID, false)
}

// Old pending differential tasks recover only their original case's baseline;
// no extra request is sent to reconstruct opening evidence.
func (e *ProbeExecutor) resolveBaseline(ctx context.Context, task model.ProbeTask) (model.ProbeTask, bool, error) {
	if task.Direction != model.ProbeAccountDifferential || task.BaselineNodeID != 0 {
		return task, true, nil
	}
	raw, found, err := e.state.CaseEvidence(ctx, task.CaseID)
	if err != nil || !found {
		return task, false, err
	}
	var opening struct {
		Exit *struct {
			Node  uint64 `json:"node"`
			Epoch uint64 `json:"epoch"`
		} `json:"exit"`
	}
	if json.Unmarshal([]byte(raw), &opening) != nil || opening.Exit == nil || opening.Exit.Node == 0 {
		return task, false, nil
	}
	task.BaselineNodeID, task.BaselineEpoch = opening.Exit.Node, opening.Exit.Epoch
	return task, true, nil
}

func (e *ProbeExecutor) checkPaths(ctx context.Context, task model.ProbeTask) (string, error) {
	if detail, err := e.checkEpoch(ctx, task.DefendantNodeID, task.DefendantEpoch, "comparison"); detail != "" || err != nil {
		return detail, err
	}
	if task.Direction == model.ProbeAccountDifferential {
		return e.checkEpoch(ctx, task.BaselineNodeID, task.BaselineEpoch, "baseline")
	}
	return "", nil
}

func (e *ProbeExecutor) checkEpoch(ctx context.Context, node, epoch uint64, label string) (string, error) {
	if node == 0 {
		return label + "_path_missing", nil
	}
	current, known, err := e.state.CurrentEpochAt(ctx, node)
	if err != nil {
		return label + "_epoch_read_failed", err
	}
	if !known {
		return label + "_epoch_unknown", nil
	}
	if current != epoch {
		return label + "_epoch_stale", nil
	}
	return "", nil
}

func rejected(result model.ProbeTaskResult, failure model.ProbeFailure, detail string) model.ProbeTaskResult {
	result.Outcome, result.FailureKind, result.Detail = model.ProbeResultError, string(failure), detail
	result.VerifiedIPChange, result.ControlVerified = false, false
	return result
}

func interrupted(result model.ProbeTaskResult) model.ProbeTaskResult {
	return rejected(result, model.ProbeFailureInterrupted, "worker_interrupted")
}

func pathFailure(result model.ProbeTaskResult, detail string, err error) model.ProbeTaskResult {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return interrupted(result)
	}
	kind := model.ProbeFailurePath
	if err != nil {
		kind = model.ProbeFailurePersistence
	}
	return rejected(result, kind, detail)
}
