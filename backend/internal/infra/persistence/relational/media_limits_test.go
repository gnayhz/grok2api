package relational

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobExecutionLimitsMigrationAndFencing(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			ra, rb := NewMediaJobRepository(a), NewMediaJobRepository(b)
			key := clientKeyModel{Name: "limits", Prefix: "limits", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			job := testMediaJob("video_limits_legacy", 0, key.ID, media.StatusQueued, now)
			if err := ra.CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().DropConstraint(&mediaJobModel{}, "chk_media_jobs_execution_limits"); err != nil {
				t.Fatal(err)
			}
			for _, column := range []string{"physical_confirmed", "physical_reserved", "physical_limit", "execution_deadline", "limits_version"} {
				if err := a.db.Migrator().DropColumn(&mediaJobModel{}, column); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			claim, ok, err := ra.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Minute), "first-limits-claim")
			if err != nil || !ok {
				t.Fatalf("claim=%v err=%v", ok, err)
			}
			if claim.Limits != (media.ExecutionLimits{}) || claim.InputJSON != job.InputJSON || claim.Prompt != job.Prompt {
				t.Fatal("migration changed legacy input or invented an execution policy")
			}
			deadline := now.Add(time.Hour)
			limits := media.ExecutionLimits{Version: 1, Deadline: &deadline, PhysicalLimit: 3}
			if err := ra.StartMediaJobExecutionLimits(ctx, job.ID, claim.ClaimToken, limits); err != nil {
				t.Fatal(err)
			}
			if err := rb.StartMediaJobExecutionLimits(ctx, job.ID, claim.ClaimToken, limits); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("policy replaced: %v", err)
			}
			for range 2 {
				if err := ra.ReserveMediaJobPhysicalCall(ctx, job.ID, claim.ClaimToken, now); err != nil {
					t.Fatal(err)
				}
			}
			results := make(chan error, 8)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					r := ra
					if i%2 == 1 {
						r = rb
					}
					results <- r.ReserveMediaJobPhysicalCall(ctx, job.ID, claim.ClaimToken, now)
				}()
			}
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err == nil {
					successes++
				} else if !errors.Is(err, media.ErrPhysicalBudgetExhausted) {
					t.Fatal(err)
				}
			}
			if successes != 1 {
				t.Fatalf("last permit consumed %d times", successes)
			}
			claim.Limits = media.ExecutionLimits{Version: 1, Deadline: &deadline, PhysicalLimit: 999, Reserved: 0}
			expired := now.Add(-time.Second)
			claim.LeaseUntil = &expired
			if err := ra.UpdateMediaJob(ctx, claim); err != nil {
				t.Fatal(err)
			}
			current, ok, err := rb.TryClaimMediaJob(ctx, job.ID, now, now.Add(time.Minute), "second-limits-claim")
			if err != nil || !ok {
				t.Fatalf("recovery=%v err=%v", ok, err)
			}
			if current.Limits.Reserved != 3 || current.Limits.PhysicalLimit != 3 || !current.Limits.Deadline.Equal(deadline) {
				t.Fatalf("recovery or generic write reset limits: %+v", current.Limits)
			}
			if err := ra.ReserveMediaJobPhysicalCall(ctx, job.ID, claim.ClaimToken, now); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("stale worker reserved: %v", err)
			}
			if err := rb.ReserveMediaJobPhysicalCall(ctx, job.ID, current.ClaimToken, now); !errors.Is(err, media.ErrPhysicalBudgetExhausted) {
				t.Fatalf("recovery renewed budget: %v", err)
			}
			if err := ra.ConfirmMediaJobPhysicalCalls(ctx, job.ID, claim.ClaimToken, 0, 2); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("old receipt accepted: %v", err)
			}
			for range 2 {
				if err := rb.ConfirmMediaJobPhysicalCalls(ctx, job.ID, current.ClaimToken, 0, 2); err != nil {
					t.Fatal(err)
				}
			}
			if err := rb.ConfirmMediaJobPhysicalCalls(ctx, job.ID, current.ClaimToken, 0, 1); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("confirmation regressed: %v", err)
			}
			if err := rb.ConfirmMediaJobPhysicalCalls(ctx, job.ID, current.ClaimToken, 2, 4); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("unreserved fact confirmed: %v", err)
			}
			if err := rb.ConfirmMediaJobPhysicalCalls(ctx, job.ID, current.ClaimToken, 2, 3); err != nil {
				t.Fatal(err)
			}
			stored, err := ra.GetMediaJob(ctx, job.ID, key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Limits.Confirmed != 3 {
				t.Fatalf("duplicate receipt acknowledgement: %+v", stored.Limits)
			}
			expiredJob := testMediaJob("video_limits_expired", 0, key.ID, media.StatusQueued, now)
			if err := ra.CreateMediaJob(ctx, expiredJob); err != nil {
				t.Fatal(err)
			}
			expiredClaim, ok, err := ra.TryClaimMediaJob(ctx, expiredJob.ID, now, now.Add(time.Minute), "expired-limits-claim")
			if err != nil || !ok {
				t.Fatal(err)
			}
			limits.Deadline = &expired
			if err := ra.StartMediaJobExecutionLimits(ctx, expiredJob.ID, expiredClaim.ClaimToken, limits); err != nil {
				t.Fatal(err)
			}
			if err := rb.ReserveMediaJobPhysicalCall(ctx, expiredJob.ID, expiredClaim.ClaimToken, now); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expired deadline renewed: %v", err)
			}
			for _, invalid := range []map[string]any{{"physical_confirmed": 4}, {"physical_reserved": 4}, {"physical_limit": 0}, {"limits_version": 0}, {"execution_deadline": nil}} {
				if err := a.db.Model(&mediaJobModel{}).Where("id = ?", job.ID).Updates(invalid).Error; err == nil {
					t.Fatalf("invalid SQL limits accepted: %v", invalid)
				}
			}
		})
	}
}
