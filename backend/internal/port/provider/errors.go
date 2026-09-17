package provider

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

var (
	ErrAuthorizationPending = errors.New("authorization pending")
	ErrSlowDown             = errors.New("authorization polling too fast")
	ErrAuthorizationDenied  = errors.New("authorization denied")
	ErrCredentialLimit      = errors.New("credential count exceeds limit")
	// ErrUnauthorized 表示上游凭据未授权。视频创建路径把它视为明确拒绝，而不是不确定提交。
	ErrUnauthorized        = errors.New("upstream credential unauthorized")
	ErrBirthDateAlreadySet = errors.New("upstream birth date is already set")
)

// HTTPStatusError preserves the upstream status when a streaming or asynchronous Provider cannot return a Response.
type HTTPStatusError interface {
	error
	HTTPStatusCode() int
}

// RetryAfterError preserves a safe upstream retry delay when an adapter cannot
// return a Response, for example when a WebSocket handshake is rejected.
type RetryAfterError interface {
	error
	RetryAfterDuration() time.Duration
}

// RequestScopedError marks an upstream rejection that retrying with another
// account or egress cannot resolve.
type RequestScopedError interface {
	error
	RequestScopedFailure() bool
}

// PublicMessageError exposes a deliberately sanitized message that may cross
// the public API boundary. Provider errors must opt in; arbitrary Error()
// strings can contain upstream response bodies, tokens, cookies, or request
// diagnostics and therefore are never returned to clients by default.
type PublicMessageError interface {
	error
	PublicErrorMessage() string
}

// PolicyForbiddenError 由上游错误实现：HTTP 403 且响应体是结构化 JSON——
// 源站应用层策略拒绝（内容审核/端点资格等），不是浏览器会话挑战也不是
// 账号级故障。调用方对这类失败不应做 egress 重试或换号轮询。
type PolicyForbiddenError interface {
	error
	IsPolicyForbidden() bool
}

// ErrorHTTPStatus extracts the upstream HTTP status from a Provider error chain.
func ErrorHTTPStatus(err error) (int, bool) {
	var statusError HTTPStatusError
	if !errors.As(err, &statusError) {
		return 0, false
	}
	status := statusError.HTTPStatusCode()
	return status, status > 0
}

// VideoStage identifies which phase of an asynchronous video job failed.
type VideoStage string

const (
	// VideoStagePrepare is local work performed before the create request is sent.
	// It is not account-failover eligible because retrying deterministic local
	// validation or configuration failures against another credential is useless.
	VideoStagePrepare VideoStage = "prepare"
	// VideoStageCreate means the upstream explicitly rejected the create request.
	// Account failover is safe only for the retryable 4xx statuses selected by
	// the gateway; 5xx responses remain indeterminate because work may already
	// have been accepted before the server failed.
	VideoStageCreate VideoStage = "create"
	// VideoStageSubmitted means the create request may have reached upstream but
	// no usable job identifier was obtained. Retrying could duplicate work.
	VideoStageSubmitted VideoStage = "submitted"
	VideoStagePoll      VideoStage = "poll"
)

// VideoStageError records the asynchronous video phase without treating every
// create-path error as safe for account failover.
type VideoStageError struct {
	Stage  VideoStage
	Status int
	Err    error
}

