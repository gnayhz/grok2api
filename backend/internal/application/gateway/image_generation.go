package gateway

import (
	"net/http"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

type imageGeneration struct {
	mu   sync.Mutex
	fact provider.ImageGenerationObservation
}

func (g *imageGeneration) observe(fact provider.ImageGenerationObservation) {
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

func (g *imageGeneration) snapshot() (provider.ImageGenerationObservation, string) {
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

func imageCompatibilityFailure(err error) *provider.Response {
	code := "image_generation_incomplete"
	if provider.IsMediaPostProcessingError(err) {
		code = "media_postprocessing_failed"
	}
	return jsonMediaResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
		"code": code, "type": "server_error", "message": "上游已生成图片，但未能完成图片响应处理",
	}})
}
