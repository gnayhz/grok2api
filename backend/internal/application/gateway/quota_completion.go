package gateway

import (
	"context"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (s *Service) finishQuotaConsumption(budget finalizationBudget, fact account.QuotaConsumption) error {
	if fact.Mode == "" || fact.Units <= 0 {
		return nil
	}
	var receipt account.QuotaConsumptionReceipt
	err := budget.run("quota_consumption", finalizationQuotaBudget, func(ctx context.Context) error {
		var err error
		receipt, err = s.accounts.ConsumeQuota(ctx, fact)
		return err
	})
	if err != nil {
		s.logger.Warn("quota_consumption_handoff_failed", "event_id", fact.EventID, "account_id", fact.AccountID, "mode", fact.Mode, "units", fact.Units, "error", err)
	}
	if err == nil && receipt.Projection != nil {
		s.selector.ApplyInvalidation(repository.InvalidationEvent{Kind: repository.InvalidationAccountQuotaChanged, AccountID: fact.AccountID, Quota: receipt.Projection})
	}
	return err
}
