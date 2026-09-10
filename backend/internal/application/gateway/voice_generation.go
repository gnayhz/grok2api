package gateway

import (
	"sync"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// A realtime session may complete one turn and start another. Preserve already
// observed generation even if a later turn or the downstream connection fails.
type voiceGeneration struct {
	mu                                 sync.Mutex
	active, completed, partial, failed bool
	duration                           float64
	durationReported                   bool
}

type voiceGenerationFacts struct {
	outcome          string
	completed        bool
	duration         float64
	durationReported bool
}

func (v *voiceGeneration) observe(fact provider.VoiceWebSocketObservation) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if fact.Started {
		v.active, v.failed = true, false
	}
	if fact.Completed {
		v.completed, v.active, v.failed = true, false, false
	}
	if fact.Partial {
		v.partial, v.active = true, true
	}
	if fact.Failed {
		v.failed, v.active = true, false
	}
	if fact.AudioDurationReported {
		v.durationReported = true
		v.duration = max(v.duration, fact.AudioDurationSeconds)
	}
}

func (v *voiceGeneration) snapshot() voiceGenerationFacts {
	v.mu.Lock()
	defer v.mu.Unlock()
	facts := voiceGenerationFacts{outcome: "unconfirmed", completed: v.completed, duration: v.duration, durationReported: v.durationReported}
	switch {
	case v.completed && (v.active || v.failed):
		facts.outcome = "partial"
	case v.completed:
		facts.outcome = "completed"
	case v.partial:
		facts.outcome = "partial"
	case v.failed:
		facts.outcome = "failed"
	}
	return facts
}
