package inference

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type videoInputBeforeCreate struct {
	repository.MediaJobRepository
	before func(context.Context, *media.Job) error
}

func (r *videoInputBeforeCreate) CreateMediaJob(ctx context.Context, job media.Job) error {
	if err := r.before(ctx, &job); err != nil {
		return err
	}
	return r.MediaJobRepository.CreateMediaJob(ctx, job)
}

func postVideoInput(t *testing.T, fx voiceCompletionFixture, input media.Asset) *httptest.ResponseRecorder {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"model": fx.publicModel, "prompt": "test input handoff", "duration": 5, "resolution": "720p", "image": map[string]string{"file_id": input.ID}})
	request := httptest.NewRequest(http.MethodPost, "/v1/videos/generations", bytes.NewReader(data))
	request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	fx.router.ServeHTTP(response, request)
	return response
}
func saveVideoTestInput(t *testing.T, fx voiceCompletionFixture) (media.Asset, []byte) {
	t.Helper()
	picture, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	input, err := fx.media.SaveInputImage(context.Background(), picture)
	if err != nil {
		t.Fatal(err)
	}
	return input, picture
}

func TestVideoJobAdmissionCannotAcceptAlreadyReleasedInput(t *testing.T) {
	for _, fault := range []string{"released_after_precheck", "sql_insert_failure"} {
		t.Run(fault, func(t *testing.T) {
			ctx := context.Background()
			fx := newMediaCompletionFixture(t, "https://unused.invalid", "grok-imagine-video", account.ProviderConsole, func(store provider.ImageAssetStore) provider.ImageAssetStore { return store })
			fx.service.ConfigureMediaAssets(fx.media)
			input, _ := saveVideoTestInput(t, fx)
			var attempted string
			jobs := &videoInputBeforeCreate{MediaJobRepository: fx.jobs, before: func(ctx context.Context, job *media.Job) error {
				attempted = job.ID
				key, err := fx.clients.Get(ctx, fx.created.Key.ID)
				if err != nil {
					return err
				}
				if key.ReservedUsageUSDTicks <= 0 {
					t.Error("test did not cross actual billing reservation")
				}
				if fault == "released_after_precheck" {
					return fx.media.ReleaseInputAssets(ctx, []string{media.InputReference(input.ID)})
				}
				// The real SQL CHECK fails after the input lock was acquired. Admission
				// must cancel its reservation even when persistence itself rejects creation.
				job.Seconds = -1
				return nil
			}}
			fx.service.ConfigureMedia(jobs, mediaapp.NewVideoResources(jobs, nil), 1)
			response := postVideoInput(t, fx, input)
			if attempted == "" {
				t.Fatalf("fixture did not cross precheck/reservation: %d %s", response.Code, response.Body.String())
			}
			want := http.StatusBadRequest
			if fault == "sql_insert_failure" {
				want = http.StatusBadGateway
			}
			if response.Code != want {
				t.Fatalf("rejected input: status=%d want=%d body=%s", response.Code, want, response.Body.String())
			}
			key, err := fx.clients.Get(ctx, fx.created.Key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if key.ReservedUsageUSDTicks != 0 || key.BilledUsageUSDTicks != 0 {
				t.Fatalf("rejected job retained billing: reserved=%d billed=%d", key.ReservedUsageUSDTicks, key.BilledUsageUSDTicks)
			}
			if _, err := fx.jobs.GetMediaJob(ctx, attempted, fx.created.Key.ID); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("rejected job persisted: %v", err)
			}
			// Failed SQL creation must release its lock so explicit input release can
			// immediately finish; the earlier-release case remains idempotent.
			if err := fx.media.ReleaseInputAssets(ctx, []string{media.InputReference(input.ID)}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVideoHTTPAcceptedInputReachesNativeWorkerAndIsReleased(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var calls, submitted, downloads atomic.Int32
	var wantInput string
	upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			var payload struct {
				Image struct {
					URL string `json:"url"`
				} `json:"image"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Error(err)
			}
			if payload.Image.URL != wantInput {
				t.Errorf("native request lost actual input bytes: length=%d want=%d", len(payload.Image.URL), len(wantInput))
			}
			submitted.Add(1)
			_, _ = w.Write([]byte(`{"request_id":"input-handoff-native"}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
		}
	})
	defer upstream.Close()
	fx := newMediaCompletionFixture(t, upstream.URL, "grok-imagine-video", account.ProviderConsole, func(store provider.ImageAssetStore) provider.ImageAssetStore { return store })
	fx.service.ConfigureMedia(fx.jobs, mediaapp.NewVideoResources(fx.jobs, nil), 1)
	fx.service.ConfigureMediaAssets(fx.media)
	fx.service.UpdateVideoMaxAttempts(1)
	input, picture := saveVideoTestInput(t, fx)
	wantInput = "data:image/png;base64," + base64.StdEncoding.EncodeToString(picture)
	payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
	fx.useProxy(imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, r *http.Request) {
		downloads.Add(1)
		w.Header().Set("Content-Type", "video/mp4")
		_, _ = w.Write(payload)
	}))
	response := postVideoInput(t, fx, input)
	var created struct {
		ID string `json:"request_id"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil || response.Code != http.StatusOK || created.ID == "" {
		t.Fatalf("create=%d %s err=%v", response.Code, response.Body.String(), err)
	}
	if err := fx.media.ReleaseInputAssets(ctx, []string{media.InputReference(input.ID)}); err != nil {
		t.Fatal(err)
	}
	_, body, err := fx.media.OpenInputAsset(ctx, input.ID)
	if err != nil {
		t.Fatalf("queued job lost input: %v", err)
	}
	_ = body.Close()
	done := make(chan struct{})
	go func() { fx.service.RunVideoWorkers(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	record := waitVoiceAudit(t, fx.audits)
	// Wait for completion after the ledger write: the worker still owns release.
	for {
		_, body, err := fx.media.OpenInputAsset(ctx, input.ID)
		if body != nil {
			_ = body.Close()
		}
		if errors.Is(err, mediaapp.ErrInputAssetNotFound) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal("completed worker retained input")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done
	assertVideoBilling(t, fx, record)
	current, err := fx.jobs.GetMediaJob(context.Background(), created.ID, fx.created.Key.ID)
	if err != nil || current.Status != media.StatusCompleted || current.ResultAssetID == "" || current.UsageRecordedAt == nil || submitted.Load() != 1 || downloads.Load() != 1 {
		t.Fatalf("worker input lifecycle: job=%+v submissions=%d downloads=%d err=%v", current, submitted.Load(), downloads.Load(), err)
	}
	if record.GenerationOutcome != "completed" || record.EstimatedCostInUSDTicks <= 0 {
		t.Fatalf("completion lost generation/cost: %+v", record)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/videos/"+created.ID+"/content", nil)
	request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
	output := httptest.NewRecorder()
	fx.router.ServeHTTP(output, request)
	if output.Code != http.StatusOK || !bytes.Equal(output.Body.Bytes(), payload) {
		t.Fatalf("completed output=%d bytes=%d", output.Code, output.Body.Len())
	}
}
