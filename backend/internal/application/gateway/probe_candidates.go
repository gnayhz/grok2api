package gateway

import (
	"context"
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// candidateEligibility is the shared account policy for current claims and
// the read-only investigation candidate plan. Only production
// recovery may claim an exhausted account for a quota recovery attempt.
func (s *Selector) candidateEligibility(provider account.Provider, upstreamModel, quotaMode string, candidate account.RoutingCandidate, value account.Credential, accountScope clientkeydomain.AccountScope, now time.Time, inference, ignoreEgressLeaseBlock, bypassQualityEligibility, allowQuotaRecovery bool) error {
	if !accountScopeAllowsCandidate(provider, accountScope, candidate) {
		return &SelectionUnavailableError{Reason: SelectionNoAccounts}
	}
	if !value.Enabled || value.AuthStatus != account.AuthStatusActive {
		return &SelectionUnavailableError{Reason: SelectionNoAccounts}
	}
	if value.RiskStatus != "" {
		return &SelectionUnavailableError{Reason: SelectionNoAccounts}
	}
	if provider == account.ProviderBuild && s.excludeBuildBotFlaggedEnabled() && (value.BuildBotFlagSource == 1 || value.BuildBotFlagSource == 2) {
		return &SelectionUnavailableError{Reason: SelectionNoAccounts}
	}
	// 账号资格谓词缝隙(D3-1):生产钉住同样尊重质量资格;
	// 取证探测(bypassQualityEligibility)不受资格限制(B2 双通道)。
	if !bypassQualityEligibility {
		if eligibility := s.qualityEligibilityObserver(); eligibility != nil && !eligibility.AccountSchedulable(value.ID) {
			return &SelectionUnavailableError{Reason: SelectionNoAccounts}
		}
	}
	if inference {
		if !s.candidateSupportsModel(provider, upstreamModel, quotaMode, candidate) {
			return &SelectionUnavailableError{Reason: SelectionUnsupportedModel}
		}
		if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
			return &SelectionUnavailableError{Reason: SelectionModelCooling, RetryAfter: retryDelay(now, candidate.ModelQuotaBlock.CooldownUntil)}
		}
		if !ignoreEgressLeaseBlock && candidateEgressLeaseCooling(candidate, value, now) {
			return &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, candidate.EgressLeaseBlock.CooldownUntil)}
		}
		if value.CooldownUntil != nil && now.Before(*value.CooldownUntil) {
			return &SelectionUnavailableError{Reason: SelectionCooling, RetryAfter: retryDelay(now, *value.CooldownUntil)}
		}
		if recovery := candidate.QuotaRecovery; recovery != nil && recovery.Status != account.QuotaRecoveryStatusActive {
			if recovery.NextProbeAt == nil || now.Before(*recovery.NextProbeAt) {
				var retryAfter time.Duration
				if recovery.NextProbeAt != nil {
					retryAfter = retryDelay(now, *recovery.NextProbeAt)
				}
				return &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
			}
			if !allowQuotaRecovery {
				return &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
			}
			return nil
		}
		if candidate.Billing != nil && candidate.Billing.IsExhausted(value.MinimumRemaining) {
			return &SelectionUnavailableError{Reason: SelectionQuotaExhausted}
		}
		if s.quotaWindowExhausted(candidate) {
			var retryAfter time.Duration
			if candidate.QuotaWindow.ResetAt != nil {
				retryAfter = retryDelay(now, *candidate.QuotaWindow.ResetAt)
			}
			return &SelectionUnavailableError{Reason: SelectionQuotaExhausted, RetryAfter: retryAfter}
		}
	}
	return nil
}

// qualityProbeCandidates reads current routing facts without claiming capacity,
// credentials or quota recovery. Execution rechecks the same admission policy.
func (s *Selector) qualityProbeCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]uint64, error) {
	values, err := s.accounts.ListRoutingCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	health := s.routingHealthSnapshot(provider, now)
	scope, _ := clientkeydomain.NormalizeAccountScope(clientkeydomain.AccountScope{})
	ids := make([]uint64, 0, len(values))
	for _, c := range values {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		value := applyHealthSnapshot(c.Credential, health)
		if s.candidateEligibility(provider, upstreamModel, quotaMode, c, value, scope, now, true, true, true, false) == nil {
			ids = append(ids, value.ID)
		}
	}
	return ids, nil
}

// QualityProbeAccounts resolves the same frozen model as the real measurement.
// Court owns quality/identity filtering; Selector owns ordinary account eligibility.
func (s *Service) QualityProbeAccounts(ctx context.Context, experiment qualitymodel.ProbeExperiment) ([]uint64, error) {
	if s.selector == nil {
		return nil, errors.New("probe selector unavailable")
	}
	if experiment.Version != "" {
		ctx = qualitymodel.WithProbeExperiment(ctx, experiment)
	}
	route, err := s.qualityProbeRoute(ctx)
	if err != nil {
		return nil, err
	}
	return s.selector.qualityProbeCandidates(ctx, route.Provider, route.ID, route.UpstreamModel, s.providers.QuotaMode(route.Provider, route.UpstreamModel))
}
