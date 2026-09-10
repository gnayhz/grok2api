package media

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// This fixture deliberately has no filesystem paths. Its staged object exposes
// only the same port used by LocalStore; metadata and tickets still use SQL.
type streamVideoObjects struct {
	repository.MediaObjectStorage
	content map[string][]byte
	stages  []*streamVideoStage
}

func (s *streamVideoObjects) BeginVideoUpload(_ context.Context, id, _ string) (repository.MediaVideoUpload, error) {
	stage := &streamVideoStage{store: s, key: "memory/" + id}
	s.stages = append(s.stages, stage)
	return stage, nil
}
func (s *streamVideoObjects) Open(_ context.Context, key string) (io.ReadCloser, error) {
	raw, ok := s.content[key]
	if !ok {
		return nil, ErrAssetNotFound
	}
	return io.NopCloser(bytes.NewReader(raw)), nil
}
func (s *streamVideoObjects) Delete(_ context.Context, key string) error {
	delete(s.content, key)
	return nil
}

type streamVideoStage struct {
	bytes.Buffer
	store   *streamVideoObjects
	key     string
	cleaned bool
}

func (s *streamVideoStage) Commit(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, exists := s.store.content[s.key]; exists {
		return "", errors.New("existing object")
	}
	s.store.content[s.key] = bytes.Clone(s.Bytes())
	return s.key, nil
}
func (s *streamVideoStage) Abort(context.Context) error {
	s.cleaned = true
	s.Reset()
	return nil
}

func TestAllVideoWritersConsumePathIndependentStreams(t *testing.T) {
	for _, consumer := range []string{"archive", "input", "ticket"} {
		t.Run(consumer, func(t *testing.T) {
			service, _, cleanup := newUploadTestService(t)
			defer cleanup()
			store := &streamVideoObjects{content: make(map[string][]byte)}
			service.objects = store
			ctx := context.Background()
			// One-byte reads split both the format signature and the 512-byte sniff
			// boundary while allowing the complete body to contribute to its hash.
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 777)...)
			body := iotest.OneByteReader(bytes.NewReader(payload))
			var asset mediadomain.Asset
			var err error
			switch consumer {
			case "archive":
				asset, err = service.SaveVideo(ctx, "", "video/mp4", body)
			case "input":
				asset, err = service.SaveInputVideo(ctx, "video/mp4", body)
			case "ticket":
				url, _, issueErr := service.IssueVideoUpload(ctx, "stream_job")
				if issueErr != nil {
					t.Fatal(issueErr)
				}
				asset, err = service.ReceiveVideoUpload(ctx, strings.TrimPrefix(url, "https://api.example/v1/media/uploads/"), "video/mp4", body)
			}
			if err != nil {
				t.Fatal(err)
			}
			digest := sha256.Sum256(payload)
			if asset.SHA256 != hex.EncodeToString(digest[:]) || asset.SizeBytes != int64(len(payload)) || asset.MIMEType != "video/mp4" {
				t.Fatalf("stream metadata differs: %+v", asset)
			}
			if (asset.ExpiresAt != nil) != (consumer == "input") {
				t.Fatalf("input lifetime changed: %+v", asset)
			}
			var reopened io.ReadCloser
			if consumer == "input" {
				_, reopened, err = service.OpenInputAsset(ctx, asset.ID)
			} else {
				_, reopened, err = service.OpenVideo(ctx, asset.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			raw, err := io.ReadAll(reopened)
			closeErr := reopened.Close()
			if err != nil || closeErr != nil || !bytes.Equal(raw, payload) {
				t.Fatalf("stream content changed: %v/%v", err, closeErr)
			}
			if len(store.stages) != 1 || !store.stages[0].cleaned || store.stages[0].Len() != 0 {
				t.Fatal("successful consumer retained staging")
			}
		})
	}
}
