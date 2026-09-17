package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/retryafter"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type attemptDecision uint8

const (
	attemptProceed attemptDecision = iota
	attemptRetry
	attemptStop
)

// handleRetryableResponse applies account/model/quota consequences once.
// The caller alone decides whether another bounded physical attempt may run.
func (r *responseExecution) handleRetryableResponse(response *provider.Response, credential accountdomain.Credential) attemptDecision {
	retryAfter := retryafter.Header(response.Header.Get("Retry-After"), time.Now().UTC())
	body, _ := readRetryableBody(response.Body)
	r.lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
	if response.StatusCode == http.StatusTooManyRequests && response.RateLimit == nil {
		if metadata := r.service.parseRateLimit(body); metadata != nil {
			response.RateLimit = metadata
			if retryAfter <= 0 && metadata.RetryAfter > 0 {
				retryAfter = metadata.RetryAfter
			}
		}
	}
	buildForbiddenReauth := credential.Provider == accountdomain.ProviderBuild && r.service.shouldInvalidateBuildForbidden(r.lastFailure)
	if response.StatusCode == http.StatusTooManyRequests && response.RateLimit != nil && response.RateLimit.Model == r.route.UpstreamModel {
		rateLimitMeta := *response.RateLimit
		limited, known := r.service.accounts.ObserveTeamModelRateLimit(credential, r.route.UpstreamModel, rateLimitMeta, time.Now().UTC())
		// Without a team identity, retain account-scoped 429 handling.
		if known {
			r.lastFailure.AccountScoped = false
			r.lastFailure.Fingerprint = "429:team_model_rate_limit"
			r.lastFailure.RetryAfter = time.Until(limited.Until)
			r.lease.Release()
			r.lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
			r.service.logger.Warn("upstream_team_model_rate_limited", "request_id", r.input.RequestID, "provider", credential.Provider, "model", r.route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "scope", rateLimitMeta.Scope, "actual", rateLimitMeta.Actual, "limit", rateLimitMeta.Limit, "retry_after", r.lastFailure.RetryAfter)
			return attemptRetry
		}
	}

	failureHandled := false
	if r.lease.QuotaMode != "" && response.StatusCode == http.StatusTooManyRequests {
		state, reconcileErr := r.service.accounts.ReconcileRateLimit(r.ctx, credential.ID, r.lease.QuotaMode, retryAfter)
		r.service.applyRateLimitReconciliation(r.ctx, credential, response.StatusCode, retryAfter, state, reconcileErr)
		failureHandled = reconcileErr == nil && state == accountapp.RateLimitReconcileExhausted
	} else if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
		// The Free subscription signal is account-scoped, but its billing
		// period is not a reliable reset promise. Probe again after 24 hours.
		r.service.selector.MarkFreeQuotaExhausted(r.ctx, credential, used, limit)
		failureHandled = true
	} else if r.lastFailure.ModelQuotaExhausted {
		r.service.selector.MarkModelQuotaExhausted(r.ctx, credential, r.lease.Billing, r.route.UpstreamModel, retryAfter)
		failureHandled = true
	} else if r.lastFailure.FreeQuotaExhausted {
		r.service.selector.MarkFreeQuotaExhausted(r.ctx, credential, 0, 0)
		failureHandled = true
	} else if r.lastFailure.SpendingLimitBlocked || r.lastFailure.QuotaExhausted {
		err := r.service.selector.MarkPaymentQuotaExhausted(r.ctx, credential, selector.QuotaRecoveryHints{Billing: r.lease.Billing})
		failureHandled = err == nil
		if err != nil {
			r.service.logger.Error("account_quota_recovery_write_failed", "request_id", r.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
		}
	}
	// Definitive account-block signals only arise from 401/403, and both
	// statuses are fully handled (with continue) before this generic retry
	// path, so no blocked response can reach here.
	if buildForbiddenReauth {
		failureHandled = r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, fmt.Sprintf("%s upstream error code %s matched the invalidation policy", credential.Provider, r.lastFailure.UpstreamCode))
	} else if r.service.providers.SupportsCredentialRefresh(credential.Provider) && r.lastFailure.PermanentAccountDenial {
		if credential.Provider == accountdomain.ProviderBuild {
			// 默认 model-scoped，视频拒绝时配额/OAuth 仍可能可用。
			// 开启 markBuildChatDeniedAsReauth 时再额外标 reauth，便于号池摘除。
			// 同时写入模型 block，避免在候选缓存窗口内本请求再次选中。
			modelErr := r.service.selector.MarkModelAccessDenied(r.ctx, credential, r.route.UpstreamModel, retryAfter)
			failureHandled = modelErr == nil
			if modelErr != nil {
				r.service.logger.Error("account_model_access_denied_write_failed", "request_id", r.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "model", r.route.UpstreamModel, "error", modelErr)
			}
			if r.service.markBuildChatDeniedAsReauth.Load() {
				reauthHandled := r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
				failureHandled = failureHandled || reauthHandled
			}
		} else {
			failureHandled = r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
		}
	} else if r.service.providers.SupportsCredentialRefresh(credential.Provider) && r.lastFailure.CredentialRejected {
		failureHandled = r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, fmt.Sprintf("%s credential rejected", credential.Provider))
	}
	if r.lastFailure.AccountScoped && !failureHandled {
		r.service.selector.MarkFailure(r.ctx, credential, response.StatusCode, retryAfter)
	} else if !r.lastFailure.AccountScoped && response.StatusCode >= http.StatusInternalServerError {
		// Provider 级 5xx:本请求换号,跨请求短暂隔离该账号,但不累积持久
		// 失败计数(#999 防瞬态 5xx 级联成 exponential 冷却)。保留真实状态码
		// 用于诊断,应用显式软失败策略。
		if markErr := r.service.selector.MarkSoftFailure(r.ctx, credential, response.StatusCode, retryAfter); markErr != nil {
			r.service.logger.Warn("soft_failure_mark_failed", "account_id", credential.ID, "status", response.StatusCode, "error", markErr.Error())
		}
	}
	r.lease.Release()
	r.lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
	r.service.logger.Warn("upstream_request_failed", "request_id", r.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", r.lastFailure.UpstreamCode, "account_scoped", r.lastFailure.AccountScoped)
	if shouldStopForNonAccountFingerprint(r.failureFingerprints, r.lastFailure) {
		return attemptStop
	}
	return attemptRetry
}

