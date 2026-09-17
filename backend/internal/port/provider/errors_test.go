package provider

import (
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type statusError struct{ status int }

func (e statusError) Error() string       { return http.StatusText(e.status) }
func (e statusError) HTTPStatusCode() int { return e.status }

type retryError struct {
	error
	delay time.Duration
}

func (e retryError) RetryAfterDuration() time.Duration { return e.delay }

type scopedError struct {
	error
	scoped bool
}

func (e scopedError) RequestScopedFailure() bool { return e.scoped }

type publicError struct {
	error
	message string
}

func (e publicError) PublicErrorMessage() string { return e.message }

type policyError struct {
	error
	forbidden bool
}

func (e policyError) IsPolicyForbidden() bool { return e.forbidden }

func TestErrorHTTPStatus(t *testing.T) {
	t.Parallel()
	if status, ok := ErrorHTTPStatus(errors.New("plain")); ok || status != 0 {
		t.Fatalf("plain error status = %d ok=%v", status, ok)
	}
	if status, ok := ErrorHTTPStatus(statusError{status: http.StatusTooManyRequests}); !ok || status != http.StatusTooManyRequests {
		t.Fatalf("typed status = %d ok=%v", status, ok)
	}
	if status, ok := ErrorHTTPStatus(statusError{}); ok || status != 0 {
		t.Fatalf("zero status = %d ok=%v", status, ok)
	}
	if got := ErrorHTTPStatusOrZero(statusError{status: 502}); got != 502 {
		t.Fatalf("ErrorHTTPStatusOrZero = %d", got)
	}
}

func TestVideoCreateFailureStageIsFailClosed(t *testing.T) {
	t.Parallel()
	if stage := VideoCreateFailureStage(errors.New("connection reset after write")); stage != VideoStageSubmitted {
		t.Fatalf("transport failure stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(fmt.Errorf("wrapped: %w", statusError{status: http.StatusTooManyRequests})); stage != VideoStageCreate {
		t.Fatalf("explicit 429 stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(ErrUnauthorized); stage != VideoStageCreate {
		t.Fatalf("explicit unauthorized stage = %q", stage)
	}
	if stage := VideoCreateFailureStage(statusError{status: http.StatusInternalServerError}); stage != VideoStageSubmitted {
		t.Fatalf("explicit 500 stage = %q", stage)
	}
}

func TestWrapVideoStage(t *testing.T) {
	t.Parallel()
	if err := WrapVideoStage(VideoStageCreate, 400, nil); err != nil {
		t.Fatalf("nil err wrapped = %v", err)
	}
	first := WrapVideoStage(VideoStageCreate, 0, statusError{status: 429})
	stage, ok := VideoErrorStage(first)
	if !ok || stage != VideoStageCreate {
		t.Fatalf("stage = %q ok=%v", stage, ok)
	}
	if status, ok := ErrorHTTPStatus(first); !ok || status != 429 {
		t.Fatalf("wrapped status = %d ok=%v", status, ok)
	}
	second := WrapVideoStage(VideoStagePoll, 500, first)
	if second != first {
		t.Fatalf("re-wrap changed identity")
	}
}

func TestErrorHelpers(t *testing.T) {
	t.Parallel()
	if delay := ErrorRetryAfter(retryError{error: errors.New("busy"), delay: 2 * time.Second}); delay != 2*time.Second {
		t.Fatalf("retry after = %s", delay)
	}
	if delay := ErrorRetryAfter(retryError{error: errors.New("busy"), delay: -time.Second}); delay != 0 {
		t.Fatalf("negative retry after = %s", delay)
	}
	if IsRequestScopedError(errors.New("plain")) || !IsRequestScopedError(scopedError{error: errors.New("no"), scoped: true}) {
		t.Fatal("request-scoped classification")
	}
	if IsRequestScopedError(scopedError{error: errors.New("no"), scoped: false}) {
		t.Fatal("opt-out request-scoped must not classify")
	}
	message, ok := ErrorPublicMessage(publicError{error: errors.New("raw"), message: "  safe  "})
	if !ok || message != "safe" {
		t.Fatalf("public message = %q ok=%v", message, ok)
	}
	if _, ok := ErrorPublicMessage(errors.New("raw")); ok {
		t.Fatal("plain error must not opt into public message")
	}
	if IsPolicyForbidden(errors.New("plain")) || !IsPolicyForbidden(policyError{error: errors.New("denied"), forbidden: true}) {
		t.Fatal("policy forbidden classification")
	}
}

func TestMediaPostProcessingError(t *testing.T) {
	t.Parallel()
	if err := NewMediaPostProcessingError(MediaPostProcessingDownload, nil); err != nil {
		t.Fatalf("nil cause = %v", err)
	}
	cause := errors.New("disk")
	err := NewMediaPostProcessingError(MediaPostProcessingStorage, cause)
	if !IsMediaPostProcessingError(err) {
		t.Fatal("expected post-processing error")
	}
	if !errors.Is(err, cause) {
		t.Fatal("unwrap")
	}
}

func TestCredentialRefreshError(t *testing.T) {
	t.Parallel()
	cause := errors.New("oauth")
	err := &CredentialRefreshError{Code: "invalid_grant", Message: "expired", Permanent: true, Cause: cause}
	if !errors.Is(err, cause) {
		t.Fatal("unwrap")
	}
	if got := err.Error(); got != "credential refresh failed: invalid_grant: expired" {
		t.Fatalf("error = %q", got)
	}
}
