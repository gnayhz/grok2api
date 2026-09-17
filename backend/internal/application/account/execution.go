package account

import (
	"context"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// Execution is the account surface Gateway may use: credential generation,
// quota observation, rate-limit reconcile and reauth. Import, conversion and
// administrator list/update APIs are not part of this contract.
type Execution interface {
	Get(ctx context.Context, id uint64) (View, error)
	EnsureCredential(ctx context.Context, value accountdomain.Credential, force bool) (accountdomain.Credential, error)
	MarkReauthRequired(ctx context.Context, observed accountdomain.CredentialRef, reason string) error
	ObserveResponseModel(ctx context.Context, id uint64, model string) error
	QueueQuotaRefresh(id uint64, mode string)
	ConsumeQuota(ctx context.Context, value accountdomain.QuotaConsumption) (accountdomain.QuotaConsumptionReceipt, error)
	ProbePaidQuota(ctx context.Context, value accountdomain.Credential, ref accountdomain.QuotaRecoveryRef) (accountdomain.Credential, bool, error)
	ReconcileRateLimit(ctx context.Context, id uint64, mode string, retryAfter time.Duration) (RateLimitReconcileState, error)
	ReconcileWebRateLimit(ctx context.Context, id uint64, mode string, retryAfter time.Duration) (bool, error)
	ActiveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (TeamModelRateLimit, bool)
	ObserveTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) (TeamModelRateLimit, bool)
}

var _ Execution = (*Service)(nil)
