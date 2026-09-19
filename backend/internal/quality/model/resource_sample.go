package model

import "github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"

// ResourceSample retains response, completion and verified identity as independent facts.
type ResourceSample struct {
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
