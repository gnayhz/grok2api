package court

import (
	"context"
	"encoding/json"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/gorm"
)

func TestSlowCaseCannotStarveOtherExpiredCase(t *testing.T) {
	b := newBench(t)
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.InvestigationTimeout = time.Second
	s := newFixtureCourt(cfg, b.registry, storeSource{b.evidence}, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	defer s.Close(context.Background())
	first := openSimpleTestCase(t, s, b.registry)
	if err := s.ReportDegraded(context.Background(), 8, model.EpochKey{NodeID: 4}); err != nil {
		t.Fatal(err)
	}
	second, err := b.registry.OpenCaseForIncident(context.Background(), 8, model.EpochKey{NodeID: 4})
	if err != nil || second == 0 || second == first {
		t.Fatalf("second=%d err=%v", second, err)
	}
	var block atomic.Bool
	if err := b.registry.DB().Callback().Update().Before("gorm:update").Register("review:slow_case_update", func(db *gorm.DB) {
		if db.Statement.Table == "q_case" && block.CompareAndSwap(true, false) {
			// A row lock or slow query obeys its context, but consumes the whole
			// shared evaluation budget before later cases get their turn.
			<-db.Statement.Context.Done()
			db.AddError(db.Statement.Context.Err())
		}
	}); err != nil {
		t.Fatal(err)
	}
	defer b.registry.DB().Callback().Update().Remove("review:slow_case_update")
	for attempt := 1; attempt <= 3; attempt++ {
		block.Store(true)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, err := s.Evaluate(ctx, time.Now().Add(time.Minute))
		cancel()
		if err == nil {
			t.Fatal("expected slow first case to hit its own deadline")
		}
		row, found, err := b.registry.GetCase(context.Background(), second)
		if err != nil || !found || row.Status == model.CaseInvestigating {
			t.Fatalf("second=%+v found=%v err=%v", row, found, err)
		}
		if !b.registry.AccountEligible(8) {
			t.Fatal("unrelated expired hold remained")
		}
		t.Logf("ISOLATION VERIFIED: evaluation=%d unrelated_expired_case=%d status=%s still_held=false", attempt, second, row.Status)
	}
}

func TestPostgresRowLockCannotStarveOtherDeadline(t *testing.T) {
	dsn := os.Getenv("GROK_EVOLUTION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("GROK_EVOLUTION_POSTGRES_DSN requires an isolated PostgreSQL database")
	}
	ctx, now := context.Background(), time.Now().UTC()
	r, err := registry.Open(ctx, registry.Options{Driver: "postgres", PostgresDSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	archive, err := evidence.New(ctx, r.DB(), model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	policy := caseProofPolicy(711, model.EpochKey{}, model.Observation{}, now.Add(-time.Hour), cfg)
	raw, err := json.Marshal(map[string]any{"policy": policy})
	if err != nil {
		t.Fatal(err)
	}
	first, err := r.OpenInvestigation(ctx, 711, model.EpochKey{}, now.Add(-time.Hour), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.OpenInvestigation(ctx, 712, model.EpochKey{}, now.Add(-time.Hour), string(raw))
	if err != nil {
		t.Fatal(err)
	}
	s := newFixtureCourt(cfg, r, storeSource{archive}, nil)
	defer s.Close(context.Background())
	locked := r.DB().Begin()
	if locked.Error != nil {
		t.Fatal(locked.Error)
	}
	defer locked.Rollback()
	if err := locked.Exec("UPDATE q_case SET updated_at = updated_at WHERE id = ?", first).Error; err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		workCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := s.Evaluate(workCtx, now)
		cancel()
		if err == nil {
			t.Fatal("expected row lock timeout")
		}
		row, found, err := r.GetCase(ctx, second)
		if err != nil || !found || row.Status == model.CaseInvestigating || !r.AccountEligible(712) {
			t.Fatalf("row=%+v found=%v err=%v", row, found, err)
		}
		t.Logf("ISOLATION VERIFIED WITH POSTGRESQL: evaluation=%d first_case_row_locked=%d unrelated_expired_case=%d still_held=false", attempt, first, second)
	}
	if err := locked.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	if _, err := s.Evaluate(ctx, now); err != nil {
		t.Fatal(err)
	}
	if !r.AccountEligible(712) {
		t.Fatal("release failed after unrelated lock removed")
	}
	t.Log("Both cases finish when the unrelated row lock is removed")
}

type deadlineFirstSource struct {
	storeSource
	check func()
}

func (s deadlineFirstSource) SnapshotWindow(now time.Time) model.Snapshot {
	s.check()
	return s.storeSource.SnapshotWindow(now)
}

func TestExpiredCaseClosesBeforeTrafficAggregation(t *testing.T) {
	b := newBench(t)
	var armed, called atomic.Bool
	source := deadlineFirstSource{storeSource: storeSource{b.evidence}, check: func() {
		if armed.Load() {
			called.Store(true)
			if !b.registry.AccountEligible(7) {
				t.Error("traffic aggregation ran before expired hold release")
			}
		}
	}}
	cfg := DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	cfg.InvestigationTimeout = time.Second
	s := newFixtureCourt(cfg, b.registry, source, simpleTaskDispatcher{registry.NewProbeTaskStore(b.registry)})
	defer s.Close(context.Background())
	openSimpleTestCase(t, s, b.registry)
	armed.Store(true)
	if _, err := s.Evaluate(context.Background(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if !called.Load() {
		t.Fatal("discovery was not exercised")
	}
}
