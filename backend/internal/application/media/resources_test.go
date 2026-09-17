package media

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type resourceJobRead func(context.Context, string, uint64) (mediadomain.Job, error)

func (f resourceJobRead) GetMediaJob(ctx context.Context, id string, key uint64) (mediadomain.Job, error) {
	return f(ctx, id, key)
}

type resourceVideoOpen func(context.Context, string) (mediadomain.Asset, io.ReadCloser, error)

func (f resourceVideoOpen) OpenVideo(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error) {
	return f(ctx, id)
}

type resourceBody struct {
	closes   atomic.Int32
	closeErr error
}

func (b *resourceBody) Read([]byte) (int, error) { return 0, io.EOF }
func (b *resourceBody) Close() error             { b.closes.Add(1); return b.closeErr }

func TestVideoResourcesOnlyProjectConfirmedAbsence(t *testing.T) {
	cause := errors.New("storage fault")
	for _, scenario := range []string{"local", "missing", "unconfigured", "failure", "nil-body", "body-and-error", "body-and-missing", "close-failure", "cancel-during-open", "queued"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := mediadomain.Job{ID: "video", ClientKeyID: 7, Status: mediadomain.StatusCompleted, ResultAssetID: "asset"}
			if scenario == "queued" {
				job.Status = mediadomain.StatusQueued
			}
			calls := 0
			body := &resourceBody{}
			if scenario == "close-failure" {
				body.closeErr = cause
			}
			var opener videoAssetReader = resourceVideoOpen(func(context.Context, string) (mediadomain.Asset, io.ReadCloser, error) {
				calls++
				switch scenario {
				case "missing":
					return mediadomain.Asset{}, nil, mediadomain.ErrAssetNotFound
				case "failure":
					return mediadomain.Asset{}, nil, cause
				case "nil-body":
					return mediadomain.Asset{}, nil, nil
				case "body-and-error":
					return mediadomain.Asset{}, body, cause
				case "body-and-missing":
					return mediadomain.Asset{}, body, mediadomain.ErrAssetNotFound
				case "cancel-during-open":
					cancel()
				}
				return mediadomain.Asset{ID: "asset", Kind: "video"}, body, nil
			})
			if scenario == "unconfigured" {
				opener = nil
			}
			resources := NewVideoResources(resourceJobRead(func(context.Context, string, uint64) (mediadomain.Job, error) { return job, nil }), opener)
			got, err := resources.Get(ctx, "video", 7)
			switch scenario {
			case "local", "queued":
				if err != nil || got.ResultAssetID != "asset" {
					t.Fatalf("available projection: %+v %v", got, err)
				}
			case "missing", "unconfigured":
				if err != nil || got.ResultAssetID != "" {
					t.Fatalf("absent projection: %+v %v", got, err)
				}
			default:
				if !errors.Is(err, mediadomain.ErrVideoResourceRead) || got.ID != "" {
					t.Fatalf("failure produced usable projection: %+v %v", got, err)
				}
			}
			if scenario == "failure" || scenario == "body-and-error" || scenario == "close-failure" {
				if !errors.Is(err, cause) {
					t.Fatalf("lost cause: %v", err)
				}
			}
			if scenario == "cancel-during-open" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			wantClose := int32(0)
			switch scenario {
			case "local", "body-and-error", "body-and-missing", "close-failure", "cancel-during-open":
				wantClose = 1
			}
			if body.closes.Load() != wantClose {
				t.Fatalf("close count=%d want=%d", body.closes.Load(), wantClose)
			}
			if (scenario == "queued" || scenario == "unconfigured") && calls != 0 {
				t.Fatal("inactive local probe dispatched")
			}
			if job.ResultAssetID != "asset" {
				t.Fatal("projection changed durable input")
			}
		})
	}
}

func TestVideoResourceLookupScopeAndErrors(t *testing.T) {
	cause := errors.New("database fault")
	calls := 0
	reader := resourceJobRead(func(context.Context, string, uint64) (mediadomain.Job, error) {
		calls++
		return mediadomain.Job{ID: "video", ClientKeyID: 7}, nil
	})
	resources := NewVideoResources(reader, nil)
	for _, identity := range []struct {
		id  string
		key uint64
	}{{"", 7}, {"video", 0}, {"foreign", 7}, {"video", 8}} {
		if _, err := resources.Lookup(context.Background(), identity.id, identity.key); !errors.Is(err, mediadomain.ErrVideoNotFound) {
			t.Fatalf("scope leaked: %v", err)
		}
	}
	if calls != 2 {
		t.Fatalf("invalid identities touched store: %d", calls)
	}
	for _, failure := range []error{repository.ErrNotFound, cause, context.DeadlineExceeded, context.Canceled} {
		resources = NewVideoResources(resourceJobRead(func(context.Context, string, uint64) (mediadomain.Job, error) { return mediadomain.Job{}, failure }), nil)
		_, err := resources.Lookup(context.Background(), "video", 7)
		if failure == repository.ErrNotFound {
			if !errors.Is(err, mediadomain.ErrVideoNotFound) {
				t.Fatal(err)
			}
		} else if !errors.Is(err, mediadomain.ErrVideoResourceRead) || !errors.Is(err, failure) || errors.Is(err, mediadomain.ErrVideoNotFound) {
			t.Fatalf("misclassified failure: %v", err)
		}
	}
	var absent *VideoResources
	if _, err := absent.Lookup(context.Background(), "video", 7); !errors.Is(err, mediadomain.ErrVideoResourceRead) {
		t.Fatalf("missing wiring hid as absence: %v", err)
	}
}

type resourceObjectOpen struct {
	repository.MediaObjectStorage
	body io.ReadCloser
	err  error
}

func (s resourceObjectOpen) Open(context.Context, string) (io.ReadCloser, error) {
	return s.body, s.err
}

type resourceAssetRow struct {
	repository.MediaAssetRepository
}

func (resourceAssetRow) GetMediaAsset(context.Context, string) (mediadomain.Asset, error) {
	return mediadomain.Asset{ID: "asset", Kind: "video", StorageKey: "videos/example"}, nil
}

func TestVideoResourceObjectHandoffFailureIsClosed(t *testing.T) {
	for _, fault := range []error{errors.New("object failure"), mediadomain.ErrAssetNotFound} {
		t.Run(fault.Error(), func(t *testing.T) {
			body := &resourceBody{}
			service := NewServiceWithTickets(resourceAssetRow{}, nil, nil, resourceObjectOpen{body: body, err: fault}, nil, Config{})
			resources := NewVideoResources(resourceJobRead(func(context.Context, string, uint64) (mediadomain.Job, error) {
				return mediadomain.Job{ID: "video", ClientKeyID: 7, Status: mediadomain.StatusCompleted, ResultAssetID: "asset"}, nil
			}), service)
			if _, err := resources.Get(context.Background(), "video", 7); !errors.Is(err, mediadomain.ErrVideoResourceRead) || !errors.Is(err, fault) {
				t.Fatalf("object error changed to absence: %v", err)
			}
			if body.closes.Load() != 1 {
				t.Fatalf("object error body not closed once: %d", body.closes.Load())
			}
		})
	}
}
