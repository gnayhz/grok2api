package audit

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"log/slog"
	"path/filepath"
	"testing"
	"time"
)

func newTestService(t testing.TB, repo repository.AuditRepository, logger *slog.Logger, capacity, batch int, flush time.Duration) *Service {
	t.Helper()
	journal, err := relational.OpenAuditJournal(context.Background(), filepath.Join(t.TempDir(), "pending.db"), relational.AuditJournalOptions{MaxRecords: capacity, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repo, journal, logger, batch, flush)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Errorf("close writer: %v", err)
			return
		}
		if err := journal.Close(); err != nil {
			t.Errorf("close journal: %v", err)
		}
	})
	return service
}
func startAuditService(t testing.TB, service *Service) {
	t.Helper()
	if err := service.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
}
