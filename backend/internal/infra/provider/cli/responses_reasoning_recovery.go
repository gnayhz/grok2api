package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type reasoningRecoveryOutcome struct {
	prepared historydomain.Prepared
	outcome  historydomain.RecoveryOutcome
}

func (o reasoningRecoveryOutcome) merge(other reasoningRecoveryOutcome) reasoningRecoveryOutcome {
	o.outcome.Actions = append(o.outcome.Actions, other.outcome.Actions...)
	o.outcome.RemovedOpaque += other.outcome.RemovedOpaque
	o.outcome.SessionHintCleared = o.outcome.SessionHintCleared || other.outcome.SessionHintCleared
	o.outcome.Failed = o.outcome.Failed || other.outcome.Failed
	if other.outcome.Reason != "" {
		o.outcome.Reason = other.outcome.Reason
	}
	o.prepared = other.prepared
	return o
}
func (o reasoningRecoveryOutcome) appendWarnings(header http.Header) {
	provider.ApplyHistoryRecoveryWarnings(header, o.outcome)
}

// The adapter interprets a pre-generation rejection and supplies a bound sender.
// Only the request owner's controller may decide history changes and retries.
func (a *Adapter) recoverReasoningDecodeFailure(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base, replayKey string, response *http.Response, requestURL string, prepared historydomain.Prepared) (*http.Response, string, reasoningRecoveryOutcome) {
	if request.DisableAutomaticReplay || response == nil || response.StatusCode != http.StatusBadRequest {
		return response, requestURL, reasoningRecoveryOutcome{}
	}
	exchange, err := inspectHistoryExchange(response, requestURL, prepared)
	if err != nil || exchange.Rejection == "" {
		return exchange.Response, requestURL, reasoningRecoveryOutcome{}
	}
	if request.HistoryControl == nil {
		return exchange.Response, requestURL, reasoningRecoveryOutcome{outcome: historydomain.RecoveryOutcome{Failed: true, Reason: "recovery_not_authorized"}}
	}
	result := request.HistoryControl.Recover(ctx, provider.HistoryRecoveryRequest{
		Input: historydomain.RecoveryInput{Rejection: exchange.Rejection, Method: request.Method, Body: body, PromptCacheKey: request.PromptCacheKey},
		Model: request.Model, Key: replayKey, History: a.replay, Original: exchange,
		Retry: func(retryCtx context.Context, step historydomain.RecoveryStep) (provider.HistoryExchange, error) {
			return a.retryReasoningRecovery(retryCtx, request, accessToken, step, base)
		},
	})
	outcome := reasoningRecoveryOutcome{outcome: result.Outcome}
	if result.Exchange.Response != exchange.Response {
		outcome.prepared = result.Exchange.Prepared
	}
	if a.logger != nil {
		a.logger.Warn("reasoning_decode_recovery", "account_id", request.Credential.ID, "model", request.Model, "operation", request.Operation,
			"actions", result.Outcome.Actions, "reason", result.Outcome.Reason, "failed", result.Outcome.Failed)
	}
	return result.Exchange.Response, result.Exchange.URL, outcome
}

func (a *Adapter) retryReasoningRecovery(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, step historydomain.RecoveryStep, base string) (provider.HistoryExchange, error) {
	request.IdempotencyID, _ = security.NewOpaqueToken(18)
	stage := "reasoning_replay"
	if step.ClearSessionHint {
		request.PromptCacheKey = ""
		request.GrokTurnIndex = ""
		stage = "reasoning_session_reset"
	}
	body, _, prepared, err := a.prepareReasoningReplay(ctx, request, step.Body, base)
	if err != nil {
		return provider.HistoryExchange{}, err
	}
	response, url, err := a.doResponseRequest(infraegress.WithPhysicalCallStage(ctx, stage), request, accessToken, body, base)
	if err != nil {
		return provider.HistoryExchange{Response: response, URL: url, Prepared: prepared}, err
	}
	if err = normalizeGzipResponse(response); err != nil {
		return provider.HistoryExchange{Response: response, URL: url, Prepared: prepared}, err
	}
	return inspectHistoryExchange(response, url, prepared)
}

func inspectHistoryExchange(response *http.Response, url string, prepared historydomain.Prepared) (provider.HistoryExchange, error) {
	result := provider.HistoryExchange{Response: response, URL: url, Prepared: prepared}
	if response == nil {
		return result, nil
	}
	result.Accepted = isHTTPSuccess(response.StatusCode)
	result.RateLimited = response.StatusCode == http.StatusTooManyRequests
	if response.StatusCode != http.StatusBadRequest {
		return result, nil
	}
	body, truncated, err := provider.ReadDiagnosticBody(response.Body)
	_ = response.Body.Close()
	result.Response = cloneBufferedResponse(response, body, truncated)
	if err == nil && !truncated && isReasoningDecodeFailure(body) {
		result.Rejection = historydomain.OpaqueDecodeRejected
	}
	return result, err
}

// Accept only the explicit error envelope. Request echoes, output items, HTML,
// truncated diagnostics and unrelated 400 responses cannot authorize recovery.
func isReasoningDecodeFailure(body []byte) bool {
	var payload struct {
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	var message string
	if json.Unmarshal(payload.Error, &message) != nil {
		var detail struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(payload.Error, &detail) != nil {
			return false
		}
		message = detail.Message
	}
	lower := strings.ToLower(strings.TrimSpace(message))
	return strings.HasPrefix(lower, "could not decode the compaction blob") || strings.HasPrefix(lower, "could not decrypt the provided encrypted_content")
}
