package gateway

import (
	"context"
	"sync"

	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// NewHistoryController 构造跨包可用的历史恢复控制器(恢复/回放测试与
// provider 侧测试的公共入口);进程内主路径由 service 直接构造同一状态。
// NewHistoryController freezes the logical request's downgrade policy
// and budget. Providers supply rejection facts and a same-scope sender only.
func NewHistoryController(mode historydomain.RecoveryMode, budget *inferencedomain.AttemptBudget) provider.HistoryController {
	return newHistoryRecoveryState(mode, budget)
}

type historyRecoveryState struct {
	mode    historydomain.RecoveryMode
	budget  *inferencedomain.AttemptBudget
	mu      sync.Mutex
	outcome historydomain.RecoveryOutcome
}

func newHistoryRecoveryState(mode historydomain.RecoveryMode, budget *inferencedomain.AttemptBudget) *historyRecoveryState {
	return &historyRecoveryState{mode: mode, budget: budget}
}
func (h *historyRecoveryState) observe(outcome historydomain.RecoveryOutcome) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.outcome.RemovedOpaque += outcome.RemovedOpaque
	h.outcome.SessionHintCleared = h.outcome.SessionHintCleared || outcome.SessionHintCleared
	h.outcome.Failed = h.outcome.Failed || outcome.Failed
}
func (h *historyRecoveryState) snapshot() historydomain.RecoveryOutcome {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.outcome
}
func (h *historyRecoveryState) annotate(response *provider.Response) {
	if response == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	provider.ApplyHistoryRecoveryWarnings(response.Header, h.outcome)
}
func (h *historyRecoveryState) Recover(ctx context.Context, request provider.HistoryRecoveryRequest) (result provider.HistoryRecoveryResult) {
	result = provider.HistoryRecoveryResult{Exchange: request.Original}
	defer func() { h.observe(result.Outcome) }()
	fail := func(reason string) provider.HistoryRecoveryResult {
		result.Outcome.Failed = true
		result.Outcome.Reason = reason
		return result
	}
	if h.mode != historydomain.AllowLossyRecovery {
		return fail("opaque_preserved")
	}
	steps, reason := historyapp.PlanRecovery(request.Input)
	if len(steps) == 0 {
		return fail(reason)
	}
	if request.Retry == nil {
		return fail("sender_unavailable")
	}
	for _, step := range steps {
		permit, err := h.budget.Reserve(ctx)
		if err != nil {
			return fail("attempt_not_authorized")
		}
		if request.History != nil {
			err = request.History.ApplyRecovery(ctx, request.Original.Prepared, request.Model, request.Key, step)
		} else {
			err = ctx.Err()
		}
		if err != nil {
			permit.Release()
			return fail("history_transition_failed")
		}
		next, err := request.Retry(portphysical.WithPhysicalCallPermit(ctx, permit), step)
		permit.Release()
		result.Outcome.Actions = append(result.Outcome.Actions, step.Action)
		result.Outcome.RemovedOpaque += step.RemovedOpaque
		result.Outcome.SessionHintCleared = result.Outcome.SessionHintCleared || step.ClearSessionHint
		if err != nil {
			historydomain.Discard(next.Prepared)
			closeHistoryExchange(next)
			return fail("recovery_exchange_failed")
		}
		if next.Accepted || next.RateLimited {
			closeHistoryExchange(request.Original)
			result.Exchange = next
			return result
		}
		historydomain.Discard(next.Prepared)
		closeHistoryExchange(next)
		if next.Rejection != historydomain.OpaqueDecodeRejected {
			return fail("recovery_rejected")
		}
	}
	return fail("recovery_exhausted")
}

func closeHistoryExchange(exchange provider.HistoryExchange) {
	if exchange.Response != nil && exchange.Response.Body != nil {
		_ = exchange.Response.Body.Close()
	}
}

// PrepareInput authorizes a proposed local input transformation; the history
// owner has made its loss explicit. It neither resets the journal nor sends an
// extra generation. Repeated account attempts retain one logical input fact.
func (h *historyRecoveryState) PrepareInput(ctx context.Context, plan historydomain.InputPreparation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if plan.UnavailableCompactions == 0 && !plan.IdentityContextUnavailable {
		return nil
	}
	if h.mode != historydomain.AllowLossyRecovery {
		if plan.IdentityContextUnavailable {
			return historydomain.ErrIdentityLossNotAuthorized
		}
		return historydomain.ErrCompactionLossNotAuthorized
	}
	h.mu.Lock()
	h.outcome.IdentityContextUnavailable = h.outcome.IdentityContextUnavailable || plan.IdentityContextUnavailable
	h.outcome.OmittedCompactions = max(h.outcome.OmittedCompactions, plan.UnavailableCompactions)
	h.mu.Unlock()
	return nil
}
