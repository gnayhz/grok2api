package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/retryafter"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
)

func (r *responseExecution) runAttempts() (*Result, error) {
	accountScope := r.input.ClientKey.AccountScope()
attemptLoop:
	for attempt := 0; r.attemptPolicy.allows(attempt); attempt++ {
		if r.requestBudget.Remaining() == 0 {
			if r.lastFailure == nil {
				r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "physical_attempt_limit", PublicMessage: "上游尝试次数达到安全上限", Cause: inferencedomain.ErrAttemptBudget}
			}
			break
		}

		if err := r.service.recordPhysicalEvents(r.physicalCallCtx); err != nil {
			r.lastErr = err
			r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "上游尝试记录暂时无法保存，请稍后重试", Cause: err}
			break
		}

		if r.qualityHoldEnabled {
			if err := r.service.checkQualityEventCapacity(r.ctx); err != nil {
				r.lastErr = err
				r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", Cause: err}
				break
			}
		}
		if r.physicalStarted && !r.replaySafety.Safe {
			break
		}
		if err := r.admission.failure(); err != nil {
			r.lastErr = err
			break
		}
		// Preserve the last observed failure rather than start another physical
		// generation with only a fraction of a second left for admission.
		if r.physicalStarted && !hasRetryAdmissionBudget(r.admission) {
			requestdiag.Failure(r.ctx, "retry", "admission", "insufficient_remaining_budget")
			break
		}
		// 账号切换预算唯一由 admission 收口：本闸门与 DecideRetry 共用
		// BudgetExhausted，覆盖本循环所有换号路径（传输失败、空输出、扣留
		// 重试），受守卫管辖的请求不会比守卫预算多耗上游账号。
		if r.qualityHoldEnabled && qualityBudgetExhausted(r.qualityAccountAttempts, r.holdCfg.MaxAttempts) {
			break
		}
		var err error
		selectionStarted := time.Now()
		if r.ownership != nil {
			r.lease, err = r.service.selector.AcquirePinnedForKey(r.ctx, r.route.Provider, r.ownership.AccountID, r.route.ID, r.route.UpstreamModel, r.quotaMode, true, accountScope)
		} else {
			if r.selection == nil {
				r.selection, err = r.service.selector.BeginSelectionSessionForKey(r.ctx, r.route.Provider, r.route.ID, r.route.UpstreamModel, r.quotaMode, r.affinityKey, r.excluded, !r.quotaProbeAttempted, accountScope)
			}
			if err == nil {
				r.lease, err = r.selection.Acquire(r.ctx, r.excluded, !r.quotaProbeAttempted)
			}
		}
		r.timing.markSelection(time.Since(selectionStarted))
		requestdiag.Stage(r.ctx, "account_selection", selectionStarted)
		if err != nil {
			if r.lastFailure == nil {
				r.lastErr = err
			}
			break
		}
		r.excluded[r.lease.Credential.ID] = true
		if limited, ok := r.service.accounts.ActiveTeamModelRateLimit(r.lease.Credential, r.route.UpstreamModel, time.Now().UTC()); ok {
			r.lease.Release()
			r.lastFailure = &UpstreamFailure{
				HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", PublicMessage: "上游请求频率受限",
				AccountID: r.lease.Credential.ID, AccountName: r.lease.Credential.Name,
				Fingerprint: "429:team_model_rate_limit", RetryAfter: time.Until(limited.Until),
			}
			r.lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
			r.service.logger.Warn("upstream_team_model_rate_limit_active", "request_id", r.input.RequestID, "account_id", r.lease.Credential.ID, "provider", r.route.Provider, "model", r.route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "retry_after", r.lastFailure.RetryAfter.Round(time.Second))
			// Stored Responses are pinned to one account. Return the cached 429
			// immediately instead of spinning until the cooldown expires or
			// replaying the request on the same account.
			if r.ownership != nil {
				break attemptLoop
			}
			attempt--
			continue
		}
		if r.lease.QuotaProbe {
			r.quotaProbeAttempted = true
		}
		if r.lease.QuotaProbeKind == accountdomain.QuotaRecoveryKindPaid {
			promoted, recovered, probeErr := r.service.accounts.ProbePaidQuota(r.ctx, r.lease.Credential, *r.lease.QuotaRecoveryRef)
			r.service.selector.MarkQuotaStateChanged(r.lease.Credential.Provider, r.lease.Credential.ID)
			if probeErr != nil || !recovered {
				r.lease.Release()
				r.lastErr = firstError(probeErr, fmt.Errorf("付费额度尚未恢复"))
				continue
			}
			r.lease.Credential = promoted
			r.lease.QuotaRecoveryRef = nil
			r.lease.QuotaProbe = false
			r.lease.QuotaProbeKind = ""
			r.lease.Billing = nil
		}
		credential, err := r.ensureCredential(r.lease.Credential, false)
		if err != nil {
			r.lease.Release()
			r.lastErr = err
			r.lastFailure = newCredentialUpstreamFailure(err, r.lease.Credential.ID, r.lease.Credential.Name)
			continue
		}
		if r.lease.QuotaRecoveryRef != nil {
			credential.QuotaRecoveryRevision = r.lease.QuotaRecoveryRef.Revision
		}
		if r.physicalStarted && !hasRetryAdmissionBudget(r.admission) {
			r.lease.SkipSelectorObservation()
			r.lease.Release()
			requestdiag.Failure(r.ctx, "retry", "admission", "insufficient_remaining_budget")
			break
		}
		if r.qualityHoldEnabled {
			r.qualityAccountAttempts++
		}
		response, err := r.forwardResponse(r.lease, credential, r.lease.Billing)
		if err != nil {
			if errors.Is(err, clientkeyapp.ErrBillingLimit) || errors.Is(err, clientkeyapp.ErrRuntimeUnavailable) {
				r.lease.SkipSelectorObservation()
				r.lease.Release()
				return nil, err
			}
			r.lease.Release()
			r.lastErr = err
			if errors.Is(err, portphysical.ErrPhysicalCallLimit) {
				r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "physical_attempt_limit", PublicMessage: "上游尝试次数达到安全上限", Cause: err}
				break
			}
			if r.ctx.Err() != nil || errors.Is(err, context.Canceled) {
				r.lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(r.ctx.Err(), err)}
				break
			}
			if isSSOCredentialRejected(err, credential) {
				r.service.markSSOCredentialRejected(r.ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				r.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			r.lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
			if !isRetryableTransportFailure(credential.Provider, err) {
				break
			}
			if !neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
				r.service.selector.MarkFailure(r.ctx, credential, 0, 0)
			}
			if shouldStopForNonAccountFingerprint(r.failureFingerprints, r.lastFailure) {
				break
			}
			continue
		}
		if r.imageFacts != nil {
			facts, _ := r.imageFacts.snapshot()
			if facts.OutputImages > 0 && response.StatusCode >= 400 {
				return r.handoffResponse(response, r.lease, credential, r.responseStartedAt), nil
			}
		}
		if response.RequestValidation != nil {
			_ = response.Body.Close()
			r.lease.SkipSelectorObservation()
			r.lease.Release()
			record := r.auditBase
			record.StatusCode = http.StatusBadRequest
			record.ErrorCode = response.RequestValidation.Code
			record.DurationMS = time.Since(r.startedAt).Milliseconds()
			record.CreatedAt = time.Now().UTC()
			record.Attempts = r.failureAttempts.snapshot()
			r.service.finishUnhandedText(&record, r.textFacts, r.physicalCallCtx, r.qualityHoldEnabled)
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), finalizationTimeout)
			defer cancel()
			if err := r.service.audits.Create(persistCtx, record); err != nil {
				r.service.logger.Error("request_validation_audit_write_failed", "event_id", record.EventID, "error", err)
			}
			r.finishGuardOutcome(false)
			return nil, response.RequestValidation
		}
		if response.ModelCatalogChanged {
			if syncer, ok := r.service.models.(accountModelSyncer); ok {
				syncer.QueueAccountSync(credential.ID)
			}
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if credential.AuthType == accountdomain.AuthTypeSSO {
				r.service.markSSOCredentialRejected(r.ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				r.lease.Release()
				r.lastErr = fmt.Errorf("%s SSO 凭据已失效", credential.Provider)
				r.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			if r.service.markPermanentlyUnrefreshableCredentialRejected(r.ctx, credential) {
				r.lease.Release()
				r.lastErr = accountapp.ErrCredentialRefreshPermanent
				r.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			refreshed, refreshErr := r.ensureCredential(credential, true)
			if refreshErr == nil {
				response, err = r.forwardResponse(r.lease, refreshed, r.lease.Billing)
				credential = refreshed
			}
			if refreshErr != nil || err != nil {
				if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
					r.service.markCredentialRejectedAfterPermanentRefresh(r.ctx, credential)
				}
				r.lease.Release()
				r.lastErr = firstError(refreshErr, err)
				if refreshErr != nil {
					r.lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
				} else if r.ctx.Err() != nil || errors.Is(err, context.Canceled) {
					r.lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(r.ctx.Err(), err)}
					break
				} else {
					r.lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(credential.Provider, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(r.failureFingerprints, r.lastFailure) {
						break attemptLoop
					}
				}
				continue
			}
			if response.StatusCode == http.StatusUnauthorized {
				body, _ := readRetryableBody(response.Body)
				// WithoutCancel+超时:客户端恰在此刻断开时, 失效标记不得静默丢失
				// (否则该账号留在池中继续被后续请求选中各自撞一次 401)。
				r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, "Grok Build OAuth credential rejected after refresh")
				r.service.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
				r.lease.Release()
				r.lastErr = fmt.Errorf("刷新后上游仍返回 401")
				r.lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, body, credential.ID, credential.Name)
				continue
			}
		}
		egressForbidden := r.service.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden
		finalEgressForbidden := egressForbidden && (attempt > 0 || !r.attemptPolicy.hasNext(attempt))
		// Classify 403 bodies before egress retry. Definitive blocked-account signals invalidate and rotate the account;
		// request-level safety rejections are returned as-is without account side effects;
		// all other 403 responses retain the egress retry path without penalizing the account.
		if response.StatusCode == http.StatusForbidden {
			retryAfter := retryafter.Header(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			r.lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if isTerminalRequestForbidden(credential.Provider, r.lastFailure) {
				// Deterministic request-scoped 403: restore the original body and return it
				// without OAuth refresh, account rotation, cooldown, or invalidation.
				response.Body = io.NopCloser(bytes.NewReader(body))
				r.lease.CompleteSelectorObservation(false)
				r.lease.Release()
				if r.lastFailure.SafetyRejection {
					r.service.logger.Warn("upstream_safety_rejection", "request_id", r.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", r.lastFailure.UpstreamCode)
				} else {
					r.service.logger.Warn("upstream_request_scoped_forbidden", "request_id", r.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", r.lastFailure.UpstreamCode)
				}
				// Fall through to the common success/error response path so the client receives the original 403.
			} else if r.lastFailure.AccountBlocked {
				failureHandled := r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
				if r.lastFailure.AccountScoped && !failureHandled {
					r.service.selector.MarkFailure(r.ctx, credential, response.StatusCode, retryAfter)
				}
				r.lease.Release()
				r.lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
				r.service.logger.Warn("upstream_request_failed", "request_id", r.input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", r.lastFailure.UpstreamCode, "account_scoped", r.lastFailure.AccountScoped, "account_blocked", true)
				continue
			} else if egressForbidden && !finalEgressForbidden {
				// A non-blocking 403 is an egress/browser-session failure and must not penalize the account.
				delete(r.excluded, credential.ID)
				if r.selection != nil {
					r.selection.RetryAccount(credential.ID)
				}
				r.lease.Release()
				r.lastErr = fmt.Errorf("上游出口会话被拒绝")
				continue
			} else {
				// Restore the consumed final non-blocking 403 body for the common response path.
				response.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		if isTerminalRequestForbidden(credential.Provider, r.lastFailure) {
			// already prepared as a terminal 403 response for the client
		} else if response.StatusCode >= 400 && (!isRetryable(response.StatusCode) || !isRetryableResponse(response, r.route.Provider)) {
			// Both nonretryable statuses and explicit retry refusals use the
			// same sanitized envelope; neither may expose raw upstream headers.
			body, _ := readRetryableBody(response.Body)
			r.lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			_ = response.Body.Close()
			r.lease.CompleteSelectorObservation(false)
			r.lease.Release()
			break attemptLoop
		} else if isRetryableResponse(response, r.route.Provider) && !finalEgressForbidden {
			if r.handleRetryableResponse(response, credential) == attemptStop {
				break attemptLoop
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			credential = r.service.selector.MarkSuccessWithRecovery(r.ctx, credential, r.lease.QuotaRecoveryRef)
			// 注：曾在此处记录上游响应头全量用于降智早期信号研究；
			// 直连矩阵证实 clean/降智头部完全一致（零判别力），已移除该噪声日志。
			switch r.admitResponse(response, credential, attempt) {
			case attemptRetry:
				continue
			case attemptStop:
				break attemptLoop
			}
			if err := prepareResponseDelivery(response, r.input.Streaming, r.textFacts); err != nil {
				if r.imageFacts != nil {
					facts, _ := r.imageFacts.snapshot()
					if facts.OutputImages > 0 {
						if response.Body != nil {
							_ = response.Body.Close()
						}
						return r.handoffResponse(imageCompatibilityFailure(err), r.lease, credential, r.responseStartedAt), nil
					}
				}
				if response.Body != nil {
					_ = response.Body.Close()
				}
				r.lease.Release()
				r.lastErr = err
				r.lastFailure = &UpstreamFailure{
					HTTPStatus: http.StatusBadGateway, Code: "response_conversion_failed",
					PublicMessage: "上游响应格式转换失败，请稍后重试", AccountID: credential.ID, AccountName: credential.Name,
					Cause: err,
				}
				if errors.Is(err, errResponseTerminalFailure) {
					r.lastFailure.Code = "upstream_response_incomplete"
					r.lastFailure.PublicMessage = "上游响应未成功完成，请稍后重试"
				}
				if errors.Is(err, responsecheck.ErrEmptyOutput) {
					r.lastFailure.Code = "upstream_empty_output"
					r.lastFailure.PublicMessage = "上游已结束但未返回答案或工具输出"
				}
				if errors.Is(err, responsebuffer.ErrExhausted) {
					r.lastFailure.HTTPStatus = http.StatusServiceUnavailable
					r.lastFailure.Code = "response_resource_exhausted"
					r.lastFailure.PublicMessage = "响应处理容量暂时不足，请稍后重试"
				}
				if isClientRequestCancel(r.ctx, err) {
					r.lastFailure.HTTPStatus, r.lastFailure.Code, r.lastFailure.PublicMessage = 499, "request_canceled", "请求已取消"
				}
				if r.qualityHoldEnabled {
					if eventErr := r.service.recordQualityEvent(r.ctx, QualityObservation{Attempt: response.Attempt, At: time.Now().UTC(),
						AccountID: credential.ID, NodeID: response.Attempt.Path.NodeID, Provider: string(r.route.Provider),
						Outcome: QualityObservedInterrupted, ErrorCode: r.lastFailure.Code}, 0); eventErr != nil {
						r.lastErr = eventErr
						r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable",
							PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", Cause: eventErr}
					}
				}
				// The complete JSON body has not reached the client. Retry only
				// replay-safe requests, within the existing physical attempt budget.
				if r.lastFailure.Code == "upstream_empty_output" {
					_ = r.failureAttempts.captureResponse(credential, r.responseStartedAt, response, err)
				}
				if r.lastFailure.Code == "upstream_empty_output" && r.replaySafety.Safe && r.attemptPolicy.hasNext(attempt) {
					r.markDegradedEgress(response)
					continue
				}
				break attemptLoop
			}
			if diagnostic := response.RecoveredPrimaryFailure; diagnostic != nil {
				recoveredFailure := newHTTPUpstreamFailure(diagnostic.StatusCode, diagnostic.Body, credential.ID, credential.Name)
				if recoveredFailure.AccountBlocked || (credential.Provider == accountdomain.ProviderBuild && r.service.shouldInvalidateBuildForbidden(recoveredFailure)) {
					reason := fmt.Sprintf("%s primary endpoint denied account access", credential.Provider)
					if !r.service.markReauthRequired(r.ctx, r.input.RequestID, credential, reason) {
						r.service.selector.MarkModelAccessDenied(r.ctx, credential, r.route.UpstreamModel, 0)
					}
				}
			}
		}
		return r.handoffResponse(response, r.lease, credential, r.responseStartedAt), nil
	}
	return r.fail()
}
