// Package testsupport builds account fixtures through the same state transitions
// used by real consumers. It must only be imported by tests.
package testsupport

import (
	"context"
	"fmt"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func Billing(ctx context.Context, repo repository.AccountRepository, billing account.Billing) error {
	value, err := repo.Get(ctx, billing.AccountID)
	if err != nil {
		return err
	}
	result, err := repo.ApplyQuotaRecovery(ctx, value.QuotaRecoveryRef(), account.RecoveryEvent{Kind: account.RecoveryBillingObserved, Billing: &billing})
	if err == nil && !result.Applied {
		return fmt.Errorf("billing fixture rejected")
	}
	return err
}

func Recovery(ctx context.Context, repo repository.AccountRepository, recovery account.QuotaRecovery) error {
	value, err := repo.Get(ctx, recovery.AccountID)
	if err != nil {
		return err
	}
	now := recovery.UpdatedAt
	if now.IsZero() {
		now = time.Now().UTC()
	}
	due := now.Add(account.FreeQuotaRecoveryPause)
	if recovery.NextProbeAt != nil {
		due = *recovery.NextProbeAt
	}
	event := account.RecoveryEvent{Kind: account.RecoveryFreeExhausted, OccurredAt: due.Add(-account.FreeQuotaRecoveryPause), Used: recovery.ConfirmedUsed, Limit: recovery.ConfirmedLimit}
	if recovery.Kind == account.QuotaRecoveryKindPaid {
		event = account.RecoveryEvent{Kind: account.RecoveryBillingObserved, OccurredAt: now, Billing: &account.Billing{AccountID: value.ID, MonthlyLimit: 100, Used: 100, BillingPeriodEnd: due.Format(time.RFC3339Nano), SyncedAt: now}}
	}
	result, err := repo.ApplyQuotaRecovery(ctx, value.QuotaRecoveryRef(), event)
	if err != nil {
		return err
	}
	if !result.Applied {
		return fmt.Errorf("recovery fixture rejected")
	}
	if recovery.Status == account.QuotaRecoveryStatusProbing {
		result, err = repo.ApplyQuotaRecovery(ctx, result.Ref, account.RecoveryEvent{Kind: account.RecoveryProbeClaimed, OccurredAt: due})
		if err == nil && !result.Applied {
			return fmt.Errorf("probe fixture rejected")
		}
	}
	return err
}

func ModelRestriction(ctx context.Context, repo repository.AccountRepository, block account.ModelQuotaBlock) error {
	value, err := repo.Get(ctx, block.AccountID)
	if err != nil {
		return err
	}
	// Choose an observation time that yields the fixture's requested deadline
	// under the real policy, even for a confirmed Free account's fixed window.
	delay := account.FreeQuotaRecoveryPause
	if block.Reason == string(account.ModelAccessDenied) {
		delay = account.ModelAccessDeniedPause
	}
	_, err = repo.ApplyModelRestriction(ctx, value.QuotaRecoveryRef(), account.ModelRestrictionEvent{Kind: account.ModelRestrictionKind(block.Reason), UpstreamModel: block.UpstreamModel, RetryAfter: delay, OccurredAt: block.CooldownUntil.Add(-delay)})
	return err
}