func (r *responseExecution) fail() (*Result, error) {
	if failure := r.admission.failure(); failure != nil {
		var upstream *UpstreamFailure
		if errors.As(failure, &upstream) {
			r.lastFailure = upstream
		}
	}
	record := r.auditBase
	var resultErr error
	if r.lastFailure != nil {
		record.StatusCode = r.lastFailure.HTTPStatus
		record.ErrorCode = r.lastFailure.AuditCode()
		if r.lastFailure.AccountID != 0 {
			accountID := r.lastFailure.AccountID
			record.AccountID, record.AccountName = &accountID, r.lastFailure.AccountName
		}
		resultErr = r.lastFailure
	} else {
		if r.lastErr == nil {
			r.lastErr = ErrNoAvailableAccount
		}
		record.StatusCode, record.ErrorCode = http.StatusServiceUnavailable, "upstream_unavailable"
		var selectionFailure *selector.SelectionUnavailableError
		if errors.As(r.lastErr, &selectionFailure) {
			record.StatusCode, record.ErrorCode = selectionFailure.HTTPStatus(), selectionFailure.Code()
		}
		resultErr = fmt.Errorf("%w: %w", ErrNoAvailableAccount, r.lastErr)
	}
	record.DurationMS = time.Since(r.startedAt).Milliseconds()
	record.Attempts = r.failureAttempts.snapshot()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, r.egressTrace, r.route.Provider)
	r.service.finishUnhandedText(&record, r.textFacts, r.physicalCallCtx, r.qualityHoldEnabled)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), finalizationTimeout)
	defer cancel()
	if err := r.service.audits.Create(persistCtx, record); err != nil {
		r.service.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", r.input.RequestID, "error", err)
	}
	r.finishGuardOutcome(false)
	return nil, resultErr
}
