package relational

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func TestConstraintLookupUsesResolvedTableSchema(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("isolated PostgreSQL required")
	}
	ctx := context.Background()
	admin, err := OpenPostgres(ctx, dsn, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	prefix := fmt.Sprintf("g34_%d", time.Now().UnixNano())
	currentSchema, foreignSchema := prefix+"_current", prefix+"_foreign"
	for _, schema := range []string{currentSchema, foreignSchema} {
		if err := admin.db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
			t.Fatal(err)
		}
		defer admin.db.Exec("DROP SCHEMA IF EXISTS " + schema + " CASCADE")
	}
	if err := admin.db.Exec("CREATE TABLE " + currentSchema + ".g34_table (value integer)").Error; err != nil {
		t.Fatal(err)
	}
	if err := admin.db.Exec("CREATE TABLE " + foreignSchema + ".g34_table (value integer, CONSTRAINT g34_rule CHECK (value = 734))").Error; err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", currentSchema)
	parsed.RawQuery = query.Encode()
	active, err := OpenPostgres(ctx, parsed.String(), 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer active.Close()
	definition, err := active.constraintDefinition(ctx, consoleConstraint{table: "g34_table", name: "g34_rule"})
	if err != nil {
		t.Fatal(err)
	}
	if definition != "" {
		t.Fatalf("foreign constraint leaked into active schema: %q", definition)
	}
	if err := active.db.Exec("ALTER TABLE g34_table ADD CONSTRAINT g34_rule CHECK (value = 213)").Error; err != nil {
		t.Fatal(err)
	}
	definition, err = active.constraintDefinition(ctx, consoleConstraint{table: "g34_table", name: "g34_rule"})
	if err != nil || !strings.Contains(definition, "213") || strings.Contains(definition, "734") {
		t.Fatalf("current definition=%q err=%v", definition, err)
	}
	if err := admin.db.Exec("DROP SCHEMA " + foreignSchema + " CASCADE").Error; err != nil {
		t.Fatal(err)
	}
	definition, err = active.constraintDefinition(ctx, consoleConstraint{table: "g34_table", name: "g34_rule"})
	if err != nil || !strings.Contains(definition, "213") {
		t.Fatalf("foreign drop changed active definition=%q err=%v", definition, err)
	}
}
