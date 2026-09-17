package inference

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
)

// clientError is the public HTTP projection of execution failure facts.
type clientError struct {
	Status            int
	Code              string
	Message           string
	Param             string
	AnthropicType     string
	RetryAfter        time.Duration
	HistoryRecovery   historydomain.RecoveryOutcome
	OmitAnthropicCode bool
}

// classifyClientError maps execution failures onto the public client contract.
func classifyClientError(err error) clientError {
	view := clientError{Status: http.StatusBadGateway, Code: "upstream_unavailable", Message: "上游服务暂不可用", AnthropicType: "api_error"}
	var validation *inferencedomain.RequestValidationError
	if errors.As(err, &validation) {
		return clientError{Status: http.StatusBadRequest, Code: validation.Code, Message: validation.Message, Param: validation.Param, AnthropicType: "invalid_request_error", OmitAnthropicCode: true}
	}
	var upstreamFailure *gateway.UpstreamFailure
	var selectionFailure *selector.SelectionUnavailableError
	switch {
	case errors.Is(err, gateway.ErrLedgerUnavailable):
		view.Status, view.Code, view.Message = http.StatusServiceUnavailable, "ledger_unavailable", gateway.ErrLedgerUnavailable.Error()
		view.AnthropicType = "overloaded_error"
	case errors.Is(err, clientkeyapp.ErrBillingLimit):
		view.Status, view.Code, view.Message = http.StatusTooManyRequests, "billing_limit_exceeded", clientkeyapp.ErrBillingLimit.Error()
		view.AnthropicType = "rate_limit_error"
	case errors.Is(err, clientkeyapp.ErrModelNotAllowed):
		view.Status, view.Code, view.Message = http.StatusForbidden, "model_not_allowed", clientkeyapp.ErrModelNotAllowed.Error()
		view.AnthropicType = "permission_error"
	case errors.Is(err, gateway.ErrModelNotFound):
		view.Status, view.Code, view.Message = http.StatusNotFound, "model_not_found", "模型不存在"
		view.AnthropicType = "not_found_error"
	case errors.Is(err, historydomain.ErrResponseRead), errors.Is(err, historydomain.ErrResponseDelete):
		view.Status, view.Code, view.Message = http.StatusServiceUnavailable, "response_state_unavailable", "Response 状态暂不可用，请重试"
		view.AnthropicType = "overloaded_error"
	case errors.Is(err, mediadomain.ErrVideoResourceRead):
		view.Status, view.Code, view.Message = http.StatusServiceUnavailable, "video_state_unavailable", "视频资源状态暂不可用，请重试"
		view.AnthropicType = "overloaded_error"
	case errors.Is(err, gateway.ErrResponseNotFound), errors.Is(err, mediadomain.ErrVideoNotFound):
		view.Status, view.Code, view.Message = http.StatusNotFound, "response_not_found", "Response 不存在或已过期"
		view.AnthropicType = "not_found_error"
	case errors.Is(err, modeldomain.ErrUnsupportedCapability):
		view.Status, view.Code, view.Message = http.StatusServiceUnavailable, "model_unavailable", err.Error()
		view.AnthropicType = "overloaded_error"
	case errors.Is(err, gateway.ErrResponseStateUnsupported), errors.Is(err, gateway.ErrConversationUnsupported):
		view.Status, view.Code, view.Message = http.StatusBadRequest, "unsupported_parameter", err.Error()
		view.AnthropicType = "invalid_request_error"
	case errors.Is(err, gateway.ErrVideoInputTooLarge), errors.Is(err, gateway.ErrVideoInputUnavailable), errors.Is(err, gateway.ErrVideoParameterInvalid):
		view.Status, view.Code, view.Message = http.StatusBadRequest, "invalid_request", err.Error()
		view.AnthropicType = "invalid_request_error"
	case errors.Is(err, gateway.ErrVideoOperationUnsupported):
		view.Status, view.Code, view.Message = http.StatusBadRequest, "unsupported_model", err.Error()
		view.AnthropicType = "invalid_request_error"
	case errors.As(err, &upstreamFailure):
		view.HistoryRecovery = upstreamFailure.HistoryRecovery
		if sanitizedUpstreamAvailability(upstreamFailure) {
			view.Code = upstreamFailure.ClientCredentialErrorCode()
			if upstreamFailure.QuotaExhausted || upstreamFailure.FreeQuotaExhausted || upstreamFailure.HTTPStatus == http.StatusPaymentRequired {
				view.Code = "upstream_unavailable"
			}
			view.Status, view.Message = http.StatusServiceUnavailable, credentialErrorMessage(view.Code)
			view.AnthropicType = "overloaded_error"
		} else {
			view.Status, view.Code, view.Message = upstreamFailure.HTTPStatus, clientFailureCode(upstreamFailure.Code), upstreamFailure.PublicMessage
			view.AnthropicType = "api_error"
			if upstreamFailure.Code == "upstream_header_timeout" {
				view.AnthropicType = "timeout_error"
			}
		}
		if !isUpstreamCredentialStatus(upstreamFailure.HTTPStatus) && upstreamFailure.RetryAfter > 0 {
			view.RetryAfter = upstreamFailure.RetryAfter
		}
		if view.Status == http.StatusTooManyRequests {
			view.AnthropicType = "rate_limit_error"
		}
	case errors.As(err, &selectionFailure):
		view.Status, view.Code, view.Message = selectionclientError(selectionFailure)
		view.AnthropicType = "overloaded_error"
		if view.Status == http.StatusTooManyRequests {
			view.AnthropicType = "rate_limit_error"
		}
		if selectionFailure != nil && selectionFailure.RetryAfter > 0 {
			view.RetryAfter = selectionFailure.RetryAfter
		}
	case errors.Is(err, gateway.ErrResponseAccountUnavailable), errors.Is(err, gateway.ErrNoAvailableAccount):
		view.Status, view.Code, view.Message = http.StatusServiceUnavailable, "upstream_unavailable", "当前没有可用的上游账号"
		view.AnthropicType = "overloaded_error"
	}
	return view
}

