package audit

import (
	"context"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"path/filepath"
	"testing"
)

func newTestAuditJournal(t testing.TB, capacity int) *relational.AuditJournal {
	t.Helper()
	journal, err := relational.OpenAuditJournal(context.Background(), filepath.Join(t.TempDir(), "pending.db"), relational.AuditJournalOptions{MaxRecords: capacity, MaxBytes: 64 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := journal.Close(); err != nil {
			t.Errorf("close journal: %v", err)
		}
	})
	return journal
}
