package relational

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestConcurrentTicketConsumeExactlyOnce：并发 PUT 同一票据 token 时
// 原子消费必须恰好一个成功——ConsumeUploadTicket 的 WHERE 守卫在行级
// 生效，两个并发消费只有一个 RowsAffected>0。此前只有读失败回滚测试，
// 并发单次语义未锁定。
func TestConcurrentTicketConsumeExactlyOnce(t *testing.T) {
	ctx := context.Background()
	database, err := OpenSQLite(ctx, filepath.Join(t.TempDir(), "ticket-race.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewMediaUploadTicketRepository(database)
	now := time.Now().UTC()
	digest := sha256.Sum256([]byte("race-token"))
	hash := hex.EncodeToString(digest[:])
	if err := repo.CreateUploadTicket(ctx, repository.MediaUploadTicket{
		TokenHash: hash, JobID: "job_ticket_race_probe1", AssetID: "vid_race_probe_asset01",
		MaxBytes: 1 << 20, ExpiresAt: now.Add(time.Hour), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var successCount int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, consumed, err := repo.ConsumeUploadTicket(ctx, hash, time.Now().UTC())
			if err != nil {
				t.Errorf("consume error: %v", err)
				return
			}
			if consumed {
				mu.Lock()
				successCount++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if successCount != 1 {
		t.Fatalf("concurrent consume succeeded %d times, want exactly 1", successCount)
	}
}
