package investigator

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// ProbeState exposes facts, not network policy. CurrentEpochAt must read the
// persistent authority, including changes committed by another instance.
type ProbeState interface {
	CurrentEpochAt(context.Context, uint64) (uint64, bool, error)
	IdentityGroupOf(uint64) (uint64, []uint64)
}

type ProbeExecutor struct {
	resourceMeasurements ResourceCheckMeasurements
	resourceProgress     ResourceCheckProgress
	state                ProbeState
}

func NewProbeExecutor(state ProbeState) *ProbeExecutor { return &ProbeExecutor{state: state} }

func (e *ProbeExecutor) Execute(ctx context.Context, task model.ProbeTask) (model.ProbeTaskResult, error) {
	if err := ctx.Err(); err != nil {
		return interrupted(model.ProbeTaskResult{}), err
	}
	if task.Direction == model.ProbeResourceCheck || task.Direction == model.ProbeCaseProof {
		return e.executeResourceCheck(ctx, task)
	}
	return rejected(model.ProbeTaskResult{}, model.ProbeFailureExperiment, "unsupported_probe_direction"), ErrInadmissible
}

func (e *ProbeExecutor) identityContext(ctx context.Context, accountID uint64) context.Context {
	group, members := e.state.IdentityGroupOf(accountID)
	if len(members) > 1 {
		return model.WithProbeIdentity(ctx, group, true)
	}
	return model.WithProbeIdentity(ctx, accountID, false)
}

func rejected(result model.ProbeTaskResult, failure model.ProbeFailure, detail string) model.ProbeTaskResult {
	result.Outcome, result.FailureKind, result.Detail = model.ProbeResultError, string(failure), detail
	result.VerifiedIPChange, result.ControlVerified = false, false
	return result
}

func interrupted(result model.ProbeTaskResult) model.ProbeTaskResult {
	return rejected(result, model.ProbeFailureInterrupted, "worker_interrupted")
}
