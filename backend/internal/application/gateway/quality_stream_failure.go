package gateway

import (
	"bytes"
	"errors"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
)

var errQualityUpstreamFailure = errors.New("upstream stream reported failure")

type qualityUpstreamFailure struct {
	diagnostic     responsecheck.FailureDiagnostic
	invalidRequest bool
}

// Error is safe for ordinary logs. Only the audit recorder may retain the
// bounded, redacted diagnostic; upstream messages may contain private input.
func (e *qualityUpstreamFailure) Error() string { return errQualityUpstreamFailure.Error() }
func (e *qualityUpstreamFailure) Unwrap() error { return errQualityUpstreamFailure }

func observeQualityFailure(state *qualityScanState, payload []byte) bool {
	kind := jsonpeek.RootStringFieldScan(payload, "type")
	errorValue := jsonpeek.RootRawValue(payload, "error")
	response := jsonpeek.RootRawValue(payload, "response")
	if len(response) == 0 {
		response = payload
	}
	status := jsonpeek.RootStringFieldScan(response, "status")
	failed := kind == "error" || kind == "response.failed" || kind == "response.incomplete" ||
		status == "failed" || status == "incomplete" || status == "cancelled" || status == "canceled" ||
		(len(errorValue) > 0 && !bytes.Equal(bytes.TrimSpace(errorValue), []byte("null")))
	if !failed {
		return false
	}
	errorValue = jsonpeek.RootRawValue(response, "error")
	code := jsonpeek.RootStringFieldScan(errorValue, "code")
	if code == "" {
		code = jsonpeek.RootStringFieldScan(errorValue, "type")
	}
	if code == "" {
		code = jsonpeek.RootStringFieldScan(payload, "code")
	}
	state.failed, state.terminal, state.completed = true, true, false
	noteQualityEvent(state, kind)
	state.protocolErr = &qualityUpstreamFailure{
		diagnostic:     responsecheck.ProjectFailureDiagnostic(payload, diagnosticBodyLimit),
		invalidRequest: code == "invalid-argument" || code == "invalid_argument" || code == "invalid_request_error",
	}
	return true
}
