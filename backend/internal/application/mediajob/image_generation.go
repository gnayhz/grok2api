package mediajob

import (
	"sync"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// ImageGeneration records whether an image request actually produced outputs.
type ImageGeneration struct {
	mu   sync.Mutex
	fact provider.ImageGenerationObservation
}

func (g *ImageGeneration) Observe(fact provider.ImageGenerationObservation) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fact.Started = g.fact.Started || fact.Started
	g.fact.Completed = g.fact.Completed || fact.Completed
	g.fact.Failed = g.fact.Failed || fact.Failed
	g.fact.OutputImages = max(g.fact.OutputImages, fact.OutputImages)
	g.fact.QuotaUnits = max(g.fact.QuotaUnits, fact.QuotaUnits)
	if fact.UpstreamStatus != 0 {
		g.fact.UpstreamStatus = fact.UpstreamStatus
	}
}

func (g *ImageGeneration) Snapshot() (provider.ImageGenerationObservation, string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	state := "not_started"
	switch {
	case g.fact.Completed:
		state = "completed"
	case g.fact.OutputImages > 0:
		state = "partial"
	case g.fact.Failed:
		state = "failed"
	case g.fact.Started:
		state = "unconfirmed"
	}
	return g.fact, state
}
