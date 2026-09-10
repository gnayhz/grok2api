package provider

import (
	"errors"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// VideoCheckpoint reports protocol facts synchronously. A persistence failure
// stops the Provider before the next network or output-processing stage.
type VideoCheckpoint struct {
	Failure                                     string
	Phase                                       media.VideoExecutionPhase
	Route, Endpoint, NativeJobID, UploadAssetID string
	Result                                      VideoResult
}

type VideoCheckpointError struct{ Err error }

func (e *VideoCheckpointError) Error() string { return "保存视频执行检查点: " + e.Err.Error() }
func (e *VideoCheckpointError) Unwrap() error { return e.Err }
func CheckpointVideo(request VideoRequest, checkpoint VideoCheckpoint) error {
	if request.Checkpoint == nil {
		return nil
	}
	if err := request.Checkpoint(checkpoint); err != nil {
		return &VideoCheckpointError{Err: err}
	}
	return nil
}

// Explicit upstream rejections and a transport gate that refused submission
// prove that this create did not start a native job. In-flight local deadlines,
// transport failures and 5xx responses retain their submitting checkpoint.
func CheckpointVideoRejection(request VideoRequest, err error) error {
	if VideoCreateFailureStage(err) != VideoStageCreate && !neterror.RejectedBeforeSubmission(err) {
		return nil
	}
	return CheckpointVideo(request, VideoCheckpoint{Phase: media.VideoExecutionReady})
}

// VideoGenerationFailure is an explicit failed native job or stream result,
// distinct from inability to query, decode, or archive that result.
type VideoGenerationFailure struct{ Err error }

func (e *VideoGenerationFailure) Error() string { return e.Err.Error() }
func (e *VideoGenerationFailure) Unwrap() error { return e.Err }
func CheckpointVideoFailure(request VideoRequest, err error) error {
	var failed *VideoGenerationFailure
	if !errors.As(err, &failed) {
		return nil
	}
	return CheckpointVideo(request, VideoCheckpoint{Phase: media.VideoExecutionFailed, Failure: failed.Error()})
}
