package media

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type videoJobReader interface {
	GetMediaJob(context.Context, string, uint64) (mediadomain.Job, error)
}
type videoAssetReader interface {
	OpenVideo(context.Context, string) (mediadomain.Asset, io.ReadCloser, error)
}

// VideoResources owns the tenant-scoped read and local availability projection.
// It never changes a durable job or fetches upstream content. Gateway owns that
// network fallback, only after OpenLocal confirms absence.
type VideoResources struct {
	jobs   videoJobReader
	assets videoAssetReader
}

func NewVideoResources(jobs videoJobReader, assets videoAssetReader) *VideoResources {
	return &VideoResources{jobs: jobs, assets: assets}
}
func videoResourceReadError(err error) error {
	return fmt.Errorf("%w: %w", mediadomain.ErrVideoResourceRead, err)
}

func (r *VideoResources) Lookup(ctx context.Context, id string, clientKeyID uint64) (mediadomain.Job, error) {
	if err := ctx.Err(); err != nil {
		return mediadomain.Job{}, videoResourceReadError(err)
	}
	id = strings.TrimSpace(id)
	if id == "" || clientKeyID == 0 {
		return mediadomain.Job{}, mediadomain.ErrVideoNotFound
	}
	if r == nil || r.jobs == nil {
		return mediadomain.Job{}, videoResourceReadError(ErrMediaJobsUnavailable)
	}
	job, err := r.jobs.GetMediaJob(ctx, id, clientKeyID)
	if errors.Is(err, repository.ErrNotFound) {
		return mediadomain.Job{}, mediadomain.ErrVideoNotFound
	}
	if err != nil {
		return mediadomain.Job{}, videoResourceReadError(err)
	}
	if job.ID != id || job.ClientKeyID != clientKeyID {
		return mediadomain.Job{}, mediadomain.ErrVideoNotFound
	}
	return job, nil
}

// Get hides only a confirmed absent asset from the response projection. The
// durable ID remains available to retries after storage or object repair.
func (r *VideoResources) Get(ctx context.Context, id string, clientKeyID uint64) (mediadomain.Job, error) {
	job, err := r.Lookup(ctx, id, clientKeyID)
	if err != nil {
		return mediadomain.Job{}, err
	}
	if job.Status != mediadomain.StatusCompleted || strings.TrimSpace(job.ResultAssetID) == "" {
		return job, nil
	}
	_, body, err := r.OpenLocal(ctx, job.ResultAssetID)
	if errors.Is(err, mediadomain.ErrVideoResourceRead) {
		return mediadomain.Job{}, err
	}
	if errors.Is(err, mediadomain.ErrAssetNotFound) {
		job.ResultAssetID = ""
		return job, nil
	}
	if err != nil {
		return mediadomain.Job{}, err
	}
	if err = body.Close(); err != nil {
		return mediadomain.Job{}, videoResourceReadError(err)
	}
	return job, nil
}

// OpenLocal transfers a non-nil body on success. Failed or canceled opens retain
// no body. An unconfigured optional archive preserves the existing remote path.
func (r *VideoResources) OpenLocal(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return mediadomain.Asset{}, nil, videoResourceReadError(err)
	}
	if r == nil || r.assets == nil {
		return mediadomain.Asset{}, nil, mediadomain.ErrAssetNotFound
	}
	asset, body, err := r.assets.OpenVideo(ctx, id)
	if err != nil {
		if body != nil {
			closeErr := body.Close()
			// A body alongside an error is an invalid handoff, even if its error
			// mentions missing data. Close it and forbid an additional download.
			return mediadomain.Asset{}, nil, videoResourceReadError(errors.Join(errors.New("video body returned with error"), err, closeErr))
		}
		if errors.Is(err, mediadomain.ErrVideoResourceRead) {
			return mediadomain.Asset{}, nil, err
		}
		if errors.Is(err, mediadomain.ErrAssetNotFound) {
			return mediadomain.Asset{}, nil, mediadomain.ErrAssetNotFound
		}
		return mediadomain.Asset{}, nil, videoResourceReadError(err)
	}
	if body == nil {
		return mediadomain.Asset{}, nil, videoResourceReadError(errors.New("video object reader missing"))
	}
	if err = ctx.Err(); err != nil {
		return mediadomain.Asset{}, nil, videoResourceReadError(errors.Join(err, body.Close()))
	}
	return asset, body, nil
}
