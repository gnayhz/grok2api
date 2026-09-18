package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// admitResponse observes one successful upstream response before delivery.
// It records evidence and may retry, but never owns another physical attempt.
func (r *responseExecution) admitResponse(response *provider.Response, credential accountdomain.Credential, attempt int) attemptDecision {
	defer requestdiag.Stage(r.ctx, "quality_admission", time.Now())
	if !r.qualityHoldEnabled {
		return attemptProceed
	}
	// ConvertStream/ConvertJSON mean the body is still native
	// Responses; peek that shape before client conversion.
	proto := qualityPeekProtocol(r.operation, response)
	var replay io.ReadCloser
	var verdict QualityVerdict
	var peekUsage Usage
	var peekErr error
	var peekFingerprint qualityHoldFingerprint
	// 活跃度预算制度表（qualityLivenessSchedule）：按请求类（搜索工具/
	// 重推理/默认）给 created/evidence 预算——证据与依据见该函数注释。
	// 流式/非流式共用：预算只对流式生效；完整 body 判决与流式共用
	// 同一套证据规则。
	peekCfg := r.peekSchedule
	// 思考期望来自 resolved effort（含别名档位）：终态纯语义输出
	// （裸工具调用）在期望思考时按 missing-thinking 扣留，堵住
	// 语义放行出口的降智交付。
	peekCfg.ReasoningExpected = reasoningExpectedForEffort(r.normalizedMetadata.ReasoningEffort)
	if r.input.Streaming {
		replay, verdict, peekUsage, peekFingerprint, peekErr = peekQualityStreamReport(r.ctx, response.Body, proto, peekCfg)
	} else {
		// 非流式：完整 body 判决（零扣留延迟），证据规则与流式一致。
		replay, verdict, peekUsage, peekFingerprint, peekErr = peekQualityBodyReportWithBudget(response.Body, peekCfg, responsebuffer.FromContext(r.ctx))
	}
	response.Body = r.lease.OwnBody(replay)
	if data, release, ok := responsebuffer.Borrow(response.Body); ok {
		r.textFacts.observeJSON(response, data)
		release()
	}
	if r.input.Streaming {
		r.textFacts.observeStream(response, peekUsage, peekFingerprint.Completed, peekFingerprint.Failed)
	}
	if verdict == QualityWithhold {
		r.textFacts.withhold()
	}
	// Real-time guard observability: per-attempt withhold decision.
	// 日志中的 usage 是判决时刻快照而非终值（规则 1 早交付先于 usage
	// 帧到达）。
	r.service.logger.Info("quality_hold_verdict", "request_id", r.input.RequestID, "account_id", credential.ID, "protocol", proto, "streaming", r.input.Streaming, "verdict", string(verdict), "rule", peekFingerprint.Rule, "first_item", peekFingerprint.FirstItem, "has_thinking", peekFingerprint.HasThinking, "encrypted", peekFingerprint.Encrypted, "usage_output", peekUsage.OutputTokens, "usage_reasoning", peekUsage.ReasoningTokens, "peek_err", peekErr)

	if peekErr == nil {
		observed := QualityObservedAdmitted
		if verdict == QualityWithhold {
			observed = QualityObservedDegraded
			if response.Body != nil {
				_ = response.Body.Close()
			}
			r.lease.Release()
		}
		obs := QualityObservation{Attempt: response.Attempt, At: time.Now().UTC(),
			AccountID: credential.ID, NodeID: response.Attempt.Path.NodeID, Provider: string(r.route.Provider),
			Outcome: observed, Rule: peekFingerprint.Rule}
		if eventErr := r.service.recordQualityEvent(r.ctx, obs, r.holdCfg.AccountCooldown); eventErr != nil {
			if response.Body != nil {
				_ = response.Body.Close()
			}
			r.lease.Release()
			r.lastErr = eventErr
			r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable",
				PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", AccountID: credential.ID, AccountName: credential.Name, Cause: eventErr}
			return attemptStop
		}

	}

	if peekErr != nil {
		if replay != nil {
			_ = replay.Close()
		} else {
			_ = response.Body.Close()
		}
		r.lease.Release()
		r.lastErr = peekErr
		errorCode := qualityHoldRule(QualityStreamSignals{}, false, peekErr)
		if isClientRequestCancel(r.ctx, peekErr) {
			errorCode = "request_canceled"
		}
		if eventErr := r.service.recordQualityEvent(r.ctx, QualityObservation{Attempt: response.Attempt, At: time.Now().UTC(), AccountID: credential.ID, NodeID: response.Attempt.Path.NodeID, Provider: string(r.route.Provider), Outcome: QualityObservedRejected, Rule: peekFingerprint.Rule, ErrorCode: errorCode}, 0); eventErr != nil {
			r.lastErr = eventErr
			r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", Cause: eventErr}
			return attemptStop
		}
		if errors.Is(peekErr, errQualityChoices) {
			r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusBadGateway, Code: "unsupported_upstream_choices", PublicMessage: "上游返回了不支持的响应选项", AccountID: credential.ID, AccountName: credential.Name, Cause: peekErr}
			return attemptStop
		}
		if errors.Is(peekErr, responsebuffer.ErrExhausted) {
			r.lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "response_resource_exhausted", PublicMessage: "响应处理容量暂时不足，请稍后重试", AccountID: credential.ID, AccountName: credential.Name, Cause: peekErr}
			return attemptStop
		}
		if isClientRequestCancel(r.ctx, peekErr) {
			r.lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(r.ctx.Err(), peekErr)}
			return attemptStop
		}
		r.lastFailure = newTransportUpstreamFailure(peekErr, credential.ID, credential.Name)
		var streamFailure *qualityUpstreamFailure
		if errors.As(peekErr, &streamFailure) {
			r.failureAttempts.captureStreamFailure(credential, r.responseStartedAt, response, StreamFailureDiagnostic{
				Body: streamFailure.diagnostic.Body, BodyTruncated: streamFailure.diagnostic.BodyTruncated,
			})
			if streamFailure.invalidRequest {
				return attemptStop
			}
		}
		switch {
		case errors.Is(peekErr, errQualityCreatedTimeout):
			r.noteGuardSignal(GuardSignalCreatedTimeout)
		case errors.Is(peekErr, errQualityEvidenceTimeout):
			r.noteGuardSignal(GuardSignalEvidenceTimeout)
		case errors.Is(peekErr, errQualityEmptyStream):
			r.noteGuardSignal(GuardSignalEmptyStream)
		}

		if neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) || neterrorpkg.IsUpstreamStreamIdleTimeout(context.Cause(r.ctx)) || errors.Is(peekErr, errQualityEmptyStream) || errors.Is(peekErr, errQualityEvidenceTimeout) || errors.Is(peekErr, errQualityCreatedTimeout) {
			// 守卫空闲路径的尝试进审计明细（round 41：此前多账号轮换
			// 轨迹在 attempts 里不可见；对照 quality_hold 路径有明细）。
			r.failureAttempts.captureQualityIdle(credential, r.responseStartedAt, r.lastFailure.Code, response, peekFingerprint)
			logPrefix := "quality_peek_idle"
			if errors.Is(peekErr, errQualityEmptyStream) {
				logPrefix = "quality_peek_empty"
			}
			writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(r.ctx), finalizationTimeout)
			if markErr := r.service.selector.MarkQualityIdleFailure(writeCtx, credential, r.holdCfg.IdleAccountCooldown); markErr != nil {
				r.service.logger.Warn(logPrefix+"_cooldown_failed", "request_id", r.input.RequestID, "account_id", credential.ID, "error", markErr)
			} else {
				r.service.logger.Warn(logPrefix+"_retry", "request_id", r.input.RequestID, "account_id", credential.ID, "cooldown", r.holdCfg.IdleAccountCooldown)
			}
			writeCancel()
			// 空闲路径同样把出口节点加入本请求的排除集(G14:扣留后的
			// 重试必须换路);账号侧由 MarkQualityIdleFailure 短冷却承担。
			r.markDegradedEgress(response)
		}
		if shouldStopForNonAccountFingerprint(r.failureFingerprints, r.lastFailure) {
			return attemptStop
		}
		return attemptRetry
	}
	response.Body = replay
	// 调用方只报告真实可用性（路由候选 + 可安全重放）；账号切换
	// 预算由 commitQualityHold→admission.DecideRetry 独自判定。
	hasNextAccount := verdict == QualityWithhold && r.attemptPolicy.hasNext(attempt) && r.selection.HasAvailableCandidate(r.excluded, !r.quotaProbeAttempted)
	hasNextAccount = hasNextAccount && r.replaySafety.Safe
	commit := commitQualityHold(verdict, r.qualityAccountAttempts-1, r.holdCfg.MaxAttempts, hasNextAccount)
	if commit.KeepBody {
		// 交付尝试的判决规则落审计主行：rule=thinking 表示流内观察到
		// 可见思考增量；其余规则的 200 交付为 fail-open 形态（配合
		// QualityFailOpen），面板可据此识别"零思考交付"。
		r.auditBase.QualityRule = peekFingerprint.Rule

	}
	if verdict == QualityWithhold {
		r.noteGuardSignal(GuardSignalWithhold)
		// 出口节点加入本请求的排除集:后续重试换到别的路径。
		r.markDegradedEgress(response)
	}
	// 扣留只记在本请求的 attempt 明细上，不再 Create 第二条主审计。
	// 线上曾出现同一 requestId 两行 200：一行 quality_degraded
	// events=0，一行成功放行——列表看起来像「有的抓住有的漏」。
	if verdict == QualityWithhold {
		r.failureAttempts.captureQualityDegraded(credential, r.responseStartedAt, response, peekFingerprint)
	}
	switch commit.Action {
	case QualityActionRetry:
		_ = response.Body.Close()
		r.lease.Release()
		r.lastErr = errQualityDegraded
		r.lastFailure = &UpstreamFailure{
			HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
			PublicMessage: "上游模型暂不可用或缺少推理能力，请稍后重试", AccountID: credential.ID, AccountName: credential.Name,
			Cause: errQualityDegraded,
		}
		r.service.logger.Info("quality_degraded_retry", "request_id", r.input.RequestID, "account_id", credential.ID, "quality_attempt", r.qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
		return attemptRetry
	case QualityActionReject:
		guardStats.recordExhausted()
		// 耗尽拒绝的主行也带最终判决规则指纹:
		// 面板对 503 的归因不再需要逐条展开 attempt
		// 明细(attempt 级详情仍保留全量)。
		r.auditBase.QualityRule = peekFingerprint.Rule
		_ = response.Body.Close()
		r.lease.Release()
		r.lastErr = errQualityDegraded
		r.lastFailure = &UpstreamFailure{
			HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
			PublicMessage: "上游模型暂不可用或缺少推理能力，请稍后重试", AccountID: credential.ID, AccountName: credential.Name,
			Cause: errQualityDegraded,
		}
		r.service.logger.Info("quality_degraded_rejected", "request_id", r.input.RequestID, "account_id", credential.ID)
		return attemptStop
	case QualityActionDeliver:
	}
	return attemptProceed
}
