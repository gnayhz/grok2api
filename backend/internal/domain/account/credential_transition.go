package account

import (
	"errors"
	"math"
	"strings"
	"time"
)

// CredentialRef identifies the exact material used for an upstream operation.
// Generation zero is the valid legacy generation, never a wildcard.
type CredentialRef struct {
	AccountID  uint64
	Provider   Provider
	Generation uint64
}

func (c Credential) CredentialRef() CredentialRef {
	return CredentialRef{AccountID: c.ID, Provider: c.Provider, Generation: c.CredentialGeneration}
}

type CredentialEventKind string

const (
	CredentialRefreshed             CredentialEventKind = "refreshed"
	CredentialRefreshFailed         CredentialEventKind = "refresh_failed"
	CredentialRejected              CredentialEventKind = "rejected"
	CredentialConfigurationRetry                        = 30 * time.Minute
	CredentialUnclassifiedAuthLimit                     = 5
)

// CredentialEvent contains a bounded outcome, not absolute counters from a
// request snapshot. Failure response text must already be redacted by Provider.
type CredentialEvent struct {
	Kind                      CredentialEventKind
	OccurredAt                time.Time
	AccessToken, RefreshToken string
	ExpiresAt                 time.Time
	BuildBotFlagSource        int
	Reason                    string
	Failure                   CredentialRefreshFailure
}

type CredentialRefreshFailure struct {
	Status                       int
	Code, Message, Response      string
	Permanent, PreservePermanent bool
	RetryAfter                   time.Duration
}

type CredentialResult struct {
	Credential Credential
	Applied    bool
}

var ErrCredentialGenerationExhausted = errors.New("credential generation exhausted")

// TransitionCredential is M07's single authentication/refresh policy. SQL
// evaluates it against locked current material. Other account dimensions stay
// intact, and stale material cannot activate, reject or penalize a replacement.
func TransitionCredential(current Credential, ref CredentialRef, event CredentialEvent, now time.Time) (CredentialResult, error) {
	result := CredentialResult{Credential: current}
	if current.ID != ref.AccountID || current.Provider != ref.Provider || current.CredentialGeneration != ref.Generation {
		return result, nil
	}
	next := current
	now = now.UTC()
	reject := func(reason string) {
		next.AuthStatus = AuthStatusReauthRequired
		next.AuthError = credentialText(reason, 512)
		if current.AuthStatus != AuthStatusReauthRequired || current.ReauthMarkedAt == nil {
			next.ReauthMarkedAt = &now
		}
	}
	switch event.Kind {
	case CredentialRefreshed:
		if current.AuthType != AuthTypeOAuth {
			return result, errors.New("only OAuth credentials may rotate")
		}
		if current.CredentialGeneration >= math.MaxInt64 {
			return result, ErrCredentialGenerationExhausted
		}
		next.CredentialGeneration++
		next.EncryptedAccessToken = event.AccessToken
		if event.RefreshToken != "" {
			next.EncryptedRefreshToken = event.RefreshToken
		}
		next.ExpiresAt = event.ExpiresAt
		due := CredentialRefreshDueAt(current.ID, event.ExpiresAt)
		next.RefreshDueAt, next.LastRefreshAt = &due, &now
		next.RefreshFailureCount, next.RefreshUnclassifiedAuthCount, next.LastRefreshErrorStatus = 0, 0, 0
		next.LastRefreshErrorCode, next.LastRefreshErrorMessage, next.LastRefreshErrorResponse = "", "", ""
		next.RefreshPermanent = false
		next.AuthStatus, next.AuthError, next.ReauthMarkedAt = AuthStatusActive, "", nil
		next.BuildBotFlagSource = 0
		if current.Provider == ProviderBuild && (event.BuildBotFlagSource == 1 || event.BuildBotFlagSource == 2) {
			next.BuildBotFlagSource = event.BuildBotFlagSource
		}
	case CredentialRefreshFailed:
		if current.AuthType != AuthTypeOAuth {
			return result, errors.New("only OAuth credentials may record refresh failure")
		}
		failure := event.Failure
		if current.RefreshFailureCount == math.MaxInt {
			return result, errors.New("credential refresh failure count exhausted")
		}
		next.RefreshFailureCount = current.RefreshFailureCount + 1
		next.LastRefreshErrorStatus = max(0, failure.Status)
		next.LastRefreshErrorCode = credentialText(failure.Code, 100)
		next.LastRefreshErrorMessage = credentialText(failure.Message, 512)
		next.LastRefreshErrorResponse = credentialText(failure.Response, 4096)
		permanent := failure.Permanent && IsPermanentCredentialRefreshErrorCode(failure.Code)
		if failure.PreservePermanent && current.RefreshPermanent && IsPermanentCredentialRefreshErrorCode(current.LastRefreshErrorCode) && IsPermanentCredentialRefreshErrorCode(failure.Code) {
			permanent = true
		}
		next.RefreshPermanent = permanent
		unclassified := IsUnclassifiedCredentialAuthRejection(failure.Status, failure.Code)
		next.RefreshUnclassifiedAuthCount = 0
		if unclassified {
			next.RefreshUnclassifiedAuthCount = 1
			if current.LastRefreshErrorStatus == failure.Status && strings.EqualFold(strings.TrimSpace(current.LastRefreshErrorCode), strings.TrimSpace(failure.Code)) {
				if current.RefreshUnclassifiedAuthCount == math.MaxInt {
					return result, errors.New("credential unclassified failure count exhausted")
				}
				next.RefreshUnclassifiedAuthCount = current.RefreshUnclassifiedAuthCount + 1
			}
		}
		retryAt := now.Add(CredentialRefreshBackoff(current.ID, next.RefreshFailureCount, failure.RetryAfter))
		if IsCredentialRefreshConfigurationErrorCode(failure.Code) && retryAt.Before(now.Add(CredentialConfigurationRetry)) {
			retryAt = now.Add(CredentialConfigurationRetry)
		}
		alive := current.EncryptedAccessToken != "" && !current.ExpiresAt.IsZero() && current.ExpiresAt.After(now)
		if permanent && alive {
			retryAt = current.ExpiresAt
		} else if permanent {
			retryAt = now
			reject("OAuth refresh failed: " + failure.Code)
		} else if unclassified && !alive && next.RefreshUnclassifiedAuthCount >= CredentialUnclassifiedAuthLimit {
			reject("OAuth refresh repeatedly rejected without a classifiable error")
		}
		next.RefreshDueAt = &retryAt
	case CredentialRejected:
		reject(event.Reason)
	default:
		return result, errors.New("invalid credential event")
	}
	return CredentialResult{Credential: next, Applied: true}, nil
}

