package media

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

type VideoExecutionPhase string

const (
	VideoExecutionReady       VideoExecutionPhase = "ready"
	VideoExecutionSubmitting  VideoExecutionPhase = "submitting"
	VideoExecutionSubmitted   VideoExecutionPhase = "submitted"
	VideoExecutionGenerated   VideoExecutionPhase = "generated"
	VideoExecutionFailed      VideoExecutionPhase = "failed"
	VideoExecutionUnconfirmed VideoExecutionPhase = "unconfirmed"
)

// VideoExecution is the durable upstream checkpoint. Public job completion and
// output archival have independent lifecycles. Empty phase means legacy data.
type VideoExecution struct {
	Revision      uint64
	Phase         VideoExecutionPhase
	Route         string
	Endpoint      string
	NativeJobID   string
	UploadAssetID string
	GeneratedAt   *time.Time
}

var ErrInvalidVideoExecution = errors.New("视频执行检查点无效")

func (e VideoExecution) Validate() error {
	if e.Revision == 0 || e.Revision > 1<<62 || len(e.NativeJobID) > 255 || strings.TrimSpace(e.NativeJobID) != e.NativeJobID || len(e.Endpoint) > 2048 || len(e.UploadAssetID) > 64 {
		return ErrInvalidVideoExecution
	}
	switch e.Phase {
	case VideoExecutionReady, VideoExecutionUnconfirmed:
		if e.Route != "" || e.Endpoint != "" || e.NativeJobID != "" || e.UploadAssetID != "" || e.GeneratedAt != nil {
			return ErrInvalidVideoExecution
		}
		return nil
	case VideoExecutionSubmitting, VideoExecutionSubmitted, VideoExecutionGenerated, VideoExecutionFailed:
	default:
		return ErrInvalidVideoExecution
	}
	if e.Route != "build" && e.Route != "xai" && e.Route != "console" && e.Route != "web" {
		return ErrInvalidVideoExecution
	}
	parsed, err := url.Parse(e.Endpoint)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ErrInvalidVideoExecution
	}
	if e.Phase == VideoExecutionSubmitting && e.NativeJobID != "" {
		return ErrInvalidVideoExecution
	}
	if e.Phase == VideoExecutionSubmitted && (e.NativeJobID == "" || e.Route == "web") {
		return ErrInvalidVideoExecution
	}
	if (e.Phase == VideoExecutionGenerated || e.Phase == VideoExecutionFailed) && e.Route != "web" && e.NativeJobID == "" {
		return ErrInvalidVideoExecution
	}
	if (e.Phase == VideoExecutionGenerated) != (e.GeneratedAt != nil) {
		return ErrInvalidVideoExecution
	}
	if e.UploadAssetID != "" && (e.Route != "xai" || len(e.UploadAssetID) < 16) {
		return ErrInvalidVideoExecution
	}
	return nil
}

// ValidateVideoExecutionTransition permits only an acknowledged explicit
// rejection to return submitting to ready. The caller owns that evidence.
func ValidateVideoExecutionTransition(previous, next VideoExecution) error {
	if previous != (VideoExecution{}) {
		if err := previous.Validate(); err != nil {
			return err
		}
	}
	if next.Revision != previous.Revision+1 {
		return ErrInvalidVideoExecution
	}
	if err := next.Validate(); err != nil {
		return err
	}
	switch previous.Phase {
	case "":
		if next.Phase == VideoExecutionReady || next.Phase == VideoExecutionUnconfirmed {
			return nil
		}
	case VideoExecutionReady:
		if next.Phase == VideoExecutionSubmitting {
			return nil
		}
	case VideoExecutionSubmitting:
		if next.Phase == VideoExecutionReady {
			return nil
		}
		if next.Phase != VideoExecutionSubmitted && next.Phase != VideoExecutionGenerated && next.Phase != VideoExecutionFailed {
			break
		}
		if (next.Phase == VideoExecutionGenerated || next.Phase == VideoExecutionFailed) && previous.Route != "web" {
			break
		}
		if sameVideoSubmission(previous, next) {
			return nil
		}
	case VideoExecutionSubmitted:
		if (next.Phase == VideoExecutionGenerated || next.Phase == VideoExecutionFailed) && sameVideoSubmission(previous, next) && previous.NativeJobID == next.NativeJobID {
			return nil
		}
	case VideoExecutionGenerated:
		if next.Phase == VideoExecutionGenerated && sameVideoSubmission(previous, next) && previous.NativeJobID == next.NativeJobID && previous.GeneratedAt.Equal(*next.GeneratedAt) {
			return nil
		}
	}
	return ErrInvalidVideoExecution
}
func sameVideoSubmission(a, b VideoExecution) bool {
	return a.Route == b.Route && a.Endpoint == b.Endpoint && a.UploadAssetID == b.UploadAssetID
}
