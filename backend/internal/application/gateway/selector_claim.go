package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// selectionCriteria carries the permission for a physical claim independently
// of the candidate snapshot used to order accounts. A session retains its
// original key scope while each claim reads current account facts.
type selectionCriteria struct {
	provider               account.Provider
	modelRouteID           uint64
	upstreamModel          string
	quotaMode              string
	accountScope           clientkeydomain.AccountScope
	inference              bool
	ignoreEgressLeaseBlock bool
	allowQuotaRecovery     bool
}

func (c selectionCriteria) withQuotaRecovery() selectionCriteria {
	c.allowQuotaRecovery = true
	return c
}

func (session *selectionSession) criteria() selectionCriteria {
	return selectionCriteria{provider: session.provider, modelRouteID: session.modelRouteID,
		upstreamModel: session.upstreamModel, quotaMode: session.quotaMode,
		accountScope: session.accountScope, inference: true}
}

func (s *Selector) currentClaimCandidate(ctx context.Context, candidate account.RoutingCandidate, criteria selectionCriteria, tracker *selectionClaimTracker) (account.RoutingCandidate, error) {
	if err := ctx.Err(); err != nil {
		return account.RoutingCandidate{}, err
	}
	if s.accounts != nil {
		current, err := s.accounts.GetRoutingCandidate(ctx, candidate.Credential.ID, criteria.provider, criteria.modelRouteID, criteria.upstreamModel, criteria.quotaMode)
		if err != nil {
			if ctx.Err() != nil {
				return account.RoutingCandidate{}, ctx.Err()
			}
			if errors.Is(err, repository.ErrNotFound) {
				s.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountStateChanged, Provider: criteria.provider, AccountID: candidate.Credential.ID})
				return account.RoutingCandidate{}, tracker.reject(&SelectionUnavailableError{Reason: SelectionNoAccounts})
			}
			return account.RoutingCandidate{}, fmt.Errorf("加载账号 %d 的当前路由事实: %w", candidate.Credential.ID, err)
		}
		if current.Credential.ID != candidate.Credential.ID || current.Credential.Provider != criteria.provider {
			return account.RoutingCandidate{}, tracker.reject(&SelectionUnavailableError{Reason: SelectionNoAccounts})
		}
		candidate = current
	}
	if err := ctx.Err(); err != nil {
		return account.RoutingCandidate{}, err
	}
	now := time.Now().UTC()
	candidate.Credential = s.applyRoutingHealth(candidate.Credential, now)
	// This is the ordinary account policy only. The final quality authority is
	// checked after capacity and material, not replaced by its cached hint here.
	if err := s.candidateEligibility(criteria.provider, criteria.upstreamModel, criteria.quotaMode, candidate, candidate.Credential, criteria.accountScope, now, criteria.inference, criteria.ignoreEgressLeaseBlock, true, criteria.allowQuotaRecovery); err != nil {
		var unavailable *SelectionUnavailableError
		if errors.As(err, &unavailable) {
			return account.RoutingCandidate{}, tracker.reject(unavailable)
		}
		return account.RoutingCandidate{}, err
	}
	return candidate, nil
}

func (tracker *selectionClaimTracker) reject(unavailable *SelectionUnavailableError) error {
	if tracker != nil {
		// Match the initial pool's preference for an actionable refusal over
		// accounts that simply no longer belong to the eligible set.
		if tracker.refusal == nil || selectionRefusalRank(unavailable.Reason) > selectionRefusalRank(tracker.refusal.Reason) {
			copy := *unavailable
			tracker.refusal = &copy
		} else if tracker.refusal.Reason == unavailable.Reason && unavailable.RetryAfter > 0 && (tracker.refusal.RetryAfter == 0 || unavailable.RetryAfter < tracker.refusal.RetryAfter) {
			tracker.refusal.RetryAfter = unavailable.RetryAfter
		}
	}
	return fmt.Errorf("%w: %w", errRoutingCredentialStale, unavailable)
}

func selectionRefusalRank(reason SelectionUnavailableReason) int {
	switch reason {
	case SelectionModelCooling:
		return 4
	case SelectionCooling:
		return 3
	case SelectionQuotaExhausted:
		return 2
	case SelectionUnsupportedModel:
		return 1
	default:
		return 0
	}
}

func (tracker *selectionClaimTracker) unavailableError() error {
	if tracker != nil && tracker.refusal != nil {
		copy := *tracker.refusal
		return &copy
	}
	return &SelectionUnavailableError{Reason: SelectionNoAccounts}
}
