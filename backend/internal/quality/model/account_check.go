package model

import (
	"errors"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

const AccountCheckVersion = "account-quality-check-v1"
const ProbeAccountCheck ProbeDirection = "account_check"
const AccountCheckSampleCount = 3
const AccountCheckQueueLimit = 32

var ErrCheckQueueFull = errors.New("quality check queue full")

// Account checks retain observations independently of court verdicts. A passing
// check is evidence about this model and time, not permission to clear a hold.
type AccountCheckSample struct {
	IdentityVerified     bool                 `json:"identity_verified"`
	CredentialGeneration uint64               `json:"credential_generation"`
	PlainOutput          bool                 `json:"plain_output"`
	UnexpectedOutput     bool                 `json:"unexpected_output"`
	Conflict             bool                 `json:"conflict"`
	Generated            bool                 `json:"generated"`
	PathChecks           int                  `json:"path_checks"`
	PathKey              string               `json:"path_key,omitempty"`
	PathFamily           int                  `json:"path_family,omitempty"`
	PathBinding          uint64               `json:"path_binding,omitempty"`
	PathVerified         bool                 `json:"path_verified"`
	Sample               string               `json:"sample"`
	Attempt              attemptmeta.Identity `json:"attempt"`
	Outcome              MeasurementOutcome   `json:"outcome"`
	Failure              ProbeFailure         `json:"failure,omitempty"`
	Rule                 string               `json:"rule,omitempty"`
	Thinking             bool                 `json:"thinking"`
	Completed            bool                 `json:"completed"`
	UsageReported        bool                 `json:"usage_reported"`
	InputTokens          int64                `json:"input_tokens"`
	CachedTokens         *int64               `json:"cached_tokens,omitempty"`
	ReasoningTokens      int64                `json:"reasoning_tokens"`
}

type AccountCheckReport struct {
	Version     string                   `json:"version"`
	Outcome     string                   `json:"outcome"`
	Reason      string                   `json:"reason"`
	Samples     []AccountCheckSample     `json:"samples"`
	Fingerprint *AccountCheckFingerprint `json:"fingerprint,omitempty"`
}

// Input deltas remove constant prompt overhead. They are a diagnostic vector,
// not a model identity or an uncalibrated degradation threshold.
type AccountCheckFingerprint struct {
	Stable      bool  `json:"stable"`
	RepeatDelta int64 `json:"repeat_delta"`
}

type AccountCheck struct {
	ID         uint64              `json:"id"`
	AccountID  uint64              `json:"account_id"`
	Model      string              `json:"model"`
	State      ProbeTaskState      `json:"state"`
	CreatedAt  time.Time           `json:"created_at"`
	FinishedAt *time.Time          `json:"finished_at,omitempty"`
	Report     *AccountCheckReport `json:"report,omitempty"`
}

// AssessAccountCheck requires all three independent sessions to agree. Errors,
// partial runs and mixed outcomes remain inconclusive; usage is diagnostic and
// never becomes an uncalibrated token threshold or a proxy for visible thinking.
func AssessAccountCheck(samples []AccountCheckSample) AccountCheckReport {
	r := AccountCheckReport{Version: AccountCheckVersion, Outcome: "inconclusive", Reason: "insufficient_samples", Samples: samples}
	if len(samples) != AccountCheckSampleCount {
		return r
	}
	if samples[0].Sample == "brief-confirmation" && samples[1].Sample == "repeated-as" && samples[2].Sample == "brief-confirmation" &&
		samples[0].Completed && samples[1].Completed && samples[2].Completed && samples[0].UsageReported && samples[1].UsageReported && samples[2].UsageReported {
		r.Fingerprint = &AccountCheckFingerprint{Stable: samples[0].InputTokens == samples[2].InputTokens, RepeatDelta: samples[1].InputTokens - samples[0].InputTokens}
	}
	clean, degraded := 0, 0
	for _, s := range samples {
		if s.Outcome == MeasurementClean {
			clean++
		}
		if s.Outcome == MeasurementDegraded {
			degraded++
		}
	}
	switch {
	case clean == AccountCheckSampleCount:
		r.Outcome, r.Reason = "clean", "thinking_present_in_all_samples"
	case degraded == AccountCheckSampleCount:
		r.Outcome, r.Reason = "degraded", "thinking_missing_in_all_samples"
	case clean > 0 && degraded > 0:
		r.Reason = "conflicting_samples"
	default:
		r.Reason = "measurement_unavailable"
	}
	return r
}
