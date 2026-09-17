package physical

import (
	"context"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
)

type attemptBudgetKey struct{}
type attemptPermitKey struct{}

// WithPhysicalCallBudget carries an execution-owner budget to every transport
// submission. Network retries can consume it but cannot extend or replace it.
func WithPhysicalCallBudget(ctx context.Context, budget *inferencedomain.AttemptBudget) context.Context {
	if ctx.Value(attemptBudgetKey{}) != nil {
		return ctx
	}
	return context.WithValue(ctx, attemptBudgetKey{}, budget)
}
func WithPhysicalCallPermit(ctx context.Context, permit *inferencedomain.AttemptPermit) context.Context {
	return context.WithValue(ctx, attemptPermitKey{}, permit)
}
func AcquirePhysicalCallBudget(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	budget, _ := ctx.Value(attemptBudgetKey{}).(*inferencedomain.AttemptBudget)
	if budget == nil {
		return nil
	}
	if permit, _ := ctx.Value(attemptPermitKey{}).(*inferencedomain.AttemptPermit); permit != nil {
		consumed, err := permit.Consume(budget)
		if err != nil || consumed {
			return err
		}
	}
	permit, err := budget.Reserve(ctx)
	if err != nil {
		return err
	}
	_, err = permit.Consume(budget)
	if err != nil {
		permit.Release()
	}
	return err
}
