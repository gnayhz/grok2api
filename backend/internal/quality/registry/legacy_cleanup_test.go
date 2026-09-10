package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	glebarezsqlite "github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// TestOpenMigratesRetiredQualityStates verifies that a database written by
// the removed bail/final state machine cannot leave accounts permanently
// unroutable after the direct-loop deployment.
func TestOpenMigratesRetiredQualityStates(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-quality.db")
	first, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := gorm.Open(glebarezsqlite.Open("file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := raw.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	now := time.Now().UTC()
	for _, table := range []string{"q_account_state", "q_case_party"} {
		if err := raw.Exec("DROP TABLE " + table).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Exec(`CREATE TABLE q_account_state (
account_id INTEGER PRIMARY KEY,
state TEXT NOT NULL,
state_since DATETIME NOT NULL,
current_case_id INTEGER NOT NULL DEFAULT 0,
trial_stage INTEGER NOT NULL DEFAULT 0,
next_review_at DATETIME,
re_evidence_at DATETIME,
updated_at DATETIME NOT NULL
)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec(`CREATE TABLE q_case_party (
id INTEGER PRIMARY KEY AUTOINCREMENT,
case_id INTEGER NOT NULL,
kind TEXT NOT NULL,
account_id INTEGER NOT NULL DEFAULT 0,
node_id INTEGER NOT NULL DEFAULT 0,
epoch INTEGER NOT NULL DEFAULT 0,
role TEXT NOT NULL,
disposition TEXT NOT NULL,
updated_at DATETIME NOT NULL
)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec("INSERT INTO q_case (id, status, verdict, evidence_json, opened_at, updated_at) VALUES (1, 'investigating', '', '', ?, ?)", now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec("INSERT INTO q_account_state (account_id, state, state_since, current_case_id, trial_stage, updated_at) VALUES (21, 'bail', ?, 1, 0, ?), (22, 'final', ?, 1, 2, ?)", now, now, now, now).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec("INSERT INTO q_case_party (case_id, kind, account_id, role, disposition, updated_at) VALUES (1, 'account', 21, 'defendant', 'bail', ?)", now).Error; err != nil {
		t.Fatal(err)
	}

	second, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if got := second.AccountState(21).State; got != "active" {
		t.Fatalf("legacy bail must release the account, got %s", got)
	}
	if got := second.AccountState(22).State; got != "sentenced" {
		t.Fatalf("legacy final must remain non-schedulable as sentenced, got %s", got)
	}
	var disposition string
	if err := second.DB().Table("q_case_party").Where("account_id = ?", 21).Pluck("disposition", &disposition).Error; err != nil {
		t.Fatal(err)
	}
	if disposition != "released" {
		t.Fatalf("legacy bail party must be released, got %s", disposition)
	}
}
