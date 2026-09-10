package relational

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/events"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualityregistry "github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/driver/postgres"
)

// A real PostgreSQL server lock exercises cancellation inside the driver, in
// addition to the SQLite tests that exhaust the local connection pool.
func TestPostgresQualityShutdownCancelsActiveSQL(t *testing.T) {
	db, _ := settingsDatabasePair(t, "postgres")
	reg, err := qualityregistry.Open(context.Background(), qualityregistry.Options{Driver: "postgres", PostgresDSN: db.db.Dialector.(*postgres.Dialector).DSN})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	source, err := evidence.New(context.Background(), reg.DB(), evidence.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"q_coordination", "q_observation"} {
		t.Run(table, func(t *testing.T) {
			lock := db.db.Begin()
			if lock.Error != nil {
				t.Fatal(lock.Error)
			}
			defer lock.Rollback()
			if err := lock.Exec("LOCK TABLE " + table + " IN ACCESS EXCLUSIVE MODE").Error; err != nil {
				t.Fatal(err)
			}
			var closeWorker func(context.Context) error
			var pending *journal.Store
			if table == "q_coordination" {
				service := court.New(court.Config{EvaluateEvery: time.Second}, reg, source, nil)
				closeWorker = service.Close
			} else {
				pending = journal.New(reg.DB())
				consumer := events.New(pending, source, nil)
				receipt := events.Receipt{Attempt: attemptmeta.Identity{ID: "pg-cancel/1", AccountID: 7, Provider: "grok_build"}, At: time.Now().UTC(), Outcome: events.Degraded, Rule: "terminal"}
				if err := consumer.RecordQualityEvent(context.Background(), receipt, time.Minute); err != nil {
					t.Fatal(err)
				}
				workerCtx, stop := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() {
					defer close(done)
					err := consumer.Run(workerCtx)
					if !errors.Is(err, context.Canceled) {
						t.Errorf("consumer cancellation=%v", err)
					}
				}()
				closeWorker = func(ctx context.Context) error {
					stop()
					select {
					case <-done:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				}

			}
			defer closeWorker(context.Background())
			deadline := time.Now().Add(3 * time.Second)
			for {
				var waiting bool
				if err := db.db.Raw("SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock' AND query LIKE ?)", "%"+table+"%").Scan(&waiting).Error; err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("worker never entered a real PostgreSQL lock wait")
				}
				time.Sleep(time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			err := closeWorker(ctx)
			cancel()
			if err != nil {
				t.Fatalf("worker could not cancel PostgreSQL: %v", err)
			}

			joinCtx, joinCancel := context.WithTimeout(context.Background(), time.Second)
			defer joinCancel()
			if err := closeWorker(joinCtx); err != nil {
				t.Fatalf("worker did not join before storage release: %v", err)
			}
			if pending != nil {
				stats, err := pending.Stats(context.Background())
				if err != nil || stats.Pending != 1 {
					t.Fatalf("unprocessed incident was lost: %+v %v", stats, err)
				}
			}

		})
	}
}
