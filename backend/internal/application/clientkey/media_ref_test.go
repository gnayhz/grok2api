package clientkey

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// TestBatchDeleteHandlesMediaJobReferences 锁定 round 51 修复：
// media_jobs.client_key_id 是 ON DELETE RESTRICT 外键——此前任何作业行
// （含已失败的）都使 key 删除落裸 500。修复后：
// - 活跃作业（queued/in_progress）→ ErrConflict 带可操作计数（409）；
// - 终态作业（completed/failed）→ 行随 key 删除清理（审计行保留快照）。
func TestBatchDeleteHandlesMediaJobReferences(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "media-ref.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	keyRepo := relational.NewClientKeyRepository(database)
	mediaRepo := relational.NewMediaJobRepository(database)
	service := NewService(keyRepo, nil, nil, 60, 5, testCipher(t))
	service.SetMediaJobRepository(mediaRepo)

	created, err := service.Create(ctx, CreateInput{Name: "mr", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	job := media.Job{
		ID: "job_media_ref_1", RequestID: "req_mr_1", ClientKeyID: created.Key.ID, ClientKeyName: "mr",
		Provider: "grok_web", Model: "Web/grok-imagine-video", ModelRouteID: 1, UpstreamModel: "grok-imagine-video",
		Prompt: "x", Seconds: 3, Status: media.StatusFailed, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	if err := mediaRepo.CreateMediaJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	// 终态作业：删除应成功（行被清理，不再触发 RESTRICT）。
	deleted, err := service.BatchDelete(ctx, []uint64{created.Key.ID})
	if err != nil || deleted != 1 {
		t.Fatalf("终态作业下删除 = (%d, %v)，应成功", deleted, err)
	}

	// 活跃作业：应得到 ErrConflict（而非裸 500）。
	created2, err := service.Create(ctx, CreateInput{Name: "mr2", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	job2 := job
	job2.ID, job2.RequestID, job2.ClientKeyID, job2.ClientKeyName = "job_media_ref_2", "req_mr_2", created2.Key.ID, "mr2"
	job2.Status = media.StatusInProgress
	if err := mediaRepo.CreateMediaJob(ctx, job2); err != nil {
		t.Fatal(err)
	}
	_, err = service.BatchDelete(ctx, []uint64{created2.Key.ID})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("活跃作业下删除应 ErrConflict(409)，得到 %v", err)
	}
	// 作业完成后删除成功。
	job2.Status = media.StatusCompleted
	job2.UpdatedAt = time.Now().UTC()
	if err := mediaRepo.UpdateMediaJob(ctx, job2); err != nil {
		t.Fatal(err)
	}
	if deleted, err := service.BatchDelete(ctx, []uint64{created2.Key.ID}); err != nil || deleted != 1 {
		t.Fatalf("完成后删除 = (%d, %v)，应成功", deleted, err)
	}
}
