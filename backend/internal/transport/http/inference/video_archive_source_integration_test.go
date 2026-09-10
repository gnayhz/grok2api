package inference

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type videoArchiveCleanupBoundary struct {
	repository.MediaAssetRepository
	jobs    repository.MediaJobRepository
	cleaner *mediaapp.Service
	keyID   uint64
	checked atomic.Int32
}

func (r *videoArchiveCleanupBoundary) CreateMediaAsset(ctx context.Context, asset media.Asset) error {
	if err := r.MediaAssetRepository.CreateMediaAsset(ctx, asset); err != nil {
		return err
	}
	current, err := r.jobs.GetMediaJob(ctx, asset.SourceJobID, r.keyID)
	if err != nil {
		return err
	}
	if current.Status != media.StatusInProgress || current.ResultAssetID != "" || current.Execution.Phase != media.VideoExecutionGenerated || current.ClaimToken == "" {
		return fmt.Errorf("archive bypassed result handoff: status=%s result=%s phase=%s claim=%t", current.Status, current.ResultAssetID, current.Execution.Phase, current.ClaimToken != "")
	}
	deleted, err := r.cleaner.Cleanup(ctx)
	if err != nil {
		return err
	}
	if deleted != 0 {
		return fmt.Errorf("peer cleanup deleted %d active archives", deleted)
	}
	r.checked.Add(1)
	return nil
}

func TestVideoHTTPArchiveSurvivesCleanupBeforeResultHandoff(t *testing.T) {
	if !imageAssetTLSChild(t) {
		return
	}
	for _, kind := range []account.Provider{account.ProviderBuild, account.ProviderConsole, account.ProviderWeb} {
		t.Run(string(kind), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var calls, downloads atomic.Int32
			upstream := voiceCompletionUpstream(t, &calls, func(w fhttp.ResponseWriter, r *fhttp.Request) {
				w.Header().Set("Content-Type", "application/json")
				if kind != account.ProviderWeb && r.Method == http.MethodPost {
					_, _ = w.Write([]byte(`{"request_id":"archive-source-native"}`))
				} else if kind == account.ProviderConsole {
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://vidgen.x.ai/video.mp4"}}`))
				} else if kind == account.ProviderWeb {
					_, _ = w.Write([]byte(`{"result":{"response":{"streamingVideoGenerationResponse":{"progress":100,"videoUrl":"https://assets.grok.com/video.mp4"}}}}`))
				} else {
					_, _ = w.Write([]byte(`{"status":"done","video":{"url":"https://assets.grok.com/video.mp4"}}`))
				}
			})
			defer upstream.Close()
			model := "grok-imagine-video"
			if kind == account.ProviderBuild {
				model = "grok-imagine-video-1.5"
			}
			fx := newMediaCompletionFixture(t, upstream.URL, model, kind, nil)
			if kind == account.ProviderBuild {
				enabled, route := true, account.BuildRouteBuild
				if _, err := fx.accounts.UpdateAdministration(ctx, fx.account.ID, repository.AccountAdminPatch{BuildSuperEntitled: &enabled, BuildRouteMode: &route}); err != nil {
					t.Fatal(err)
				}
			}
			db, err := relational.OpenSQLite(ctx, fx.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			peer, err := relational.OpenSQLite(ctx, fx.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer peer.Close()
			objects, err := localmedia.NewLocalStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			cfg := mediaapp.Config{MaxTotalBytes: 64, CleanupThresholdPercent: 50}
			cleaner := mediaapp.NewServiceWithTickets(relational.NewMediaAssetRepository(peer), relational.NewMediaJobRepository(peer), relational.NewMediaUploadTicketRepository(peer), objects, nil, cfg)
			assets := &videoArchiveCleanupBoundary{MediaAssetRepository: relational.NewMediaAssetRepository(db), jobs: relational.NewMediaJobRepository(peer), cleaner: cleaner, keyID: fx.created.Key.ID}
			local := mediaapp.NewServiceWithTickets(assets, relational.NewMediaJobRepository(db), relational.NewMediaUploadTicketRepository(db), objects, nil, cfg)
			payload := append([]byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}, bytes.Repeat([]byte{1}, 128)...)
			fx.useProxy(imageAssetProxy(t, upstream.URL, func(w http.ResponseWriter, r *http.Request) {
				downloads.Add(1)
				w.Header().Set("Content-Type", "video/mp4")
				_, _ = w.Write(payload)
			}))
			fx.service.ConfigureMedia(fx.jobs, 1)
			fx.service.ConfigureMediaAssets(local)
			fx.service.UpdateVideoMaxAttempts(1)
			job := createVideoAuthorizationJob(t, fx)
			workerCtx, stop := context.WithCancel(ctx)
			done := make(chan struct{})
			go func() { fx.service.RunVideoWorkers(workerCtx); close(done) }()
			defer func() { stop(); <-done }()
			record := waitVoiceAudit(t, fx.audits)
			stop()
			<-done
			assertVideoBilling(t, fx, record)
			current, err := fx.jobs.GetMediaJob(ctx, job.ID, job.ClientKeyID)
			if err != nil || current.Status != media.StatusCompleted || current.ResultAssetID == "" || assets.checked.Load() != 1 || downloads.Load() != 1 {
				t.Fatalf("real archive handoff failed: job=%+v checks=%d downloads=%d err=%v", current, assets.checked.Load(), downloads.Load(), err)
			}
			if record.GenerationOutcome != "completed" || record.EstimatedCostInUSDTicks <= 0 {
				t.Fatalf("generation/cost facts lost: %+v", record)
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/videos/"+job.ID+"/content", nil)
			request.Header.Set("Authorization", "Bearer "+fx.created.Secret)
			response := httptest.NewRecorder()
			fx.router.ServeHTTP(response, request)
			if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), payload) {
				t.Fatalf("archived HTTP content: status=%d bytes=%d", response.Code, response.Body.Len())
			}
			storedAsset, err := assets.GetMediaAsset(ctx, current.ResultAssetID)
			if err != nil || storedAsset.SourceJobID != job.ID {
				t.Fatalf("result lost source provenance: %+v %v", storedAsset, err)
			}
		})
	}
}
