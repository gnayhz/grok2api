package relational

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobExecutionMigrationAndFencing(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra, rb := NewMediaJobRepository(a), NewMediaJobRepository(b)
			key := clientKeyModel{Name: "video-execution", Prefix: "video-execution", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			var accountIDs []uint64
			for i := range 2 {
				credential, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: fmt.Sprintf("native-account-%d", i), SourceKey: fmt.Sprintf("native-account-%d", i), AuthType: account.AuthTypeSSO, EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive})
				if err != nil {
					t.Fatal(err)
				}
				accountIDs = append(accountIDs, credential.ID)
			}
			now := time.Now().UTC()
			queued := testMediaJob("video_execution_legacy_queue", accountIDs[0], key.ID, media.StatusQueued, now)
			active := testMediaJob("video_execution_legacy_active", 0, key.ID, media.StatusInProgress, now)
			active.UpstreamURL = "https://vidgen.x.ai/existing.mp4"
			for _, job := range []media.Job{queued, active} {
				if err := ra.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"execution_shape", "execution_revision", "execution_phase", "native_route", "native_endpoint", "native_job_id", "upload_asset_id"} {
				if err := a.db.Migrator().DropConstraint(&mediaJobModel{}, "chk_media_jobs_"+name); err != nil {
					t.Fatal(err)
				}
			}
			for _, column := range []string{"generated_at", "upload_asset_id", "native_job_id", "native_endpoint", "native_route", "execution_phase", "execution_revision"} {
				if err := a.db.Migrator().DropColumn(&mediaJobModel{}, column); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			preserved, err := rb.GetMediaJob(ctx, active.ID, key.ID)
			if err != nil || preserved.Execution != (media.VideoExecution{}) || preserved.UpstreamURL != active.UpstreamURL {
				t.Fatalf("migration fabricated or erased execution: %+v %v", preserved, err)
			}
			claim, ok, err := ra.TryClaimMediaJob(ctx, queued.ID, now, now.Add(time.Minute), "execution_first_claim")
			if err != nil || !ok || claim.ClaimedFromStatus != media.StatusQueued {
				t.Fatalf("queued claim %+v %v %v", claim, ok, err)
			}
			old, ok, err := rb.TryClaimMediaJob(ctx, active.ID, now, now.Add(time.Minute), "execution_legacy_claim")
			if err != nil || !ok || old.ClaimedFromStatus != media.StatusInProgress {
				t.Fatalf("legacy claim %+v %v %v", old, ok, err)
			}
			previous := claim.Execution
			claim.Execution = media.VideoExecution{Revision: 1, Phase: media.VideoExecutionReady}
			if err := ra.SaveMediaJobExecution(ctx, claim, previous); err != nil {
				t.Fatal(err)
			}
			previous = claim.Execution
			results := make(chan error, 8)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					next := claim
					next.Execution = media.VideoExecution{Revision: 2, Phase: media.VideoExecutionSubmitting, Route: "xai", Endpoint: fmt.Sprintf("https://native-%d.invalid/v1", i), UploadAssetID: "vid_execution_upload_001"}
					repo := ra
					if i%2 != 0 {
						repo = rb
					}
					results <- repo.SaveMediaJobExecution(ctx, next, previous)
				}(i)
			}
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				} else if !errors.Is(err, repository.ErrConflict) {
					t.Fatal(err)
				}
			}
			if accepted != 1 {
				t.Fatalf("multiple native submissions accepted: %d", accepted)
			}
			claim, err = ra.GetMediaJob(ctx, queued.ID, key.ID)
			if err != nil {
				t.Fatal(err)
			}
			protected, err := NewMediaAssetRepository(a).ListProtectedMediaAssetIDs(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := protected[claim.Execution.UploadAssetID]; !ok {
				t.Fatal("active native upload target lost cleanup protection")
			}
			previous = claim.Execution
			claim.Execution.Revision++
			claim.Execution.Phase = media.VideoExecutionSubmitted
			claim.Execution.NativeJobID = "native-identity-1"
			if err := ra.SaveMediaJobExecution(ctx, claim, previous); err != nil {
				t.Fatal(err)
			}
			stale := claim
			current, ok, err := rb.TryClaimMediaJob(ctx, claim.ID, now.Add(2*time.Minute), now.Add(3*time.Minute), "execution_second_claim")
			if err != nil || !ok || current.Execution.NativeJobID != "native-identity-1" {
				t.Fatalf("reclaim: %+v %v %v", current, ok, err)
			}
			generatedAt := now.Add(time.Minute)
			next := current
			next.Execution.Revision++
			next.Execution.Phase = media.VideoExecutionGenerated
			next.Execution.GeneratedAt = &generatedAt
			next.UpstreamURL = "https://vidgen.x.ai/completed.mp4"
			next.ContentType = "video/mp4"
			next.ResultAssetID = "vid_execution_complete_001"
			foreignNext := next
			foreignNext.AccountID = accountIDs[1]
			if err := ra.SaveMediaJobExecution(ctx, foreignNext, current.Execution); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("native checkpoint changed account: %v", err)
			}
			staleNext := next
			staleNext.ClaimToken = stale.ClaimToken
			if err := ra.SaveMediaJobExecution(ctx, staleNext, stale.Execution); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("stale owner committed result: %v", err)
			}
			if err := rb.SaveMediaJobExecution(ctx, next, current.Execution); err != nil {
				t.Fatal(err)
			}
			if err := ra.UpdateMediaJob(ctx, stale); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("stale progress replaced checkpoint: %v", err)
			}
			blankClaim := next
			blankClaim.ClaimToken = ""
			if err := ra.UpdateMediaJob(ctx, blankClaim); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("unclaimed update replaced checkpoint: %v", err)
			}
			changed := next
			changed.Execution.Phase = media.VideoExecutionReady
			changed.UpstreamURL, changed.ResultAssetID, changed.ContentType = "", "", ""
			changed.Progress = 70
			if err := ra.UpdateMediaJob(ctx, changed); err != nil {
				t.Fatal(err)
			}
			if err := NewMediaUploadTicketRepository(b).BindLegacyJobResultAsset(ctx, claim.ID, "vid_late_upload_00001"); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("legacy asset writer modified checkpoint job: %v", err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := rb.GetMediaJob(ctx, claim.ID, key.ID)
			if err != nil || stored.Execution.Phase != media.VideoExecutionGenerated || stored.UpstreamURL != next.UpstreamURL || stored.ResultAssetID != next.ResultAssetID || stored.Progress != 70 {
				t.Fatalf("result changed after ordinary writer or restart: %+v %v", stored, err)
			}
			forged := stored
			forged.Execution.Revision++
			forged.Execution.Endpoint = "https://another.invalid"
			if err := ra.SaveMediaJobExecution(ctx, forged, stored.Execution); !errors.Is(err, media.ErrInvalidVideoExecution) {
				t.Fatalf("changed native location: %v", err)
			}
			completed := stored
			completed.Status = media.StatusCompleted
			if err := ra.UpdateMediaJob(ctx, completed); err != nil {
				t.Fatal(err)
			}
			if err := rb.UpdateMediaJob(ctx, stored); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("late current-claim progress reopened terminal task: %v", err)
			}
			failed := queued
			failed.ID = "video_execution_native_failed"
			failed.Execution = media.VideoExecution{Revision: 1, Phase: media.VideoExecutionReady}
			if err := ra.CreateMediaJob(ctx, failed); err != nil {
				t.Fatal(err)
			}
			failed, ok, err = ra.TryClaimMediaJob(ctx, failed.ID, now, now.Add(time.Minute), "native_failed_job_claim")
			if err != nil || !ok {
				t.Fatalf("native failed claim: %v %v", ok, err)
			}
			for _, phase := range []media.VideoExecutionPhase{media.VideoExecutionSubmitting, media.VideoExecutionSubmitted, media.VideoExecutionFailed} {
				prior := failed.Execution
				failed.Execution.Revision++
				failed.Execution.Phase = phase
				failed.Execution.Route = "console"
				failed.Execution.Endpoint = "https://console.invalid/v1"
				if phase != media.VideoExecutionSubmitting {
					failed.Execution.NativeJobID = "native-failed-identity"
				}
				if phase == media.VideoExecutionFailed {
					failed.ErrorCode = "generation_failed"
					failed.ErrorMessage = "native generation failed"
				}
				if err := rb.SaveMediaJobExecution(ctx, failed, prior); err != nil {
					t.Fatal(err)
				}
			}
			reloaded, err := ra.GetMediaJob(ctx, failed.ID, key.ID)
			if err != nil || reloaded.Execution.Phase != media.VideoExecutionFailed || reloaded.ErrorMessage != failed.ErrorMessage {
				t.Fatalf("native failure lost: %+v %v", reloaded, err)
			}
			for i, execution := range []media.VideoExecution{
				{Revision: 1},
				{Phase: media.VideoExecutionReady},
				{Revision: 1, Phase: media.VideoExecutionSubmitted, Route: "web", Endpoint: "https://grok.invalid", NativeJobID: "not-queryable"},
				{Revision: 1, Phase: media.VideoExecutionGenerated, Route: "console", Endpoint: "https://console.invalid"},
				{Revision: 1, Phase: media.VideoExecutionSubmitting, Route: "console", Endpoint: "https://console.invalid", NativeJobID: "already-created"},
			} {
				invalid := queued
				invalid.ID = fmt.Sprintf("video_invalid_execution_%d", i)
				invalid.Execution = execution
				if err := ra.CreateMediaJob(ctx, invalid); err == nil {
					t.Fatalf("domain accepted invalid execution %+v", execution)
				}
				if err := a.db.Create(mediaJobFromDomain(invalid)).Error; err == nil {
					t.Fatalf("SQL accepted invalid execution %+v", execution)
				}
			}
		})
	}
}
