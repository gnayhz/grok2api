package mediajob

import (
	"sync"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// VoiceGeneration preserves already observed realtime turns if a later turn fails.
type VoiceGeneration struct {
	mu                                 sync.Mutex
	active, completed, partial, failed bool
	duration                           float64
	durationReported                   bool
}

type VoiceGenerationFacts struct {
	Outcome          string
	Completed        bool
	Duration         float64
	DurationReported bool
}

func (v *VoiceGeneration) Observe(fact provider.VoiceWebSocketObservation) {
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

func (v *VoiceGeneration) Snapshot() VoiceGenerationFacts {
	v.mu.Lock()
	defer v.mu.Unlock()
	facts := VoiceGenerationFacts{Outcome: "unconfirmed", Completed: v.completed, Duration: v.duration, DurationReported: v.durationReported}
	switch {
	case v.completed && (v.active || v.failed):
		facts.Outcome = "partial"
	case v.completed:
		facts.Outcome = "completed"
	case v.partial:
		facts.Outcome = "partial"
	case v.failed:
		facts.Outcome = "failed"
	}
	return facts
}
