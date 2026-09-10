package account

import (
	"context"
	"database/sql"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"net/url"
	"path/filepath"
	"time"
)

// Only used to create historical SQL state in test-owned SQLite files.
// Real requests and management changes still execute their production owners.
func seedHealthFixture(path string, ctx context.Context, id uint64, provider account.Provider, count int, until *time.Time, reason string, success bool) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	uri := (&url.URL{Scheme: "file", Path: absolute}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		return err
	}
	defer db.Close()
	var marked, used *time.Time
	now := time.Now().UTC()
	if until != nil {
		marked = &now
	}
	if success {
		used = &now
	}
	_, err = db.ExecContext(ctx, "UPDATE provider_accounts SET health_revision = health_revision + 1, failure_count = ?, cooldown_until = ?, cooldown_marked_at = ?, last_error = ?, last_used_at = ? WHERE id = ? AND provider = ?", count, until, marked, reason, used, id, string(provider))
	return err
}
