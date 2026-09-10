package relational

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAuditDetailSnapshotSurvivesConcurrentRetention(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			key := settlementTestKey(t, a, "detail-snapshot")
			value := settlementRecord(key.ID, "detail-snapshot", 30)
			value.CreatedAt = time.Now().UTC().Add(-48 * time.Hour)
			value.GenerationUsages = []audit.GenerationUsage{{PhysicalID: "detail-snapshot/1", Ordinal: 1, AccountID: 1, Selected: true, Outcome: "completed", UsageSource: audit.UsageSourceUpstream, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 30}}
			reader, cleaner := NewAuditRepository(a), NewAuditRepository(b)
			if err := reader.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			rows, _, err := reader.List(ctx, 0, 1)
			if err != nil || len(rows) != 1 {
				t.Fatalf("seed=%v %v", rows, err)
			}
			id := rows[0].ID
			entered, release := make(chan struct{}), make(chan struct{})
			var once atomic.Bool
			const hook = "audit_detail_after_primary"
			if err := a.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "request_audits" && tx.Error == nil && once.CompareAndSwap(false, true) {
					close(entered)
					select {
					case <-release:
					case <-tx.Statement.Context.Done():
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = a.db.Callback().Query().Remove(hook) })
			type result struct {
				value audit.Record
				err   error
			}
			done := make(chan result, 1)
			go func() { v, e := reader.Get(ctx, id); done <- result{v, e} }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			type deletion struct {
				count int
				err   error
			}
			deleted := make(chan deletion, 1)
			go func() { n, err := cleaner.DeleteOlderThan(ctx, time.Now().UTC(), 10); deleted <- deletion{n, err} }()
			// PostgreSQL's snapshot permits deletion before the next detail read.
			// SQLite uses the repository's existing BEGIN IMMEDIATE policy, which
			// holds the writer until this short read transaction completes.
			if dialect == "postgres" {
				select {
				case result := <-deleted:
					deleted <- result
				case <-ctx.Done():
					close(release)
					t.Fatal(ctx.Err())
				}
			}
			close(release)
			var got result
			select {
			case got = <-done:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if got.err != nil || got.value.AttemptCount != 1 || len(got.value.Attempts) != 1 || len(got.value.GenerationUsages) != 1 {
				t.Fatalf("torn successful detail: err=%v count=%d attempts=%d generations=%d", got.err, got.value.AttemptCount, len(got.value.Attempts), len(got.value.GenerationUsages))
			}
			select {
			case result := <-deleted:
				if result.err != nil || result.count != 1 {
					t.Fatalf("delete=%d err=%v", result.count, result.err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if _, err := reader.Get(ctx, id); !errors.Is(err, repository.ErrNotFound) {
				t.Fatalf("next request must observe retention: %v", err)
			}
			assertSettlementKey(t, b, key.ID, 30, 0)
			if tableRowCount(t, b, "billing_settlements") != 1 {
				t.Fatal("retention lost permanent settlement")
			}
		})
	}
}

func TestAuditDetailSnapshotFailureReturnsNoPartialRecord(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, _ := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "detail-failure")
			repo := NewAuditRepository(a)
			value := settlementRecord(key.ID, "detail-failure", 30)
			if err := repo.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			rows, _, err := repo.List(ctx, 0, 1)
			if err != nil || len(rows) != 1 {
				t.Fatal(err)
			}
			id := rows[0].ID
			for _, table := range []string{"request_audits", "request_audit_attempts", "request_audit_generations"} {
				t.Run(table, func(t *testing.T) {
					fault := errors.New("injected audit detail query failure")
					const hook = "audit_detail_query_failure"
					if err := a.db.Callback().Query().Before("gorm:query").Register(hook, func(tx *gorm.DB) {
						if tx.Statement.Table == table {
							tx.AddError(fault)
						}
					}); err != nil {
						t.Fatal(err)
					}
					defer a.db.Callback().Query().Remove(hook)
					got, err := repo.Get(ctx, id)
					if !errors.Is(err, fault) || got.ID != 0 || len(got.Attempts) != 0 || len(got.GenerationUsages) != 0 {
						t.Fatalf("partial successful/error result: %+v %v", got, err)
					}
				})
			}
			cancelCtx, cancel := context.WithCancel(ctx)
			const hook = "audit_detail_cancel_after_primary"
			if err := a.db.Callback().Query().After("gorm:query").Register(hook, func(tx *gorm.DB) {
				if tx.Statement.Table == "request_audits" {
					cancel()
				}
			}); err != nil {
				t.Fatal(err)
			}
			got, err := repo.Get(cancelCtx, id)
			_ = a.db.Callback().Query().Remove(hook)
			cancel()
			if !errors.Is(err, context.Canceled) || got.ID != 0 {
				t.Fatalf("canceled detail=%+v err=%v", got, err)
			}
			if got, err := repo.Get(ctx, id); err != nil || got.ID != id || len(got.Attempts) != 1 {
				t.Fatalf("following detail failed: %+v %v", got, err)
			}
			sqlDB, err := a.db.DB()
			if err != nil {
				t.Fatal(err)
			}
			if inUse := sqlDB.Stats().InUse; inUse != 0 {
				t.Fatalf("snapshot retained %d SQL connections", inUse)
			}
		})
	}
}
