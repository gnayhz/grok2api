package provider

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
)

func TestReadDiagnosticBody(t *testing.T) {
	t.Parallel()
	data, truncated, err := ReadDiagnosticBody(nil)
	if err != nil || truncated || data != nil {
		t.Fatalf("nil body = %v truncated=%v err=%v", data, truncated, err)
	}
	body := strings.Repeat("x", MaxDiagnosticBodyBytes+4096)
	data, truncated, err = ReadDiagnosticBody(strings.NewReader(body))
	if err != nil || !truncated || len(data) != MaxDiagnosticBodyBytes {
		t.Fatalf("over-limit body len=%d truncated=%v err=%v", len(data), truncated, err)
	}
}

func TestObserveImageGenerationNilSafe(t *testing.T) {
	t.Parallel()
	ObserveImageGeneration(nil, ImageGenerationObservation{Completed: true})
	var seen ImageGenerationObservation
	ObserveImageGeneration(func(fact ImageGenerationObservation) { seen = fact }, ImageGenerationObservation{OutputImages: 2})
	if seen.OutputImages != 2 {
		t.Fatalf("observed = %#v", seen)
	}
}

func TestCheckpointVideo(t *testing.T) {
	t.Parallel()
	if err := CheckpointVideo(VideoRequest{}, VideoCheckpoint{Phase: media.VideoExecutionSubmitting}); err != nil {
		t.Fatalf("nil callback = %v", err)
	}
	cause := errors.New("disk")
	err := CheckpointVideo(VideoRequest{Checkpoint: func(VideoCheckpoint) error { return cause }}, VideoCheckpoint{})
	var typed *VideoCheckpointError
	if !errors.As(err, &typed) || !errors.Is(err, cause) {
		t.Fatalf("checkpoint error = %v", err)
	}
}

func TestApplyHistoryRecoveryWarnings(t *testing.T) {
	t.Parallel()
	header := http.Header{}
	ApplyHistoryRecoveryWarnings(header, historydomain.RecoveryOutcome{Failed: true, RemovedOpaque: 1})
	got := header.Get("X-Grok2API-Compatibility-Warnings")
	if !strings.Contains(got, "reasoning_recovery_failed") || !strings.Contains(got, "reasoning_encrypted_content_downgraded") {
		t.Fatalf("warnings = %q", got)
	}
}
