package registry

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The persisted queue schema before leases and transactional projections.
type legacyEvolutionProbeTask struct {
	AttemptJSON        string `gorm:"type:text;not null;default:''"`
	ControlAttemptJSON string `gorm:"type:text;not null;default:''"`
	ID                 uint64 `gorm:"primaryKey;autoIncrement"`
	CaseID             uint64 `gorm:"not null;default:0;index:idx_q_probe_task_case"`
	Direction          string `gorm:"size:32;not null;check:chk_q_probe_task_direction,direction IN ('account_differential','exit_jury')"`
	DefendantAccountID uint64 `gorm:"not null;default:0"`
	DefendantNodeID    uint64 `gorm:"not null;default:0"`
	DefendantEpoch     uint64 `gorm:"not null;default:0"`
	BaselineNodeID     uint64 `gorm:"not null;default:0"`
	BaselineEpoch      uint64 `gorm:"not null;default:0"`
	JurorAccountID     uint64 `gorm:"not null;default:0"`
	ControlAccountID   uint64 `gorm:"not null;default:0"`
	ControlNodeID      uint64 `gorm:"not null;default:0"`
	ControlEpoch       uint64 `gorm:"not null;default:0"`
	FailureKind        string `gorm:"size:48;not null;default:''"`
	PathKey            string `gorm:"size:64;not null;default:''"`
	ControlOutcome     string `gorm:"size:16;not null;default:''"`
	ControlDetail      string `gorm:"size:512;not null;default:''"`
	ControlPathKey     string `gorm:"size:64;not null;default:''"`
	ControlVerified    bool   `gorm:"not null;default:false"`
	State              string `gorm:"size:16;not null;default:pending;index:idx_q_probe_task_state_updated,priority:1;check:chk_q_probe_task_state,state IN ('pending','running','done','failed','cancelled')"`
	Result             string `gorm:"size:16;not null;default:'';check:chk_q_probe_task_result,result IN ('','clean','degraded','error')"`
	// VerifiedIPChange is persisted with the result so a later court pass does
	// not have to trust an in-memory executor flag. Unverified differential
	// degradation can never become an account vote after a restart.
	VerifiedIPChange bool      `gorm:"not null;default:false"`
	Detail           string    `gorm:"size:512;not null;default:'';check:chk_q_probe_task_detail,length(detail) <= 512"`
	CreatedAt        time.Time `gorm:"not null"`
	UpdatedAt        time.Time `gorm:"not null;index:idx_q_probe_task_state_updated,priority:2"`
	FinishedAt       *time.Time
}

func (legacyEvolutionProbeTask) TableName() string { return "q_probe_task" }

func TestUpgradeAddsLeasesAndRepairsExistingQueue(t *testing.T) {
	ctx, now := context.Background(), time.Now().UTC()
	path := filepath.Join(t.TempDir(), "pre-evolution.db")
	db, err := gorm.Open(sqlite.Open(path), qualityGormConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&legacyEvolutionProbeTask{}); err != nil {
		t.Fatal(err)
	}
	for _, row := range []legacyEvolutionProbeTask{
		{ID: 1, Direction: "exit_jury", JurorAccountID: 7, State: "running", CreatedAt: now, UpdatedAt: now},
		{ID: 2, Direction: "exit_jury", JurorAccountID: 8, State: "done", Result: "clean", CreatedAt: now, UpdatedAt: now, FinishedAt: &now},
	} {
		if err := db.Create(&row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("CREATE TABLE q_guard_capacity (id integer PRIMARY KEY, pending integer NOT NULL, capacity_limit integer NOT NULL)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO q_guard_capacity VALUES (1, 0, 10000)").Error; err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if n, err := r.ReclaimRunningProbes(ctx, "legacy owner missing"); err != nil || n != 1 {
		t.Fatalf("reclaim=%d err=%v", n, err)
	}
	store := NewProbeTaskStore(r)
	archive, err := evidence.New(ctx, r.DB(), model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := store.ProcessProbeProjections(ctx, 100, archive.Record); err != nil {
			t.Fatal(err)
		}
	}
	if count, err := archive.Count(ctx); err != nil || count != 1 {
		t.Fatalf("projection count=%d err=%v", count, err)
	}
	tasks, err := store.ListProbeTasksForCase(ctx, 0)
	if err != nil || len(tasks) != 2 || tasks[1].State != model.ProbeDone || tasks[0].State != model.ProbeCancelled {
		t.Fatalf("tasks=%+v err=%v", tasks, err)
	}
	if tasks[0].Experiment.Version != "" || tasks[0].Attempt.ID != "" {
		t.Fatal("upgrade fabricated legacy experiment identity")
	}
	stats, err := journal.New(r.DB()).Stats(ctx)
	if err != nil || stats.InFlight != 0 || stats.Limit != 10000 {
		t.Fatalf("capacity=%+v err=%v", stats, err)
	}
}
