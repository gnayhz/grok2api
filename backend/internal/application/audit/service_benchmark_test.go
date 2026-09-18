package audit

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// Compare acknowledgement latency and throughput without changing durability:
// each operation waits for the pending journal and authoritative SQL commit.
func BenchmarkAuditCommitDelay(b *testing.B) {
	for _, delay := range []time.Duration{time.Millisecond, 5 * time.Millisecond} {
		for _, workers := range []int{1, 8} {
			b.Run(fmt.Sprintf("delay_%s/workers_%d", delay, workers), func(b *testing.B) {
				db, err := relational.OpenSQLite(context.Background(), filepath.Join(b.TempDir(), "audit.db"))
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() { _ = db.Close() })
				if err := db.InitializeSchema(context.Background()); err != nil {
					b.Fatal(err)
				}
				service := newTestService(b, relational.NewAuditRepository(db), slog.Default(), 16384, 256, 250*time.Millisecond)
				service.UpdateWriterConfig(256, 250*time.Millisecond, delay)
				startAuditService(b, service)
				var next, acknowledged, elapsed atomic.Int64
				var group sync.WaitGroup
				b.ReportAllocs()
				b.ResetTimer()
				for range workers {
					group.Add(1)
					go func() {
						defer group.Done()
						for {
							i := next.Add(1)
							if i > int64(b.N) {
								return
							}
							record := auditdomain.Record{EventID: fmt.Sprintf("synthetic-delay-%d", i), RequestID: fmt.Sprintf("synthetic-request-%d", i), ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC()}
							started := time.Now()
							if err := service.Create(context.Background(), record); err != nil {
								b.Error(err)
								return
							}
							elapsed.Add(int64(time.Since(started)))
							acknowledged.Add(1)
						}
					}()
				}
				group.Wait()
				b.StopTimer()
				if acknowledged.Load() != int64(b.N) {
					b.Fatalf("acknowledged %d of %d", acknowledged.Load(), b.N)
				}
				b.ReportMetric(float64(elapsed.Load())/float64(b.N)/1e6, "ack-ms/op")
			})
		}
	}
}

func BenchmarkAuditServiceSQLite(b *testing.B) {
	for _, attemptCount := range []int{0, 2} {
		b.Run(fmt.Sprintf("attempts-%d", attemptCount), func(b *testing.B) {
			ctx := context.Background()
			database, err := relational.OpenSQLite(ctx, filepath.Join(b.TempDir(), "audit-benchmark.db"))
			if err != nil {
				b.Fatal(err)
			}
			if err := database.InitializeSchema(ctx); err != nil {
				database.Close()
				b.Fatal(err)
			}
			service := newTestService(b, relational.NewAuditRepository(database), slog.Default(), 16_384, 256, 250*time.Millisecond)
			startAuditService(b, service)

			var sequence atomic.Uint64
			errCh := make(chan error, 1)
			baseTime := time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
			b.SetParallelism(4)
			b.ResetTimer()
			b.RunParallel(func(worker *testing.PB) {
				for worker.Next() {
					index := sequence.Add(1)
					record := auditdomain.Record{
						EventID: fmt.Sprintf("evt_benchmark_%020d", index), RequestID: fmt.Sprintf("benchmark-%d", index),
						ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: baseTime.Add(time.Duration(index)),
					}
					for attempt := 1; attempt <= attemptCount; attempt++ {
						record.Attempts = append(record.Attempts, auditdomain.Attempt{
							Number: attempt, Source: auditdomain.AttemptSourceCredential, Stage: "credential", StartedAt: record.CreatedAt,
						})
					}
					if err := service.Create(context.Background(), record); err != nil {
						select {
						case errCh <- err:
						default:
						}
					}
				}
			})
			b.StopTimer()
			closeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			if err := service.Close(closeCtx); err != nil {
				cancel()
				database.Close()
				b.Fatal(err)
			}
			cancel()
			if err := database.Close(); err != nil {
				b.Fatal(err)
			}
			select {
			case err := <-errCh:
				b.Fatal(err)
			default:
			}
		})
	}
}
