package registry

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func isolatedQualityPostgres(t *testing.T) (Options, *gorm.DB) {
	t.Helper()
	dsn := os.Getenv("GROK_EVOLUTION_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated GROK_EVOLUTION_POSTGRES_DSN")
	}
	admin, err := gorm.Open(postgres.Open(dsn), qualityGormConfig())
	if err != nil {
		t.Fatal(err)
	}
	adminSQL, err := admin.DB()
	if err != nil {
		t.Fatal(err)
	}
	schema := fmt.Sprintf("maturity_start_%d", time.Now().UnixNano())
	if err := admin.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		adminSQL.Close()
		t.Fatal(err)
	}
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	opts := Options{Driver: "postgres", PostgresDSN: dsn + separator + "search_path=" + schema}
	raw, err := gorm.Open(postgres.Open(opts.PostgresDSN), qualityGormConfig())
	if err != nil {
		admin.Exec("DROP SCHEMA " + schema + " CASCADE")
		adminSQL.Close()
		t.Fatal(err)
	}
	rawSQL, err := raw.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rawSQL.Close(); admin.Exec("DROP SCHEMA " + schema + " CASCADE"); adminSQL.Close() })
	return opts, raw
}

func TestPostgresConcurrentQualityStartup(t *testing.T) {
	for _, upgrade := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "upgrade"}[upgrade], func(t *testing.T) {
			opts, raw := isolatedQualityPostgres(t)
			if upgrade {
				if err := raw.AutoMigrate(&qIPEpochModel{}); err != nil {
					t.Fatal(err)
				}
				if err := raw.Exec("CREATE TABLE q_state_revision (id smallint PRIMARY KEY, revision bigint NOT NULL DEFAULT 0)").Error; err != nil {
					t.Fatal(err)
				}
				if err := raw.Exec("INSERT INTO q_state_revision (id,revision) VALUES (1,9)").Error; err != nil {
					t.Fatal(err)
				}
				now := time.Now().UTC()
				if err := raw.Create(&qIPEpochModel{NodeID: 7, Epoch: 3, CurrentIP: "192.0.2.3", FirstSeenAt: now, ChangedAt: now}).Error; err != nil {
					t.Fatal(err)
				}
			}
			start := make(chan struct{})
			errs := make(chan error, 2)
			var wg sync.WaitGroup
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					r, err := Open(context.Background(), opts)
					if err == nil {
						if upgrade && r.CurrentEpoch(7) != 3 {
							err = fmt.Errorf("lost historical epoch: %d", r.CurrentEpoch(7))
						}
						if closeErr := r.Close(); err == nil {
							err = closeErr
						}
					}
					errs <- err
				}()
			}
			close(start)
			wg.Wait()
			close(errs)
			for err := range errs {
				if err != nil {
					t.Errorf("concurrent startup: %v", err)
				}
			}
			var revision qStateRevisionModel
			if err := raw.First(&revision).Error; err != nil || !revision.EpochsInitialized || !revision.IncidentsInitialized {
				t.Fatalf("initialization incomplete: %+v %v", revision, err)
			}
		})
	}
}

func TestPostgresQualityUpgradeRollsBackAndRetries(t *testing.T) {
	opts, raw := isolatedQualityPostgres(t)
	if err := raw.AutoMigrate(&qIPEpochModel{}, &qNodeEpochModel{}, &qStateRevisionModel{}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := raw.Create(&qIPEpochModel{NodeID: 7, Epoch: 3, CurrentIP: "192.0.2.3", FirstSeenAt: now, ChangedAt: now}).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Create(&qStateRevisionModel{ID: 1, Revision: 9}).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec(`CREATE FUNCTION reject_pointer() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected pointer migration failure'; END $$`).Error; err != nil {
		t.Fatal(err)
	}
	if err := raw.Exec(`CREATE TRIGGER reject_pointer BEFORE INSERT ON q_node_epoch FOR EACH ROW EXECUTE FUNCTION reject_pointer()`).Error; err != nil {
		t.Fatal(err)
	}
	if r, err := Open(context.Background(), opts); err == nil {
		r.Close()
		t.Fatal("injected migration failure ignored")
	}
	var revision qStateRevisionModel
	if err := raw.First(&revision).Error; err != nil || revision.EpochsInitialized || revision.Revision != 9 {
		t.Fatalf("failed upgrade marked complete: %+v %v", revision, err)
	}
	if raw.Migrator().HasTable("q_case") {
		t.Fatal("failed upgrade left partially created schema")
	}
	if err := raw.Exec("DROP TRIGGER reject_pointer ON q_node_epoch").Error; err != nil {
		t.Fatal(err)
	}
	r, err := Open(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.CurrentEpoch(7) != 3 {
		t.Fatal("retry did not recover legacy epoch")
	}
}
