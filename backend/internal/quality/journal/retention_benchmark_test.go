package journal_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"gorm.io/gorm"
)

// Use a file-backed WAL database and 1 KiB archival payloads. Fixture writes
// are excluded: the measured operation drains both event and outbox rows.
func BenchmarkRetentionDrain(b *testing.B) {
	const records = 20000
	ctx, now := context.Background(), time.Now().UTC()
	old := now.Add(-8 * 24 * time.Hour)
	b.StopTimer()
	r, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(b.TempDir(), "retention.db")})
	if err != nil {
		b.Fatal(err)
	}
	defer r.Close()
	store := journal.New(r.DB())
	for iteration := 0; iteration < b.N; iteration++ {
		if err := r.DB().Transaction(func(tx *gorm.DB) error {
			for start := 0; start < records; start += 500 {
				events, outbox := make([]journal.EventRow, 0, 500), make([]journal.OutboxRow, 0, 500)
				for n := start; n < start+500; n++ {
					id := fmt.Sprintf("archive-%06d/completion", n)
					events = append(events, journal.EventRow{ID: id, AttemptID: id, AccountID: 1, Stage: "completion", Outcome: "completed", Payload: strings.Repeat("x", 1024), ObservedAt: old})
					outbox = append(outbox, journal.OutboxRow{EventID: id, ReadyAt: old, ProcessedAt: &old})
				}
				if err := tx.Create(&events).Error; err != nil {
					return err
				}
				if err := tx.Create(&outbox).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		err := store.Sweep(ctx, now)
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		for _, table := range []any{&journal.EventRow{}, &journal.OutboxRow{}} {
			var count int64
			if err := r.DB().Model(table).Count(&count).Error; err != nil || count != 0 {
				b.Fatalf("retention left rows: count=%d err=%v", count, err)
			}
		}
	}
	b.ReportMetric(float64(records*b.N)/b.Elapsed().Seconds(), "events/s")
}
