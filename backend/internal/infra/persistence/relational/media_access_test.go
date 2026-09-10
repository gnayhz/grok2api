package relational

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestMediaJobAccessPolicyMigrationAndClaim(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repoA, repoB := NewMediaJobRepository(a), NewMediaJobRepository(b)
			key := clientKeyModel{Name: "video-access", Prefix: "video-access", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			old := testMediaJob("video_old_access_policy", 0, key.ID, media.StatusQueued, now)
			old.Prompt = "preserved legacy input"
			if err := repoA.CreateMediaJob(ctx, old); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().DropConstraint(&mediaJobModel{}, "chk_media_jobs_access_policy"); err != nil {
				t.Fatal(err)
			}
			for _, column := range []string{"account_tier_scope", "account_provider_scope", "access_policy_version"} {
				if err := a.db.Migrator().DropColumn(&mediaJobModel{}, column); err != nil {
					t.Fatal(err)
				}
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			stored, err := repoB.GetMediaJob(ctx, old.ID, key.ID)
			if err != nil || stored.Prompt != old.Prompt || !stored.AccessPolicy.IsLegacy() {
				t.Fatalf("migration fabricated or lost permission: %+v %v", stored, err)
			}
			first, claimed, err := repoA.TryClaimMediaJob(ctx, old.ID, now, now.Add(time.Minute), "claim_video_access_first")
			if err != nil || !claimed {
				t.Fatalf("first claim: %v %v", claimed, err)
			}
			second, claimed, err := repoB.TryClaimMediaJob(ctx, old.ID, now.Add(2*time.Minute), now.Add(3*time.Minute), "claim_video_access_second")
			if err != nil || !claimed {
				t.Fatalf("second claim: %v %v", claimed, err)
			}
			free, err := media.NewJobAccessPolicy(clientkey.AccountScope{Providers: clientkey.ProviderScopeWeb, Tiers: clientkey.TierScopeFree})
			if err != nil {
				t.Fatal(err)
			}
			if err := repoA.SaveMediaJobAccessPolicy(ctx, old.ID, first.ClaimToken, free); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("stale claim adopted policy: %v", err)
			}
			results := make(chan error, 8)
			var wg sync.WaitGroup
			for i := range 8 {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					repo := repoA
					if i%2 != 0 {
						repo = repoB
					}
					results <- repo.SaveMediaJobAccessPolicy(ctx, old.ID, second.ClaimToken, free)
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
				t.Fatalf("policy adopted %d times", accepted)
			}
			stored, err = repoA.GetMediaJob(ctx, old.ID, key.ID)
			if err != nil || stored.AccessPolicy != free {
				t.Fatalf("stored policy=%+v err=%v", stored.AccessPolicy, err)
			}
			all, _ := media.NewJobAccessPolicy(clientkey.AccountScope{})
			stored.AccessPolicy, stored.Progress = all, 12
			if err := repoB.UpdateMediaJob(ctx, stored); err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			stored, err = repoA.GetMediaJob(ctx, old.ID, key.ID)
			if err != nil || stored.AccessPolicy != free || stored.Progress != 12 {
				t.Fatalf("progress or restart changed permission: %+v %v", stored, err)
			}
			if err := repoB.SaveMediaJobAccessPolicy(ctx, old.ID, second.ClaimToken, all); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("existing grant overwritten: %v", err)
			}
			fresh := old
			fresh.ID, fresh.AccessPolicy = "video_new_access_policy", all
			if err := repoA.CreateMediaJob(ctx, fresh); err != nil {
				t.Fatal(err)
			}
			stored, err = repoB.GetMediaJob(ctx, fresh.ID, key.ID)
			if err != nil || stored.AccessPolicy != all || stored.AccessPolicy.IsLegacy() {
				t.Fatalf("explicit all lost: %+v %v", stored.AccessPolicy, err)
			}
			for i, policy := range []media.JobAccessPolicy{
				{Version: 1},
				{Version: 2, AccountScope: all.AccountScope},
				{AccountScope: free.AccountScope},
				{Version: 1, AccountScope: clientkey.AccountScope{Providers: 8, Tiers: 1}},
				{Version: 1, AccountScope: clientkey.AccountScope{Providers: 2, Tiers: 4}},
			} {
				invalid := old
				invalid.ID, invalid.AccessPolicy = fmt.Sprintf("video_invalid_policy_%d", i), policy
				if err := repoA.CreateMediaJob(ctx, invalid); err == nil {
					t.Fatalf("domain accepted invalid grant %+v", policy)
				}
				if err := a.db.Create(mediaJobFromDomain(invalid)).Error; err == nil {
					t.Fatalf("database accepted invalid grant %+v", policy)
				}
			}
		})
	}
}
