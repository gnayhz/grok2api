package journal

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// setup 直接以 gorm SQLite 建 journal 自有表(不经 registry,避免测试包反向
// 依赖 registry→journal 的环)。claim/complete/retry 已收为非导出,断言这些
// 内部机制的测试必须与本实现同包。AccountAllowed 会联查 registry 的
// 案件/账号状态表,这里以空表桩满足查询形状(空表=不阻塞)。
func setup(t *testing.T) (*gorm.DB, *Store) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "journal.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sqlDB, dbErr := db.DB(); sqlDB != nil && dbErr == nil {
			_ = sqlDB.Close()
		}
	})
	if err := db.AutoMigrate(append(Models(), &accountStateStub{}, &casePartyStub{}, &caseStub{})...); err != nil {
		t.Fatal(err)
	}
	return db, New(db)
}

type accountStateStub struct {
	AccountID uint64 `gorm:"column:account_id"`
	State     string `gorm:"column:state"`
}

func (accountStateStub) TableName() string { return "q_account_state" }

type casePartyStub struct {
	Kind        string `gorm:"column:kind"`
	AccountID   uint64 `gorm:"column:account_id"`
	Disposition string `gorm:"column:disposition"`
	CaseID      uint64 `gorm:"column:case_id"`
}

func (casePartyStub) TableName() string { return "q_case_party" }

type caseStub struct {
	ID     uint64 `gorm:"column:id;primaryKey"`
	Status string `gorm:"column:status"`
}

func (caseStub) TableName() string { return "q_case" }

func event(id string, now time.Time) qualitymodel.Event {
	return qualitymodel.Event{Attempt: attemptmeta.Identity{ID: id, RequestID: "request", AccountID: 7, Provider: "grok_build", Revision: 4,
		Path: attemptmeta.Path{NodeID: 9, Epoch: 3, Status: attemptmeta.PathRegistered}}, Stage: "admission", Outcome: "degraded", At: now, HoldUntil: now.Add(time.Minute)}
}

func allowed(t *testing.T, s *Store, want bool, now time.Time) {
	t.Helper()
	got, err := s.AccountAllowed(context.Background(), 7, now)
	if err != nil || got != want {
		t.Fatalf("allowed=%v want=%v err=%v", got, want, err)
	}
}

func TestOutboxLeaseRecoveryFencesOldWorkerAndKeepsIdentity(t *testing.T) {
	r, first := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	e := event("physical-request-1", now)
	if err := first.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	old, err := first.claim(ctx, "worker-old", now, time.Second, 1)
	if err != nil || len(old) != 1 {
		t.Fatalf("claim=%+v err=%v", old, err)
	}
	restarted := New(r)
	claims, err := restarted.claim(ctx, "worker-new", now.Add(time.Second), time.Second, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("recovery=%+v err=%v", claims, err)
	}
	if claims[0].Event.Attempt != e.Attempt || !claims[0].Event.At.Equal(now) {
		t.Fatal("recovery changed physical identity or event time")
	}
	if err := first.complete(ctx, old[0], now.Add(time.Second)); err == nil {
		t.Fatal("old worker acknowledged new lease")
	}
	allowed(t, restarted, false, now)
	if err := restarted.complete(ctx, claims[0], now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	allowed(t, restarted, false, now.Add(time.Second))
	if err := restarted.Release(ctx, e.ID(), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := restarted.Record(ctx, e); err != nil {
		t.Fatal(err)
	}
	allowed(t, restarted, true, now.Add(time.Second))
	var count int64
	if err := r.Table("q_guard_event").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("fact count=%d err=%v", count, err)
	}
}

func TestConcurrentWorkersClaimEachEventOnce(t *testing.T) {
	_, s := setup(t)
	ctx, now := context.Background(), time.Now().UTC()
	if err := s.Record(ctx, event("a", now)); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan []Claim, 2)
	errs := make(chan error, 2)
	for _, id := range []string{"worker-a", "worker-b"} {
		wg.Add(1)
		go func(owner string) {
			defer wg.Done()
			c, e := s.claim(ctx, owner, now, time.Minute, 1)
			results <- c
			errs <- e
		}(id)
	}
	wg.Wait()
	total := len(<-results) + len(<-results)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("claims=%d", total)
	}
}
