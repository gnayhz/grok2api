package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaQuotaMigrationAndHandoffFencing(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ra, rb := NewMediaJobRepository(a), NewMediaJobRepository(b)
			ctx, now := context.Background(), time.Now().UTC().Truncate(time.Microsecond)
			key := clientKeyModel{Name: "quota-job", Prefix: "quota-job", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			credential, _, err := NewAccountRepository(a).UpsertByIdentity(ctx, account.Credential{Provider: account.ProviderWeb, Name: "quota-job", SourceKey: "quota-job", AuthType: account.AuthTypeSSO, EncryptedAccessToken: testEncryptedToken, Enabled: true, AuthStatus: account.AuthStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			legacy := testMediaJob("video_quota_legacy_completed", credential.ID, key.ID, media.StatusCompleted, now)
			queued := testMediaJob("video_quota_new_queued", credential.ID, key.ID, media.StatusQueued, now)
			for _, job := range []media.Job{legacy, queued} {
				if err := ra.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"quota_mode", "quota_version", "quota_account"} {
				if err := a.db.Migrator().DropConstraint(&mediaJobModel{}, "chk_media_jobs_"+name); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"quota_mode", "quota_snapshot_version", "quota_account_id", "quota_recorded_at"} {
				if err := a.db.Migrator().DropColumn(&mediaJobModel{}, name); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := rb.GetMediaJob(ctx, legacy.ID, key.ID)
			if err != nil || old.Quota != (media.JobQuota{}) || !old.PendingQuotaHandoff() {
				t.Fatalf("legacy handoff fabricated: %+v %v", old, err)
			}
			pending, err := rb.ListUnrecordedMediaJobQuotas(ctx, "", 1)
			if err != nil || len(pending) != 1 || pending[0].ID != legacy.ID {
				t.Fatalf("legacy recovery=%+v %v", pending, err)
			}
			if err := ra.MarkMediaJobQuotaRecorded(ctx, old, now); err != nil {
				t.Fatal(err)
			}
			claim, ok, err := ra.TryClaimMediaJob(ctx, queued.ID, now, now.Add(time.Minute), "quota_execution_first_claim")
			if err != nil || !ok {
				t.Fatalf("claim=%+v %t %v", claim, ok, err)
			}
			prior := claim.Execution
			claim.Execution = media.VideoExecution{Revision: 1, Phase: media.VideoExecutionReady}
			if err := ra.SaveMediaJobExecution(ctx, claim, prior); err != nil {
				t.Fatal(err)
			}
			if err := rb.MarkMediaJobQuotaRecorded(ctx, claim, now); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("non-generation acknowledged: %v", err)
			}
			prior = claim.Execution
			claim.Quota = media.JobQuota{AccountID: credential.ID, Mode: account.QuotaModeWebVideo720p, SnapshotVersion: 7}
			claim.Execution = media.VideoExecution{Revision: 2, Phase: media.VideoExecutionSubmitting, Route: "web", Endpoint: "https://native.invalid"}
			if err := ra.SaveMediaJobExecution(ctx, claim, prior); err != nil {
				t.Fatal(err)
			}
			forged := claim
			forged.Quota = media.JobQuota{AccountID: 999, Mode: "weekly", SnapshotVersion: 99, RecordedAt: &now}
			if err := rb.UpdateMediaJob(ctx, forged); err != nil {
				t.Fatal(err)
			}
			stored, err := ra.GetMediaJob(ctx, claim.ID, key.ID)
			if err != nil || stored.Quota != claim.Quota {
				t.Fatalf("ordinary update changed owner: %+v %v", stored, err)
			}
			prior = claim.Execution
			claim.Execution.Revision++
			claim.Execution.Phase = media.VideoExecutionGenerated
			claim.Execution.GeneratedAt = &now
			forged = claim
			forged.Quota.SnapshotVersion++
			if err := rb.SaveMediaJobExecution(ctx, forged, prior); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("in-flight generation switched window: %v", err)
			}
			if err := ra.SaveMediaJobExecution(ctx, claim, prior); err != nil {
				t.Fatal(err)
			}
			claim.Status, claim.LeaseUntil = media.StatusFailed, nil
			if err := ra.UpdateMediaJob(ctx, claim); err != nil {
				t.Fatal(err)
			}
			if err := rb.DeleteMediaJob(ctx, claim.ID); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("pending generation deleted: %v", err)
			}
			if _, err := NewClientKeyRepository(b).DeleteMany(ctx, []uint64{key.ID}); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("key deleted pending generation: %v", err)
			}
			forged = claim
			forged.Quota.Mode = "weekly"
			if err := rb.MarkMediaJobQuotaRecorded(ctx, forged, now); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("foreign receipt accepted: %v", err)
			}
			// The quota account identity remains after the display/account FK is
			// cleared by administrator deletion of a terminal account.
			if err := NewAccountRepository(a).Delete(ctx, credential.ID); err != nil {
				t.Fatal(err)
			}
			claim, err = rb.GetMediaJob(ctx, claim.ID, key.ID)
			if err != nil || claim.AccountID != 0 || claim.Quota.AccountID != credential.ID {
				t.Fatalf("deleted account lost quota owner: %+v %v", claim, err)
			}
			for i := range 2 {
				if err := rb.MarkMediaJobQuotaRecorded(ctx, claim, now.Add(time.Duration(i)*time.Second)); err != nil {
					t.Fatal(err)
				}
			}
			ack, err := ra.GetMediaJob(ctx, claim.ID, key.ID)
			if err != nil || ack.Quota.RecordedAt == nil || !ack.Quota.RecordedAt.Equal(now) {
				t.Fatalf("acknowledgement replay moved time: %+v %v", ack, err)
			}
			for _, id := range []string{claim.ID, legacy.ID} {
				if err := ra.MarkMediaJobUsageRecorded(ctx, id, now); err != nil {
					t.Fatal(err)
				}
			}
			if err := ra.DeleteMediaJob(ctx, claim.ID); err != nil {
				t.Fatal(err)
			}
			if rows, err := NewClientKeyRepository(a).DeleteMany(ctx, []uint64{key.ID}); err != nil || rows != 1 {
				t.Fatalf("resolved jobs not deletable: %d %v", rows, err)
			}
		})
	}
}