func credentialText(v string, limit int) string {
	runes := []rune(v)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return v
}

func CredentialRefreshBackoff(accountID uint64, failureCount int, retryAfter time.Duration) time.Duration {
	delays := [...]time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 15 * time.Minute}
	index := max(0, min(failureCount-1, len(delays)-1))
	delay := delays[index]
	if retryAfter > delay {
		delay = min(retryAfter, 30*time.Minute)
	}
	return delay + time.Duration((accountID*37)%16)*time.Second
}

// IsPermanentCredentialRefreshErrorCode reports credential-specific terminal
// failures. HTTP status alone is intentionally insufficient: OAuth gateways
// also use 400/401 for temporary policy, client, and infrastructure errors.
// IsRecoverableCredentialRefreshErrorCode reports local or temporary refresh
// failures that may be stored as RefreshPermanent but must not block a later
// successful refresh from clearing that mark.
func IsRecoverableCredentialRefreshErrorCode(code string) bool {
	return !IsPermanentCredentialRefreshErrorCode(code)
}

func IsPermanentCredentialRefreshErrorCode(code string) bool {
	switch normalizeCredentialRefreshErrorCode(code) {
	case "invalid_grant",
		"invalid_refresh_token",
		"refresh_token_invalid",
		"refresh_token_expired",
		"refresh_token_revoked",
		"refresh_token_reused",
		"refresh_token_reuse",
		"token_reused",
		"token_reuse_detected",
		"expired_token",
		"revoked_token",
		"token_revoked",
		"missing_refresh_token":
		return true
	default:
		return false
	}
}

// IsCredentialRefreshConfigurationErrorCode reports OAuth failures caused by
// this gateway's client/request configuration rather than by one account's
// refresh token. These errors should be retried conservatively and surfaced to
// operators, but must not mark an individual account reauthRequired.
func IsCredentialRefreshConfigurationErrorCode(code string) bool {
	switch normalizeCredentialRefreshErrorCode(code) {
	case "invalid_client", "unauthorized_client", "invalid_request", "invalid_scope", "unsupported_grant_type":
		return true
	default:
		return false
	}
}

// IsUnclassifiedCredentialAuthRejection reports a 400/401 response that is
// neither a known terminal refresh-token error, a known client configuration
// error, nor an explicitly retryable OAuth condition. Repeated occurrences can
// eventually require operator reauthorization without claiming the refresh
// token was definitively revoked.
func IsUnclassifiedCredentialAuthRejection(status int, code string) bool {
	if status != 400 && status != 401 {
		return false
	}
	if IsPermanentCredentialRefreshErrorCode(code) || IsCredentialRefreshConfigurationErrorCode(code) {
		return false
	}
	switch normalizeCredentialRefreshErrorCode(code) {
	case "authorization_pending", "slow_down", "temporarily_unavailable", "server_error",
		"rate_limited", "rate_limit_exceeded", "too_many_requests", "oauth_timeout",
		"oauth_transport_error", "oauth_unavailable":
		return false
	default:
		return true
	}
}

func normalizeCredentialRefreshErrorCode(code string) string {
	normalized := strings.ToLower(strings.TrimSpace(code))
	return strings.ReplaceAll(normalized, "-", "_")
}
