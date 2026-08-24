package app

import (
	"context"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/application/account/risk"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/rsc"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// rscCheckerAdapter projects the concrete infra RSC checker onto the risk
// package's CheckResult port, keeping the application layer free of infra types.
type rscCheckerAdapter struct {
	Checker *rsc.Checker
}

var _ risk.Checker = rscCheckerAdapter{}

func (a rscCheckerAdapter) Check(ctx context.Context, ssoToken string) risk.CheckResult {
	result := a.Checker.Check(ctx, ssoToken)
	return risk.CheckResult{
		Verdict: string(result.Verdict), BotFlagSource: result.BotFlagSource, BotFlagDetails: result.BotFlagDetails,
		RiskScore: result.RiskScore, HTTPStatus: result.HTTPStatus,
		Error: result.Error, CheckedAt: result.CheckedAt,
	}
}

// riskRelationalStore adapts the relational risk repository onto the risk
// package's Store port. It lives in the composition root (not inside the
// application layer) because only the wiring layer may couple application
// ports to the GORM persistence implementation.
type riskRelationalStore struct {
	Repo *relational.RiskRepository
}

var _ risk.Store = riskRelationalStore{}

func (s riskRelationalStore) GetRiskVerdict(ctx context.Context, accountID uint64) (risk.StoredVerdict, error) {
	verdict, err := s.Repo.GetRiskVerdict(ctx, accountID)
	if err != nil {
		if errors.Is(err, repository.ErrNotFound) {
			return risk.StoredVerdict{}, risk.ErrNotFound
		}
		return risk.StoredVerdict{}, err
	}
	return risk.StoredVerdict{
		Verdict: verdict.Verdict, BotFlagSrc: verdict.BotFlagSrc, BotFlagDtl: verdict.BotFlagDtl,
		RiskScore: verdict.RiskScore, HTTPStatus: verdict.HTTPStatus, Error: verdict.Error,
		Source: verdict.Source, CheckedAt: verdict.CheckedAt,
	}, nil
}

func (s riskRelationalStore) SaveRiskVerdict(ctx context.Context, accountID uint64, verdict risk.StoredVerdict) error {
	return s.Repo.SaveRiskVerdict(ctx, relational.AccountRiskVerdict{
		AccountID: accountID, Verdict: verdict.Verdict, BotFlagSrc: verdict.BotFlagSrc,
		BotFlagDtl: verdict.BotFlagDtl, RiskScore: verdict.RiskScore, HTTPStatus: verdict.HTTPStatus,
		Error: verdict.Error, Source: verdict.Source, CheckedAt: verdict.CheckedAt,
	})
}

func (s riskRelationalStore) DeleteRiskVerdict(ctx context.Context, accountID uint64) error {
	return s.Repo.DeleteRiskVerdict(ctx, accountID)
}

func (s riskRelationalStore) ListRiskyVerdictAccountIDs(ctx context.Context) ([]uint64, error) {
	return s.Repo.ListRiskyVerdictAccountIDs(ctx)
}

func (s riskRelationalStore) ListRiskyVerdictAccountIDsAfter(ctx context.Context, afterID uint64) ([]uint64, error) {
	return s.Repo.ListRiskyVerdictAccountIDsAfter(ctx, afterID)
}
