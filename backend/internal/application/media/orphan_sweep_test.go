package media

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	localmedia "github.com/chenyme/grok2api/backend/internal/infra/media"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// TestSweepOrphanObjectsReclaimsCrashResidue：saveObject 先硬链接提交对象、
// 后写元数据行——两步之间崩溃会留下无 DB 行的孤儿对象与点前缀临时文件。
// Cleanup 只枚举 DB 行，这类文件不可见且绕过容量统计。扫描必须：删除
// 超过宽限期的孤儿对象与过期临时文件；保留有 DB 行的活动对象与宽限期
// 内的新文件（多实例共享存储下别实例刚提交、行尚未可见的竞态保护）。
func TestSweepOrphanObjectsReclaimsCrashResidue(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "orphan-sweep.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "objects")
	objects, err := localmedia.NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, Config{
		PublicBaseURL: "https://api.example", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: 10 * time.Minute,
	})

	// 活动资产：DB 行 + 文件（必须保留）。
	raw, _ := base64.StdEncoding.DecodeString(onePixelPNG)
	live, err := service.SaveImage(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}

	write := func(key string, age time.Duration) string {
		path := filepath.Join(root, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("residue"), 0o600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-age)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// 崩溃残留：孤儿最终对象 + 过期临时文件（48h 前的 mtime）。
	orphan := write("images/ff/img_orphan-crash.png", 48*time.Hour)
	staleTemp := write("videos/ff/.video-crash-residue", 48*time.Hour)
	// 竞态保护：同样新写入但 mtime 在宽限期内——多实例别实例刚提交。
	freshOrphan := write("images/ee/img_fresh-inflight.png", 0)

	deleted, err := service.sweepOrphanObjects(ctx, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 crash residues removed, got %d", deleted)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("stale orphan object must be reclaimed")
	}
	if _, err := os.Stat(staleTemp); !os.IsNotExist(err) {
		t.Fatal("stale temp file must be reclaimed")
	}
	if _, err := os.Stat(freshOrphan); err != nil {
		t.Fatalf("in-flight file within grace must survive: %v", err)
	}
	if _, _, err := service.OpenImage(ctx, live.ID); err != nil {
		t.Fatalf("live asset must survive sweep: %v", err)
	}

	// 再跑一次：幂等，无残留可删。
	if deleted, err := service.sweepOrphanObjects(ctx, time.Now().UTC()); err != nil || deleted != 0 {
		t.Fatalf("second sweep should be a no-op, got deleted=%d err=%v", deleted, err)
	}
}

// TestRunCleanupTriggersOrphanSweep：RunCleanup 循环必须在首个清理 tick
// 触发孤儿对账（lastSweep 零值），否则崩溃残留仅在进程存活满 24h 后才
// 有机会回收。构造极短清理间隔 + 旧 mtime 孤儿，验证循环确实调用扫描。
func TestRunCleanupTriggersOrphanSweep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "run-sweep.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "objects")
	objects, err := localmedia.NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(relational.NewMediaAssetRepository(database), relational.NewMediaJobRepository(database), objects, nil, Config{
		MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
		CleanupThresholdPercent: 80, CleanupInterval: 30 * time.Millisecond,
	})

	orphanPath := filepath.Join(root, "images", "aa", "img_run-cleanup-orphan.png")
	if err := os.MkdirAll(filepath.Dir(orphanPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanPath, []byte("residue"), 0o600); err != nil {
		t.Fatal(err)
	}
	stamp := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphanPath, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	go service.RunCleanup(ctx, func(error) {})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, statErr := os.Stat(orphanPath); os.IsNotExist(statErr) {
			return // 循环触发的扫描已回收孤儿
		}
		if time.Now().After(deadline) {
			t.Fatal("RunCleanup loop did not reclaim the orphan within deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSweepOrphanObjectsSkipsNonListerStorage：对象存储未实现枚举能力时
// 静默跳过（返回 0、nil），不破坏既有后端行为。
func TestSweepOrphanObjectsSkipsNonListerStorage(t *testing.T) {
	service := NewService(nil, nil, stubObjectStorage{}, nil, Config{})
	deleted, err := service.sweepOrphanObjects(context.Background(), time.Now().UTC())
	if err != nil || deleted != 0 {
		t.Fatalf("non-lister storage must be a no-op, got deleted=%d err=%v", deleted, err)
	}
}

type stubObjectStorage struct{}

func (stubObjectStorage) SaveImage(context.Context, string, string, []byte) (string, error) {
	return "", nil
}
func (stubObjectStorage) SaveVideo(context.Context, string, string, []byte) (string, error) {
	return "", nil
}
func (stubObjectStorage) BeginVideoUpload(context.Context, string, string) (string, string, error) {
	return "", "", nil
}
func (stubObjectStorage) CommitVideoUpload(context.Context, string, string) error { return nil }
func (stubObjectStorage) AbortVideoUpload(context.Context, string) error          { return nil }
func (stubObjectStorage) Open(context.Context, string) (io.ReadCloser, error)     { return nil, nil }
func (stubObjectStorage) Delete(context.Context, string) error                    { return nil }
