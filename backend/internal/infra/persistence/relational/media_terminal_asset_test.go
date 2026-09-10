package relational

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"testing"
	"time"
)

func TestTerminalVideoAssetCannotBeRebound(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := clientKeyModel{Name: "terminal-asset", Prefix: "terminal-asset", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 60, MaxConcurrent: 4}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			job := testMediaJob("video_terminal_asset", 0, key.ID, media.StatusCompleted, now)
			job.ResultAssetID = "vid_terminal_original_001"
			jobs := NewMediaJobRepository(a)
			if err := jobs.CreateMediaJob(ctx, job); err != nil {
				t.Fatal(err)
			}
			if err := NewMediaUploadTicketRepository(b).BindLegacyJobResultAsset(ctx, job.ID, "vid_late_other_result_001"); !errors.Is(err, repository.ErrNotFound) {
				t.Errorf("late upload could rebind completed legacy job: %v", err)
			}
			stored, err := jobs.GetMediaJob(ctx, job.ID, key.ID)
			if err != nil || stored.ResultAssetID != job.ResultAssetID {
				t.Fatalf("completed output replaced: asset=%s want=%s err=%v", stored.ResultAssetID, job.ResultAssetID, err)
			}
		})
	}
}
