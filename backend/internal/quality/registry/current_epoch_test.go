package registry

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"gorm.io/gorm"
)

func TestCurrentStateCostDoesNotFollowEpochHistory(t *testing.T) {
	sizes := []int{10000, 100000}
	if n, _ := strconv.Atoi(os.Getenv("GROK_MATURITY_HISTORY_ROWS")); n > 100000 {
		sizes = append(sizes, n)
	}
	for _, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			r := newReconcileRegistry(t)
			ctx, now := context.Background(), time.Now().UTC()
			if err := r.DB().Transaction(func(tx *gorm.DB) error {
				for start := 0; start < size; start += 1000 {
					rows := make([]qIPEpochModel, 0, 1000)
					for i := start; i < min(start+1000, size); i++ {
						rows = append(rows, qIPEpochModel{NodeID: 1, Epoch: uint64(i), CurrentIP: "192.0.2.1", FirstSeenAt: now, ChangedAt: now})
					}
					if err := tx.Create(&rows).Error; err != nil {
						return err
					}
				}
				return tx.Model(&qStateRevisionModel{}).Where("id = 1").Update("epochs_initialized", false).Error
			}); err != nil {
				t.Fatal(err)
			}
			if err := r.migrateCurrentEpochs(ctx); err != nil {
				t.Fatal(err)
			}
			var historical, current, state atomic.Int64
			if err := r.DB().Callback().Query().After("gorm:query").Register("maturity:state_reads", func(db *gorm.DB) {
				switch db.Statement.Table {
				case "q_ip_epoch":
					historical.Add(db.RowsAffected)
				case "q_node_epoch":
					current.Add(db.RowsAffected)
				case "q_account_state", "q_exit_state", "q_identity_group":
					state.Add(1)
				}
			}); err != nil {
				t.Fatal(err)
			}
			defer r.DB().Callback().Query().Remove("maturity:state_reads")
			start := time.Now()
			if err := r.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			if r.CurrentEpoch(1) != uint64(size-1) || historical.Load() != 0 || current.Load() != 1 {
				t.Fatalf("epoch=%d history=%d current=%d", r.CurrentEpoch(1), historical.Load(), current.Load())
			}
			t.Logf("history=%d nodes=1 refresh_history_rows=0 current_rows=1 duration=%v", size, time.Since(start))
			current.Store(0)
			state.Store(0)
			if err := r.RefreshState(ctx); err != nil {
				t.Fatal(err)
			}
			if current.Load() != 0 || state.Load() != 0 {
				t.Fatal("unchanged revision reloaded state")
			}
			start = time.Now()
			if _, err := NewProbeTaskStore(r).CreateProbeTask(ctx, model.ProbeTask{Direction: model.ProbeExitJury, JurorAccountID: 7}); err != nil {
				t.Fatal(err)
			}
			if historical.Load() != 0 || current.Load() != 0 || state.Load() != 0 {
				t.Fatal("queue insert rebuilt state/history")
			}
			t.Logf("history=%d task_insert_history_rows=0 state_queries=0 duration=%v", size, time.Since(start))
			// A repeated upgrade does not replace an already initialized pointer.
			if err := r.migrateCurrentEpochs(ctx); err != nil {
				t.Fatal(err)
			}
			if historical.Load() != 0 {
				t.Fatal("upgrade rescanned archive")
			}
		})
	}
}

func TestEpochPointerAndArchiveRollbackTogether(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	if err := r.RecordExitIP(ctx, 1, "192.0.2.1"); err != nil {
		t.Fatal(err)
	}
	if err := r.DB().Exec("CREATE TRIGGER fail_epoch_pointer BEFORE INSERT ON q_node_epoch BEGIN SELECT RAISE(ABORT, 'injected pointer failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.AdvanceEpoch(ctx, 1, "192.0.2.2"); err == nil {
		t.Fatal("failure ignored")
	}
	archive, err := r.ExitIPArchive(ctx, 1)
	if err != nil || len(archive) != 1 || r.CurrentEpoch(1) != 0 {
		t.Fatalf("partial epoch: %+v %v", archive, err)
	}
	if err := r.RecordExitIP(ctx, 2, "192.0.2.3"); err == nil {
		t.Fatal("initial pointer failure ignored")
	}
	archive, err = r.ExitIPArchive(ctx, 2)
	if err != nil || len(archive) != 0 {
		t.Fatalf("partial initial archive: %+v %v", archive, err)
	}
}

func TestProbeTaskCannotBeInsertedAfterSettlement(t *testing.T) {
	r := newReconcileRegistry(t)
	ctx := context.Background()
	id, err := r.OpenInvestigation(ctx, 7, model.EpochKey{}, time.Now(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictInsufficient, `{}`, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := NewProbeTaskStore(r).CreateProbeTask(ctx, model.ProbeTask{CaseID: id, Direction: model.ProbeExitJury}); err == nil {
		t.Fatal("closed case acquired new work")
	}
}
