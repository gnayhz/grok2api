package relational

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestAccountDeletionCrashChild(t *testing.T) {
	location := os.Getenv("G20_ACCOUNT_CRASH_DATABASE")
	if location == "" {
		t.Skip("subprocess-only crash fixture")
	}
	ctx := context.Background()
	var db *Database
	var err error
	if os.Getenv("G20_ACCOUNT_CRASH_DIALECT") == "postgres" {
		db, err = OpenPostgres(ctx, location, 2, 2)
	} else {
		db, err = OpenSQLite(ctx, location)
	}
	if err != nil {
		t.Fatal("open isolated child database failed")
	}
	defer db.Close()
	id, err := strconv.ParseUint(os.Getenv("G20_ACCOUNT_CRASH_ID"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.db.Callback().Create().After("gorm:create").Register("g20_after_tombstone", func(tx *gorm.DB) {
		if tx.Statement.Table == "account_tombstones" && tx.Error == nil {
			fmt.Fprintln(os.Stdout, "G20_DELETE_TX_READY")
			<-time.After(time.Minute)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := NewAccountRepository(db).Delete(ctx, id); err != nil {
		t.Fatal("child deletion failed before intentional termination")
	}
	t.Fatal("child deletion was not terminated")
}

func TestAccountDeletionProcessDeathRollsBackIntentAndAccount(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			r := NewAccountRepository(a)
			v := importFixture(t, r, account.ProviderWeb, "crash", "crash@example.test")
			location := ""
			if dialect == "postgres" {
				location = a.db.Dialector.(*postgres.Dialector).Config.DSN
			} else {
				var rows []struct{ Name, File string }
				if err := a.db.Raw("PRAGMA database_list").Scan(&rows).Error; err != nil {
					t.Fatal(err)
				}
				for _, row := range rows {
					if row.Name == "main" {
						location = row.File
					}
				}
			}
			if location == "" {
				t.Fatal("isolated database location missing")
			}
			child := exec.Command(os.Args[0], "-test.run=^TestAccountDeletionCrashChild$", "-test.v")
			for _, env := range os.Environ() {
				if !strings.HasPrefix(env, "TEST_POSTGRES_") && !strings.HasPrefix(env, "G20_ACCOUNT_CRASH_") {
					child.Env = append(child.Env, env)
				}
			}
			child.Env = append(child.Env, "G20_ACCOUNT_CRASH_DATABASE="+location, "G20_ACCOUNT_CRASH_DIALECT="+dialect, fmt.Sprintf("G20_ACCOUNT_CRASH_ID=%d", v.ID))
			stdout, err := child.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := child.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			defer func() {
				if !waited {
					_ = child.Process.Kill()
					_ = child.Wait()
				}
			}()
			ready := make(chan bool, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					if scanner.Text() == "G20_DELETE_TX_READY" {
						ready <- true
						return
					}
				}
				ready <- false
			}()
			select {
			case reached := <-ready:
				if !reached {
					t.Fatal("child exited before tombstone insert")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("child did not reach deletion transaction")
			}
			if err := child.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			err = child.Wait()
			waited = true
			if err == nil {
				t.Fatal("child was not terminated")
			}
			other := NewAccountRepository(b)
			ctx := context.Background()
			if _, err := other.Get(ctx, v.ID); err != nil {
				t.Fatalf("uncommitted deletion survived process death: %v", err)
			}
			marks, err := other.TombstonedEmails(ctx, []string{v.Email})
			if err != nil || len(marks) != 0 {
				t.Fatalf("uncommitted intent survived process death: %v", err)
			}
			if err := other.Delete(ctx, v.ID); err != nil {
				t.Fatalf("process death retained transaction lock: %v", err)
			}
		})
	}
}
