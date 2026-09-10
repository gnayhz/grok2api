package web

import (
	"context"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/jackc/pgx/v5"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func statsigWaiterDatabase(t *testing.T, driver string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var db *relational.Database
	var err error
	if driver == "sqlite" {
		db, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "statsig-waiter.db"))
	} else {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires isolated TEST_POSTGRES_DSN")
		}
		admin, connectErr := pgx.Connect(ctx, dsn)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		t.Cleanup(func() { _ = admin.Close(ctx) })
		schema := fmt.Sprintf("web_statsig_waiter_%d", time.Now().UnixNano())
		if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		db, err = relational.OpenPostgres(ctx, parsed.String(), 4, 4)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return db
}
