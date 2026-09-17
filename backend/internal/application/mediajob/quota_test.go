package mediajob

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestQuotaFinalizationModesKeepsEffectiveFence(t *testing.T) {
	refresh, decrement, _ := QuotaFinalizationModes(account.QuotaModeWebImagePro, account.QuotaGroupWebImagine)
	if refresh != account.QuotaGroupWebImagine || decrement != account.QuotaModeWebImagePro {
		t.Fatalf("refresh=%q decrement=%q", refresh, decrement)
	}
	refresh, decrement, availability := QuotaFinalizationModes("weekly", account.QuotaGroupWebImagine)
	if refresh != "weekly" || decrement != "weekly" || availability != account.QuotaGroupWebImagine {
		t.Fatalf("weekly refresh=%q decrement=%q availability=%q", refresh, decrement, availability)
	}
}

func TestVideoGeneratedUsesExecutionPhase(t *testing.T) {
	if VideoGenerated(media.Job{Status: media.StatusCompleted}) != true {
		t.Fatal("legacy completed job must count as generated")
	}
	if VideoGenerated(media.Job{Execution: media.VideoExecution{Phase: media.VideoExecutionGenerated}}) != true {
		t.Fatal("generated phase must count")
	}
	if VideoGenerated(media.Job{Status: media.StatusQueued, Execution: media.VideoExecution{Phase: media.VideoExecutionReady}}) {
		t.Fatal("ready phase is not generated")
	}
}
