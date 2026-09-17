package selector

import (
	"context"
	"io"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

type SelectionSession = selectionSession
type QuotaRecoveryHints = quotaRecoveryHints

func NewAttemptResources(parent context.Context) (context.Context, *AttemptResources) {
	return newAttemptResources(parent)
}

func (l *accountLease) ReplaceResources(resources *AttemptResources) { l.replaceResources(resources) }
func (l *accountLease) OwnBody(body io.ReadCloser) io.ReadCloser     { return l.ownBody(body) }

func (s *Selector) BeginSelectionSessionForKey(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode, affinityKey string, excluded map[uint64]bool, allowQuotaProbe bool, requestedScope clientkeydomain.AccountScope) (*SelectionSession, error) {
	return s.beginSelectionSessionForKey(ctx, provider, modelRouteID, upstreamModel, quotaMode, affinityKey, excluded, allowQuotaProbe, requestedScope)
}

func (s *Selector) MarkSoftFailure(ctx context.Context, credential account.Credential, status int, retryAfter time.Duration) error {
	return s.markSoftFailure(ctx, credential, status, retryAfter)
}

func (s *Selector) MarkSuccessWithRecovery(ctx context.Context, credential account.Credential, probe *account.QuotaRecoveryRef) account.Credential {
	return s.markSuccess(ctx, credential, probe)
}

func (s *Selector) ProtectionStats() LocalProtectionStats { return s.localProtectionStats() }
func (s *Selector) HoldLocalQuality(accountID uint64, owner string, until time.Time) {
	s.holdLocalQuality(accountID, owner, until)
}

func (s *Selector) AcquireProbeResources(ctx context.Context, accountID uint64) (func(), error) {
	return s.acquireProbeResources(ctx, accountID)
}

func (s *Selector) QualityProbeCandidates(ctx context.Context, provider account.Provider, modelRouteID uint64, upstreamModel, quotaMode string) ([]uint64, error) {
	return s.qualityProbeCandidates(ctx, provider, modelRouteID, upstreamModel, quotaMode)
}

func (s *Selector) HasAccountStore() bool { return s != nil && s.accounts != nil }

func (s *Selector) Account(ctx context.Context, id uint64) (account.Credential, error) {
	return s.accounts.Get(ctx, id)
}

type OnceCloseBody = onceCloseBody
type AdmissionBody = admissionBody

const QualityMeasurementTimeout = qualityMeasurementTimeout

func (l *accountLease) SkipSelectorObservation() { l.skipSelectorObservation() }
func (l *accountLease) CompleteSelectorObservation(success bool) {
	l.completeSelectorObservation(success)
}

func (l *accountLease) MarkSelectorUpstreamStarted()             { l.markSelectorUpstreamStarted() }
func (r *attemptResources) Own(body io.ReadCloser) io.ReadCloser { return r.own(body) }
func (r *attemptResources) Close()                               { r.close() }

func (s *selectionSession) HasAvailableCandidate(excluded map[uint64]bool, allowQuotaProbe bool) bool {
	return s.hasAvailableCandidate(excluded, allowQuotaProbe)
}

func NewAdmissionBody(body io.ReadCloser, commit func() error) *AdmissionBody {
	return &admissionBody{ReadCloser: body, commit: commit}
}

// LocalQualityAllowed 是本地质量持有的只读观测投影(跨包集成测试断言
// "降智后账号被本地限制"使用;语义归 localQualityAllowed)。
func (s *Selector) LocalQualityAllowed(accountID uint64, now time.Time) bool {
	return s.localQualityAllowed(accountID, now)
}

// StickySessionKey 暴露粘性会话键的确定性构造,供跨包测试直接读取
// sticky 存储断言绑定关系。
func StickySessionKey(value string) string { return stickySessionKey(value) }

// ReplaceAccountStore 是集成测试缝:在构造后替换账号事实来源
// (健康阻塞桩等);生产装配直接在 NewSelector 注入。
func (s *Selector) ReplaceAccountStore(accounts RoutingStore) { s.accounts = accounts }
