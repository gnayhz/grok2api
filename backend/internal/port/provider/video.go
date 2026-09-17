package provider

import (
	"errors"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// VideoOperation selects the official xAI video endpoint family.
type VideoOperation = media.VideoOperation

const (
	VideoOperationGenerate = media.VideoOperationGenerate
	VideoOperationEdit     = media.VideoOperationEdit
	VideoOperationExtend   = media.VideoOperationExtend
)

// ConsoleVideoMaxReferenceImages and ConsoleVideoMaxReferenceDurationSeconds
// describe the Console reference-to-video contract enforced by the upstream.
// They are shared by admission control and the Console adapter so invalid
// asynchronous jobs are rejected before enqueueing without weakening the
// adapter's final request-boundary validation.
const (
	ConsoleVideoMaxReferenceImages          = 7
	ConsoleVideoMaxReferenceDurationSeconds = 10
)

type VideoRequest struct {
	// Resume contains the acknowledged native checkpoint; Providers must not create again.
	Resume     *VideoCheckpoint
	Checkpoint func(VideoCheckpoint) error
	Credential account.Credential
	// Billing is used only to determine XAI eligibility in Build auto mode; nil means the account tier is unknown.
	Billing *account.Billing
	// JobID binds the local video job to XAI ZDR upload tickets and result assets.
	JobID string
	// Model is the selected upstream video model when the Provider supports more than one.
	Model string
	// Operation defaults to generate when empty.
	Operation   VideoOperation
	Prompt      string
	Duration    int
	AspectRatio string
	Resolution  string
	// ImageURL is the optional first-frame image (official "image" field).
	ImageURL string
	// ReferenceURLs are style/content references (official "reference_images").
	// A single reference must stay in reference_images and must not be coerced to image.
	// Official docs forbid combining image with reference_images.
	ReferenceURLs []string
	// ReferenceAudios are preset voice_ids for reference-to-video (official "reference_audios").
	// At most 3 entries; may be used alone or with reference_images.
	ReferenceAudios []string
	// VideoURL is required for edit/extend (official "video" field).
	VideoURL string
	Progress func(int)
}

type VideoResult struct {
	URL         string
	ContentType string
	// A non-empty AssetID means the result is stored as a local media asset; content reads must use MediaObjectStorage.
	AssetID string
}

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
