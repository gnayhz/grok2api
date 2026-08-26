package app

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/account/risk"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/rsc"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// rscCheckerAdapter projects the concrete infra RSC probe (SSO thinking
// probe by default, legacy homepage checker for rollback) onto the risk
// package's CheckResult port, keeping the application layer free of infra types.
type rscCheckerAdapter struct {
	Checker rsc.Probe
	// Source tags results with the probe method ("sso_probe"/"rsc") so the
	// risk service can reject cached cleans produced by a different method.
	Source string
}

var _ risk.Checker = rscCheckerAdapter{}

func (a rscCheckerAdapter) Check(ctx context.Context, ssoToken string) risk.CheckResult {
	result := a.Checker.Check(ctx, ssoToken)
	return risk.CheckResult{
		Verdict: string(result.Verdict), BotFlagSource: result.BotFlagSource, BotFlagDetails: result.BotFlagDetails,
		RiskScore: result.RiskScore, HTTPStatus: result.HTTPStatus,
		Error: result.Error, CheckedAt: result.CheckedAt, Source: a.Source, Suppressed: result.Suppressed,
	}
}

// buildProberAdapter projects the gateway's Build-native differential probe
// onto the risk package's BuildProber port (outcome vocabulary mapping).
type buildProberAdapter struct {
	Gateway *gateway.Service
}

var _ risk.BuildProber = buildProberAdapter{}

func (a buildProberAdapter) ProbeBuildThinking(ctx context.Context, accountID, degradedNodeID uint64) risk.BuildProbeResult {
	result := a.Gateway.ProbeBuildThinking(ctx, accountID, degradedNodeID)
	verdict := result.Outcome
	switch result.Outcome {
	case "clean", "denied", "error", "unconfigured":
	default:
		verdict = "error"
	}
	return risk.BuildProbeResult{
		Verdict:   verdict,
		Error:     result.Reason,
		Details:   result.Details,
		CheckedAt: time.Now().UTC(),
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
		Source: verdict.Source, CheckedAt: verdict.CheckedAt, OriginAccountID: verdict.OriginAccountID,
	}, nil
}

func (s riskRelationalStore) SaveRiskVerdict(ctx context.Context, accountID uint64, verdict risk.StoredVerdict) error {
	return s.Repo.SaveRiskVerdict(ctx, relational.AccountRiskVerdict{
		AccountID: accountID, Verdict: verdict.Verdict, BotFlagSrc: verdict.BotFlagSrc,
		BotFlagDtl: verdict.BotFlagDtl, RiskScore: verdict.RiskScore, HTTPStatus: verdict.HTTPStatus,
		Error: verdict.Error, Source: verdict.Source, CheckedAt: verdict.CheckedAt,
		OriginAccountID: verdict.OriginAccountID,
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

func (s riskRelationalStore) DeleteCleanVerdictsExceptSources(ctx context.Context, keepSources ...string) (int64, error) {
	return s.Repo.DeleteCleanVerdictsExceptSources(ctx, keepSources...)
}

func (s riskRelationalStore) MostRecentCleanVerdict(ctx context.Context, source string) (uint64, bool, error) {
	return s.Repo.MostRecentCleanVerdict(ctx, source)
}
