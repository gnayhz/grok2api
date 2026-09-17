package selector

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// CredentialRefreshSupport 是 /readyz 判定需要的最小凭据刷新能力面
// (组合根以 provider.Registry 满足)。
type CredentialRefreshSupport interface {
	SupportsCredentialRefresh(provider account.Provider) bool
}

// StartupUsable 是 /readyz 的 provider 可用性判定:复用调度资格
// (candidateEligibility)的账号硬条件子集——认证活跃、(可刷新凭据的)
// 未过期、账号冷却、模型能力、模型配额块、额度恢复态、账单余额与配额
// 窗口。它是有意的粗粒度信号:不检查 enabled/RiskStatus/BuildBotFlag
// 与质量资格,也不承诺此刻能领取;"所有候选都不可用"才意味着该
// provider 尚未就绪。规则归 M06,组合根只调用。
func StartupUsable(candidate account.RoutingCandidate, now time.Time, refresh CredentialRefreshSupport) bool {
	credential := candidate.Credential
	if credential.AuthType == "" || credential.AuthStatus != account.AuthStatusActive {
		return false
	}
	refreshable := credential.AuthType == account.AuthTypeOAuth
	if refresh != nil {
		refreshable = refresh.SupportsCredentialRefresh(credential.Provider)
	}
	if refreshable && !credential.ExpiresAt.IsZero() && !now.Before(credential.ExpiresAt) {
		return false
	}
	if credential.CooldownUntil != nil && now.Before(*credential.CooldownUntil) {
		return false
	}
	if candidate.ModelCapabilityKnown && !candidate.SupportsModel {
		return false
	}
	if candidate.ModelQuotaBlock != nil && now.Before(candidate.ModelQuotaBlock.CooldownUntil) {
		return false
	}
	if candidate.QuotaRecovery != nil && candidate.QuotaRecovery.Status != account.QuotaRecoveryStatusActive {
		return false
	}
	if candidate.Billing != nil && candidate.Billing.IsExhausted(credential.MinimumRemaining) {
		return false
	}
	return candidate.QuotaWindow == nil || candidate.QuotaWindow.Remaining > 0
}
