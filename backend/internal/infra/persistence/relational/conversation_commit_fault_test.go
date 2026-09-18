package relational

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestHistoryCommitQueryFaultDoesNotBecomeMissingHistory(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, _ := settingsDatabasePair(t, dialect)
			cipher, err := security.NewVersionedCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", nil)
			if err != nil {
				t.Fatal(err)
			}
			j := NewConversationJournal(db, cipher, 1<<20)
			ctx := context.Background()
			r, err := j.Reserve(ctx, journalParams("synthetic-query-failure", "fictional input"))
			if err != nil {
				t.Fatal(err)
			}
			const callback = "synthetic:fail_history_query"
			if err := db.db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == (conversationRequestModel{}).TableName() {
					tx.AddError(driver.ErrBadConn)
				}
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.db.Callback().Query().Remove(callback) })
			p := repository.JournalCommit{Ticket: r.Ticket, ResponseID: "synthetic-response", PrefixHash: "synthetic-prefix", TotalCount: 2, Output: [][]byte{[]byte("fictional output")}, Now: time.Now().UTC()}
			err = j.Commit(ctx, p)
			kind, ok := repository.StoreFaultKindOf(err)
			if !ok || kind != repository.StoreFaultConnection || errors.Is(err, historydomain.ErrHistoryMissing) {
				t.Fatalf("lost transient cause: %v", err)
			}
			if err := db.db.Callback().Query().Remove(callback); err != nil {
				t.Fatal(err)
			}
			if err := j.Commit(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := j.Commit(ctx, p); err != nil {
				t.Fatal("identical acknowledged commit failed:", err)
			}
		})
	}
}
