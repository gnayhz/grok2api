package relational

import (
	"context"
	"path/filepath"
	"testing"
)

func newTestAuditJournal(t testing.TB, capacity int) *AuditJournal {
	t.Helper()
	journal, err := OpenAuditJournal(context.Background(), filepath.Join(t.TempDir(), "pending.db"), AuditJournalOptions{MaxRecords: capacity, MaxBytes: 64 << 20})
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