func (e *VideoStageError) Error() string {
	if e == nil {
		return "video request failed"
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	if e.Status > 0 {
		return fmt.Sprintf("video %s failed with status %d", e.Stage, e.Status)
	}
	return fmt.Sprintf("video %s failed", e.Stage)
}

func (e *VideoStageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *VideoStageError) HTTPStatusCode() int {
	if e == nil {
		return 0
	}
	if e.Status > 0 {
		return e.Status
	}
	return ErrorHTTPStatusOrZero(e.Err)
}

// ErrorHTTPStatusOrZero extracts an upstream status or returns 0.
func ErrorHTTPStatusOrZero(err error) int {
	status, ok := ErrorHTTPStatus(err)
	if !ok {
		return 0
	}
	return status
}

// VideoErrorStage reports the video phase for an error chain.
func VideoErrorStage(err error) (VideoStage, bool) {
	var stageErr *VideoStageError
	if !errors.As(err, &stageErr) || stageErr == nil || stageErr.Stage == "" {
		return "", false
	}
	return stageErr.Stage, true
}

// VideoCreateFailureStage distinguishes an explicit upstream rejection from an
// indeterminate POST result. Explicit 4xx responses (including the 401
// sentinel) are rejections; transport errors and 5xx responses remain
// submitted because the upstream may already have accepted the job.
func VideoCreateFailureStage(err error) VideoStage {
	if errors.Is(err, ErrUnauthorized) {
		return VideoStageCreate
	}
	if status, ok := ErrorHTTPStatus(err); ok && status >= http.StatusBadRequest && status < http.StatusInternalServerError {
		return VideoStageCreate
	}
	return VideoStageSubmitted
}

// WrapVideoStage annotates err with the video phase and optional HTTP status.
func WrapVideoStage(stage VideoStage, status int, err error) error {
	if err == nil {
		return nil
	}
	var existing *VideoStageError
	if errors.As(err, &existing) {
		return err
	}
	if status <= 0 {
		status = ErrorHTTPStatusOrZero(err)
	}
	return &VideoStageError{Stage: stage, Status: status, Err: err}
}

// ErrorRetryAfter extracts a positive retry delay from an error chain.
func ErrorRetryAfter(err error) time.Duration {
	var retryError RetryAfterError
	if !errors.As(err, &retryError) {
		return 0
	}
	return max(0, retryError.RetryAfterDuration())
}

// IsRequestScopedError reports whether the Provider has positively classified
// the failure as request-scoped.
func IsRequestScopedError(err error) bool {
	var requestError RequestScopedError
	return errors.As(err, &requestError) && requestError.RequestScopedFailure()
}

// ErrorPublicMessage extracts a message that the Provider has explicitly
// classified as safe for clients.
func ErrorPublicMessage(err error) (string, bool) {
	var publicError PublicMessageError
	if !errors.As(err, &publicError) {
		return "", false
	}
	message := strings.TrimSpace(publicError.PublicErrorMessage())
	return message, message != ""
}

// IsPolicyForbidden 判定错误链中是否携带源站策略级 403。
func IsPolicyForbidden(err error) bool {
	var policy PolicyForbiddenError
	return errors.As(err, &policy) && policy.IsPolicyForbidden()
}

// MediaPostProcessingStage identifies a local processing stage that failed after media generation.
type MediaPostProcessingStage string

const (
	MediaPostProcessingDownload MediaPostProcessingStage = "download"
	MediaPostProcessingStorage  MediaPostProcessingStage = "storage"
)

// MediaPostProcessingError indicates that upstream media was created but download or storage failed.
// These errors must not trigger generation on another account or reduce the generating account's health.
type MediaPostProcessingError struct {
	Stage MediaPostProcessingStage
	Cause error
}

func (e *MediaPostProcessingError) Error() string {
	if e == nil || e.Cause == nil {
		return "media post-processing failed"
	}
	return fmt.Sprintf("media post-processing %s failed: %v", e.Stage, e.Cause)
}

func (e *MediaPostProcessingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// NewMediaPostProcessingError marks a download or storage error as non-retryable across accounts.
func NewMediaPostProcessingError(stage MediaPostProcessingStage, cause error) error {
	if cause == nil {
		return nil
	}
	return &MediaPostProcessingError{Stage: stage, Cause: cause}
}

// IsMediaPostProcessingError reports whether an error occurred during local processing after media generation.
func IsMediaPostProcessingError(err error) bool {
	var target *MediaPostProcessingError
	return errors.As(err, &target)
}

// CredentialRefreshError distinguishes permanent OAuth errors requiring reauthorization from temporary errors that can retry with backoff.
type CredentialRefreshError struct {
	Status  int
	Code    string
	Message string
	// Response is a bounded, redacted representation of the upstream OAuth
	// response. It is diagnostic only and must never contain credentials.
	Response   string
	Permanent  bool
	RetryAfter time.Duration
	Cause      error
}

func (e *CredentialRefreshError) Error() string {
	if e == nil {
		return "credential refresh failed"
	}
	if e.Code != "" {
		if e.Message != "" {
			return "credential refresh failed: " + e.Code + ": " + e.Message
		}
		return "credential refresh failed: " + e.Code
	}
	if e.Message != "" {
		return "credential refresh failed: " + e.Message
	}
	if e.Cause != nil {
		return "credential refresh failed: " + e.Cause.Error()
	}
	return "credential refresh failed"
}

func (e *CredentialRefreshError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}
