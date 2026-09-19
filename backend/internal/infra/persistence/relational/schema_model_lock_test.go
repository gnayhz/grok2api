package relational

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestPostgresSchemaMigrationDoesNotInvertModelWriterLocks(t *testing.T) {
	writer, migrator := settingsDatabasePair(t, "postgres")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tx := writer.db.WithContext(ctx).Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	if err := lockModelNamespaces(tx, account.ProviderConsole); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- migrator.InitializeSchema(ctx) }()
	joined := false
	defer func() {
		cancel()
		tx.Rollback()
		if !joined {
			<-done
		}
	}()
	waitMediaDeletionLock(t, ctx, writer, "pg_advisory_xact_lock", nil)
	// The migration must wait before taking any model table locks, allowing
	// the namespace owner to finish its write and release the namespace.
	if err := tx.Exec("UPDATE model_routes SET updated_at = updated_at WHERE provider = ?", account.ProviderConsole).Error; err != nil {
		t.Fatal("migration blocked the writer while waiting for its namespace:", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}
	err := <-done
	joined = true
	if err != nil {
		t.Fatal(err)
	}
}