func clientFailureCode(code string) string {
	if code == gateway.ErrorQualityDegraded {
		return "upstream_degraded"
	}
	return code
}

func isUpstreamCredentialStatus(status int) bool {
	return status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusPaymentRequired
}

func sanitizedUpstreamAvailability(failure *gateway.UpstreamFailure) bool {
	return failure != nil && (isUpstreamCredentialStatus(failure.HTTPStatus) || failure.QuotaExhausted || failure.FreeQuotaExhausted)
}

func selectionclientError(failure *selector.SelectionUnavailableError) (int, string, string) {
	status, code, message := http.StatusServiceUnavailable, "upstream_unavailable", "当前没有可用的上游账号"
	if failure == nil {
		return status, code, message
	}
	status, code = failure.HTTPStatus(), failure.Code()
	if failure.Scope.IsRestricted() {
		message = failure.Error()
	} else {
		switch failure.Reason {
		case selector.SelectionCooling:
			message = "上游账号正在冷却"
		case selector.SelectionModelCooling:
			message = "上游账号的目标模型正在冷却"
		case selector.SelectionQuotaExhausted:
			message = "上游账号额度等待恢复"
		case selector.SelectionSaturated:
			message = "上游账号当前均达到并发上限"
		case selector.SelectionUnsupportedModel:
			message = "当前账号池不支持该模型"
		}
	}
	return status, code, message
}

func credentialErrorMessage(code string) string {
	if code == "permission-denied" {
		return "上游服务暂不可用，聊天端点访问被拒绝"
	}
	return "上游服务暂不可用"
}

// writeCredentialStatusUnavailable writes the "credential-state upstream →
// 503" response for delivery results whose upstream status means the account
// credential is unavailable, and returns the audit errorCode. This is the
// single home of that protocol shape (ARCHITECTURE.md: 失败事实→协议形状的
// 映射归 client_error.go); anthropic selects the error envelope dialect.
func writeCredentialStatusUnavailable(c *gin.Context, anthropic bool, upstreamStatus int, body io.Reader) string {
	code := readCredentialErrorCode(upstreamStatus, body)
	if anthropic {
		writeAnthropicError(c, http.StatusServiceUnavailable, "overloaded_error", credentialErrorMessage(code), code)
	} else {
		writeOpenAIError(c, http.StatusServiceUnavailable, code, credentialErrorMessage(code))
	}
	return "upstream_unavailable"
}

// streamFailureShape maps the shared completion-failure sentinels onto the
// public error code and message used by both the audit errorCode projection
// (handler writeResult) and the SSE abort trailer. ok=false means the error is
// not one of the shared sentinels; callers keep their own extra cases and
// defaults. Keeping this table in one place prevents the two consumers from
// drifting apart.
func streamFailureShape(err error) (code, message string, ok bool) {
	switch {
	case errors.Is(err, inferencedomain.ErrProviderStateCommit):
		return "provider_state_commit_failed", "上游会话状态保存失败", true
	case errors.Is(err, inferencedomain.ErrResponseOwnershipCommit):
		return "response_ownership_commit_failed", "响应归属保存失败", true
	case errors.Is(err, historydomain.ErrHistoryCommit):
		return "history_commit_failed", "会话历史提交失败", true
	case errors.Is(err, inferencedomain.ErrCompletionCommit):
		return "completion_commit_failed", "响应完成提交失败", true
	case errors.Is(err, errUpstreamStreamFailed):
		return "upstream_stream_error", "上游流式响应返回错误", true
	case errors.Is(err, neterror.ErrUpstreamStreamIdleTimeout):
		return "upstream_stream_idle_timeout", "上游流式响应长时间无数据", true
	case errors.Is(err, neterror.ErrUpstreamOutputLoop):
		return "upstream_output_loop", "上游输出陷入循环", true
	case errors.Is(err, errUpstreamStreamIncomplete):
		return "upstream_stream_incomplete", "上游流式响应未完整结束", true
	case errors.Is(err, responsecheck.ErrToolChoice):
		return "upstream_tool_choice_mismatch", "上游未返回请求要求的工具调用", true
	case errors.Is(err, responsecheck.ErrEmptyOutput):
		return "upstream_empty_output", "上游已结束但未返回答案或工具输出", true
	}
	return "", "", false
}
