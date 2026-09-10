package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobTerminalRetryPreservesCompletionHandoff(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key, err := NewClientKeyRepository(a).Create(ctx, clientkey.Key{Name: "terminal-retry", Prefix: "terminal-retry", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			job := media.Job{ID: "video_terminal_retry", RequestID: "request_terminal_retry", ClientKeyID: key.ID, ClientKeyName: key.Name, Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video", Prompt: "synthetic", Seconds: 3, Quality: "720p", Status: media.StatusInProgress, CreatedAt: now, UpdatedAt: now}
			ra, rb := NewMediaJobRepository(a), NewMediaJobRepository(b)
			if err := ra.CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			job.Status, job.CompletedAt = media.StatusCompleted, &now
			job.UsageRecordedAt = &now // An ordinary writer cannot forge a handoff.
			if err := ra.UpdateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			stored, err := rb.GetMediaJob(ctx, job.ID, key.ID)
			if err != nil || stored.UsageRecordedAt != nil {
				t.Fatalf("ordinary terminal writer forged usage handoff: %v %v", stored.UsageRecordedAt, err)
			}
			job.UsageRecordedAt = nil
			if err := rb.MarkMediaJobUsageRecorded(ctx, job.ID, now); err != nil {
				t.Fatal(err)
			}
			// The first terminal update committed but its acknowledgement was lost.
			// The worker retries while a peer has already accepted the usage fact.
			if err := ra.UpdateMediaJob(ctx, job); err != nil {
				t.Fatalf("terminal retry must remain idempotent: %v", err)
			}
			stored, err = rb.GetMediaJob(ctx, job.ID, key.ID)
			if err != nil || stored.UsageRecordedAt == nil || !stored.UsageRecordedAt.Equal(now) {
				t.Fatalf("terminal retry erased durable usage handoff: recorded=%v err=%v", stored.UsageRecordedAt, err)
			}
			// Once terminal, late same-status snapshots cannot change the output
			// being removed by a concurrent administrator either.
			job.ResultAssetID = "vid_foreign_late_result"
			job.ErrorMessage = "late failure"
			if err := ra.UpdateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			stored, err = rb.GetMediaJob(ctx, job.ID, key.ID)
			if err != nil || stored.ResultAssetID != "" || stored.ErrorMessage != "" {
				t.Fatalf("late retry changed terminal facts: %+v %v", stored, err)
			}
			job.Status = media.StatusInProgress
			if err := ra.UpdateMediaJob(ctx, job); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("late progress reopened terminal job: %v", err)
			}
		})
	}
}
