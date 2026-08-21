package audit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// retentionRepo 可编程 DeleteOlderThan mock：脚本化每批返回值与错误。
type retentionRepo struct {
	repository.AuditRepository
	mu       sync.Mutex
	calls    int
	perBatch []int // 每次调用的返回 deleted；长度耗尽后返回 0
	errAt    map[int]error
	cutoffs  []time.Time
	limits   []int
}

func (r *retentionRepo) DeleteOlderThan(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cutoffs = append(r.cutoffs, cutoff)
	r.limits = append(r.limits, limit)
	index := r.calls
	r.calls++
	if err, ok := r.errAt[index]; ok {
		return 0, err
	}
	if index < len(r.perBatch) {
		return r.perBatch[index], nil
	}
	return 0, nil
}

func (r *retentionRepo) stats() (int, []time.Time, []int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls, r.cutoffs, r.limits
}

// TestRunRetentionSweepsUntilDrainedThenSleeps 锁定 sweep 循环语义：
// 连续分批直到某批 < batchSize（排空）即停；cutoff 恒为 now-retention；
// 排空后循环等待（不忙轮询——用取消时序证明）。
func TestRunRetentionSweepsUntilDrainedThenSleeps(t *testing.T) {
	repo := &retentionRepo{perBatch: []int{500, 500, 137}} // 第三批 < 500 → 排空
	service := NewService(repo, nil, 8, 4, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())

	swept := make(chan struct{})
	go func() {
		_ = service.RunRetention(ctx, 24*time.Hour)
		close(swept)
	}()
	// 排空三批应在一轮预算内完成；等待调用数稳定。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		calls, _, _ := repo.stats()
		if calls >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls, cutoffs, limits := repo.stats()
	if calls != 3 {
		t.Fatalf("排空前 DeleteOlderThan 调用数 = %d，应为 3", calls)
	}
	for i, cutoff := range cutoffs {
		want := time.Now().Add(-24 * time.Hour)
		if diff := cutoff.Sub(want); diff < -time.Minute || diff > time.Minute {
			t.Fatalf("第 %d 批 cutoff = %v，应约 now-24h", i, cutoff)
		}
	}
	if limits[0] != auditRetentionBatchSize {
		t.Fatalf("批大小 = %d，应为 %d", limits[0], auditRetentionBatchSize)
	}
	// 排空后不再有新调用（sweep 已进入 ticker 等待）。
	time.Sleep(150 * time.Millisecond)
	callsAfter, _, _ := repo.stats()
	if callsAfter != calls {
		t.Fatalf("排空后仍出现调用：%d → %d（应进入休眠）", calls, callsAfter)
	}
	cancel()
	select {
	case <-swept:
	case <-time.After(time.Second):
		t.Fatal("RunRetention 未随 ctx 取消退出")
	}
}

// TestRunRetentionErrorStopsSweepNotLoop 锁定错误语义：单批失败即结束本轮
// sweep（等下轮 ticker），循环本身存活。
func TestRunRetentionErrorStopsSweepNotLoop(t *testing.T) {
	repo := &retentionRepo{
		perBatch: []int{500},
		errAt:    map[int]error{1: errors.New("db busy")},
	}
	service := NewService(repo, nil, 8, 4, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = service.RunRetention(ctx, 24*time.Hour) }()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		calls, _, _ := repo.stats()
		if calls >= 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	calls, _, _ := repo.stats()
	if calls != 2 {
		t.Fatalf("错误批后调用数 = %d，应为 2（第一批成功+第二批报错即停）", calls)
	}
	// 错误后循环存活：不再追加调用（等 ticker），且进程可取消。
	time.Sleep(150 * time.Millisecond)
	callsAfter, _, _ := repo.stats()
	if callsAfter != calls {
		t.Fatalf("错误后 sweep 未停止：%d → %d", calls, callsAfter)
	}
}

// TestRunRetentionZeroDisables 锁定 retention<=0 的防御路径：直接等待 ctx。
func TestRunRetentionZeroDisables(t *testing.T) {
	repo := &retentionRepo{}
	service := NewService(repo, nil, 8, 4, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = service.RunRetention(ctx, 0)
		close(done)
	}()
	time.Sleep(100 * time.Millisecond)
	if calls, _, _ := repo.stats(); calls != 0 {
		t.Fatalf("retention=0 不应产生删除调用，得到 %d", calls)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("retention=0 未随取消退出")
	}
}

var _ = auditdomain.Record{} // 保持 audit 导入（mock 接口完整性需要）
