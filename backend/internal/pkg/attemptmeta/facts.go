package attemptmeta

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

// PhysicalFact records one physical exchange independently of admission and
// final client delivery. It contains no response text, headers or credentials.
type PhysicalFact struct {
	Attempt           Identity            `json:"attempt"`
	Plane             string              `json:"plane"`
	Stage             string              `json:"stage"`
	Status            int                 `json:"status"`
	HeaderOutcome     string              `json:"header_outcome"`
	BodyOutcome       string              `json:"body_outcome"`
	BodyBytes         int64               `json:"body_bytes"`
	Usage             jsonpeek.TokenUsage `json:"usage"`
	GenerationOutcome string              `json:"generation_outcome,omitempty"`
	At                time.Time           `json:"at"`
	DurationMS        int64               `json:"duration_ms"`
}
