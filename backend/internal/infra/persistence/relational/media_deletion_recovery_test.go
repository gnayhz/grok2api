package relational

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

type mediaDeletionMarkerFailure struct {
	repository.MediaJobRepository
	fail atomic.Bool
}

func (r *mediaDeletionMarkerFailure) MarkMediaJobUsageRecorded(ctx context.Context, id string, at time.Time) error {
	if r.fail.Load() {
		return errors.New("injected usage handoff marker failure")
	}
	return r.MediaJobRepository.MarkMediaJobUsageRecorded(ctx, id, at)
}

func TestMediaDeletionRetainsSourceThroughJournalAndSQLFailures(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		for _, fault := range []string{"journal_unavailable", "sql_unavailable", "marker_unavailable"} {
			t.Run(dialect+"/"+fault, func(t *testing.T) {
				db, peer := settingsDatabasePair(t, dialect)
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				key, job := seedMediaDeletion(t, db, "journal", media.StatusCompleted)
				job.UsageRecordedAt = nil
				jobs := &mediaDeletionMarkerFailure{MediaJobRepository: NewMediaJobRepository(db)}
				if err := jobs.CreateMediaJob(ctx, job); err != nil {
					t.Fatal(err)
				}
				keys := clientkeyapp.NewService("deletion", NewClientKeyRepository(db), nil, nil, 0, 0, nil)
				defer keys.Close(context.Background())
				if ok, err := NewClientKeyRepository(db).ReserveBillingUsage(ctx, key.ID, "video_usage_"+job.ID, 3000000000, time.Now().UTC().Add(-time.Hour), repository.BillingReservationScope{OwnerID: "deletion"}); err != nil || !ok {
					t.Fatalf("reserve: %v %v", ok, err)
				}
				path := filepath.Join(t.TempDir(), "pending.db")
				journal, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				defer journal.Close()
				writer := auditapp.NewService(NewAuditRepository(db), journal, nil, 1, time.Millisecond)
				writer.SetBillingObserver(keys)
				if err := writer.Start(ctx); err != nil {
					t.Fatal(err)
				}
				defer writer.Close(context.Background())
				removeFault := func() {}
				switch fault {
				case "journal_unavailable":
					if err := journal.database.db.Callback().Update().Before("gorm:update").Register("media_journal_failure", func(tx *gorm.DB) {
						tx.AddError(errors.New("injected journal unavailable"))
					}); err != nil {
						t.Fatal(err)
					}
					removeFault = func() {
						if err := journal.database.db.Callback().Update().Remove("media_journal_failure"); err != nil {
							t.Fatal(err)
						}
					}
				case "sql_unavailable":
					removeFault = installAuditSettlementFailure(t, db)
				case "marker_unavailable":
					jobs.fail.Store(true)
				}
				recovery := gateway.NewService(nil, writer, nil, keys, nil, nil, nil, 1)
				recovery.ConfigureMedia(jobs, 1)
				failCtx, failCancel := context.WithTimeout(ctx, 150*time.Millisecond)
				err = recovery.RecoverVideoJobs(failCtx)
				failCancel()
				if err == nil {
					t.Fatal("injected completion failure was hidden")
				}
				if n, err := keys.BatchDelete(ctx, []uint64{key.ID}); n != 0 || !errors.Is(err, clientkeyapp.ErrConflict) {
					t.Fatalf("incomplete handoff deleted source/key: %d %v", n, err)
				}
				stored, err := NewMediaJobRepository(peer).GetMediaJob(ctx, job.ID, key.ID)
				if err != nil || stored.UsageRecordedAt != nil {
					t.Fatalf("failed handoff removed/marked source: %v %v", stored.UsageRecordedAt, err)
				}
				if n, err := keys.CleanupExpiredBilling(ctx, 10); n != 0 || err != nil {
					t.Fatalf("recovery source lost expired reservation protection: %d %v", n, err)
				}
				if fault == "sql_unavailable" && journal.Snapshot().Records != 1 {
					t.Fatalf("SQL failure discarded pending fact: %+v", journal.Snapshot())
				}
				if fault == "marker_unavailable" && tableRowCount(t, peer, "billing_settlements") != 1 {
					t.Fatal("marker failure fixture must follow an actual SQL settlement")
				}
				removeFault()
				jobs.fail.Store(false)
				for range 2 {
					if err := recovery.RecoverVideoJobs(ctx); err != nil {
						t.Fatal(err)
					}
				}
				waitAuditRecovery(t, func() bool { return journal.Snapshot().Records == 0 })
				assertSettlementKey(t, peer, key.ID, 2100000000, 0)
				if tableRowCount(t, peer, "billing_settlements") != 1 || tableRowCount(t, peer, "request_audits") != 1 {
					t.Fatal("repeated completion duplicated/lost the ledger identity")
				}
				if err := keys.Delete(ctx, key.ID); err != nil {
					t.Fatalf("completed source not deletable: %v", err)
				}
				if tableRowCount(t, peer, "billing_settlements") != 1 || tableRowCount(t, peer, "request_audits") != 1 {
					t.Fatal("identity deletion removed the independent ledger")
				}
			})
		}
	}
}
