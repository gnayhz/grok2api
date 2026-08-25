package relational

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
)

// TestMediaJobLeaseExpiryMakesJobReclaimable：视频任务崩溃恢复的核心
// 不变量——执行实例失联后 lease_until 过期，任务必须重新可被原子认领；
// 未过期的活跃租约必须拒绝第二认领（多实例不重复执行的另一面）。
func TestMediaJobLeaseExpiryMakesJobReclaimable(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "lease-reclaim.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}

	accountValue, _, err := NewAccountRepository(database).UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO,
		WebTier: accountdomain.WebTierBasic, Name: "lease-account", SourceKey: "lease-account",
		EncryptedAccessToken: testEncryptedToken, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err != nil {
		t.Fatal(err)
	}
	key := clientKeyModel{Name: "lease-key", Prefix: "lease-key", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
	if err := database.db.WithContext(ctx).Create(&key).Error; err != nil {
		t.Fatal(err)
	}

	repo := NewMediaJobRepository(database)
	now := time.Now().UTC()
	job := testMediaJob("media_job_lease_reclaim_probe", accountValue.ID, key.ID, mediadomain.StatusQueued, now)
	if err := repo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}

	// 第一个实例认领：租约覆盖整个执行窗口。
	first, ok, err := repo.TryClaimMediaJob(ctx, job.ID, now, now.Add(20*time.Minute), "claim_aaaaaaaaaaaaaa1")
	if err != nil || !ok {
		t.Fatalf("first claim failed: ok=%v err=%v", ok, err)
	}
	if first.ID != job.ID {
		t.Fatalf("first claim returned %+v", first)
	}

	// 活跃租约期内：第二实例必须被拒。
	if _, stolen, err := repo.TryClaimMediaJob(ctx, job.ID, now.Add(time.Minute), now.Add(time.Hour), "claim_bbbbbbbbbbbbbb2"); err != nil || stolen {
		t.Fatalf("active lease stolen: stolen=%v err=%v", stolen, err)
	}

	// 恢复列表也不得包含活跃租约任务。
	due, err := repo.ListRecoverableMediaJobs(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range due {
		if item.ID == job.ID {
			t.Fatal("job with active lease listed as recoverable")
		}
	}

	// 租约过期（实例失联）：任务必须重新可认领。
	expired := now.Add(20*time.Minute + time.Second)
	second, reclaimed, err := repo.TryClaimMediaJob(ctx, job.ID, expired, expired.Add(time.Hour), "claim_ccccccccccccc3")
	if err != nil || !reclaimed {
		t.Fatalf("expired lease not reclaimable: ok=%v err=%v", reclaimed, err)
	}
	if second.ID != job.ID {
		t.Fatalf("second claim returned %+v", second)
	}
}
