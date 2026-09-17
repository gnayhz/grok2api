package gateway

import (
	"context"
	"net/http"

	"github.com/chenyme/grok2api/backend/internal/application/mediajob"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type imageGeneration struct{ mediajob.ImageGeneration }

func (g *imageGeneration) observe(fact provider.ImageGenerationObservation) {
	g.Observe(fact)
}
func (g *imageGeneration) snapshot() (provider.ImageGenerationObservation, string) {
	return g.Snapshot()
}

type voiceGeneration struct{ mediajob.VoiceGeneration }

func (v *voiceGeneration) observe(fact provider.VoiceWebSocketObservation) {
	v.Observe(fact)
}

// voiceGenerationFacts 直接复用 mediajob 导出的事实类型,避免逐字段镜像。
func (v *voiceGeneration) snapshot() mediajob.VoiceGenerationFacts {
	return v.Snapshot()
}

func quotaFinalizationModes(effectiveMode, refreshGroup string) (string, string, string) {
	return mediajob.QuotaFinalizationModes(effectiveMode, refreshGroup)
}

func videoGenerated(job media.Job) bool { return mediajob.VideoGenerated(job) }

func videoResumeCheckpoint(job media.Job) *provider.VideoCheckpoint {
	return mediajob.ResumeCheckpoint(job)
}

func (s *Service) mediaExecution() *mediajob.Execution {
	return mediajob.NewExecution(s.mediaJobs, s.finishVideoQuota, sanitizeDiagnosticText)
}

func (s *Service) initializeVideoExecution(ctx context.Context, job *media.Job) error {
	return s.mediaExecution().Initialize(ctx, job)
}

func (s *Service) checkpointVideo(ctx context.Context, job *media.Job, point provider.VideoCheckpoint) error {
	return s.mediaExecution().Checkpoint(ctx, job, point)
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
