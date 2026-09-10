package relational

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestAuditJournalPersistenceCapacityAndOwnership(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "持久 ?#%", "pending.db")
	options := AuditJournalOptions{MaxRecords: 2, MaxBytes: 64 << 10}
	j, err := OpenAuditJournal(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	if duplicate, err := OpenAuditJournal(ctx, path, options); !errors.Is(err, repository.ErrAuditPendingOwner) || duplicate != nil {
		t.Fatalf("second writer obtained the same file: %v", err)
	}
	value := settlementRecord(1, "journal", 30)
	first, err := j.Append(ctx, value)
	if err != nil || first.ID == 0 {
		t.Fatalf("append=%+v %v", first, err)
	}
	value.EstimatedCostInUSDTicks = 999
	if repeated, err := j.Append(ctx, value); err != nil || repeated.ID != first.ID || repeated.Record.EstimatedCostInUSDTicks != 30 {
		t.Fatalf("first pending payload changed: %+v %v", repeated, err)
	}
	value.ClientKeyID = 2
	if _, err := j.Append(ctx, value); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("pending event changed owner: %v", err)
	}
	second, err := j.Append(ctx, settlementRecord(1, "journal_two", 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(ctx, settlementRecord(1, "journal_three", 5)); !errors.Is(err, repository.ErrAuditPendingFull) {
		t.Fatalf("record capacity not enforced: %v", err)
	}
	if err := j.Reject(ctx, first.ID, "invalid_record"); err != nil {
		t.Fatal(err)
	}
	if snapshot := j.Snapshot(); snapshot.Records != 2 || snapshot.Rejected != 1 || snapshot.Bytes <= 0 {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenAuditJournal(ctx, path, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	entries, err := reopened.ReadPending(ctx, 0, 10)
	if err != nil || len(entries) != 1 || entries[0].ID != second.ID {
		t.Fatalf("pending after reopen=%+v %v", entries, err)
	}
	protected, err := reopened.PendingEventIDs(ctx, 0, 10)
	if err != nil || len(protected) != 2 {
		t.Fatalf("rejected identity lost protection=%+v %v", protected, err)
	}
	if err := reopened.RetryRejected(ctx); err != nil {
		t.Fatal(err)
	}
	entries, err = reopened.ReadPending(ctx, 0, 10)
	if err != nil || len(entries) != 2 || entries[0].Record.EstimatedCostInUSDTicks != 30 {
		t.Fatalf("retained facts changed: %+v %v", entries, err)
	}
	// Acknowledgements can repeat after a lost local commit response.
	for range 2 {
		if err := reopened.Acknowledge(ctx, []uint64{first.ID, first.ID, second.ID}); err != nil {
			t.Fatal(err)
		}
	}
	if snapshot := reopened.Snapshot(); snapshot.Records != 0 || snapshot.Bytes != 0 || snapshot.Rejected != 0 {
		t.Fatalf("ack did not release exact capacity: %+v", snapshot)
	}
	for _, file := range []string{path, path + ".lock"} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("file permissions=%v %v", info, err)
		}
	}
	// Each pooled connection must use the durable setting, including newly
	// created connections rather than a PRAGMA applied to only one session.
	sqlDB, err := reopened.database.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	for range 4 {
		conn, err := sqlDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var mode int
		if err := conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&mode); err != nil || mode != 2 {
			t.Fatalf("synchronous=%d %v", mode, err)
		}
	}
}

func TestAuditJournalWriteFailureAndMalformedPayloadRetainCapacity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pending.db")
	j, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 4, MaxBytes: 16 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	value := settlementRecord(1, "journal_fault", 30)
	const callback = "audit_journal_meta_fault"
	if err := j.database.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "audit_pending_meta" {
			tx.AddError(errors.New("injected journal counter failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	_, appendErr := j.Append(ctx, value)
	if err := j.database.db.Callback().Update().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if appendErr == nil || tableRowCount(t, j.database, "audit_pending") != 0 || j.Snapshot().Records != 0 {
		t.Fatalf("failed append accepted a partial record: %v", appendErr)
	}
	entry, err := j.Append(ctx, value)
	if err != nil {
		t.Fatal(err)
	}
	if err := j.database.db.Callback().Delete().Before("gorm:delete").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "audit_pending" {
			tx.AddError(errors.New("injected journal ack failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	ackErr := j.Acknowledge(ctx, []uint64{entry.ID})
	if err := j.database.db.Callback().Delete().Remove(callback); err != nil {
		t.Fatal(err)
	}
	if ackErr == nil || j.Snapshot().Records != 1 || tableRowCount(t, j.database, "audit_pending") != 1 {
		t.Fatalf("failed acknowledgement lost the record: %v", ackErr)
	}
	large := settlementRecord(1, "journal_large", 5)
	large.RequestPath = strings.Repeat("x", 16<<10)
	if _, err := j.Append(ctx, large); !errors.Is(err, repository.ErrAuditPendingFull) {
		t.Fatalf("byte capacity not enforced: %v", err)
	}
	if err := j.database.db.Model(&auditPendingModel{}).Where("id = ?", entry.ID).UpdateColumn("payload", []byte(`{"version":99}`)).Error; err != nil {
		t.Fatal(err)
	}
	entries, err := j.ReadPending(ctx, 0, 10)
	if err != nil || len(entries) != 1 || !errors.Is(entries[0].DecodeError, repository.ErrAuditPendingFormat) {
		t.Fatalf("unknown payload silently consumed: %+v %v", entries, err)
	}
	if err := j.Reject(ctx, entry.ID, "unsupported_payload"); err != nil {
		t.Fatal(err)
	}
	if j.Snapshot().Records != 1 || j.Snapshot().Rejected != 1 {
		t.Fatal("malformed payload was discarded")
	}
	if err := j.database.db.Model(&auditPendingMetaModel{}).Where("id = 1").UpdateColumn("version", 99).Error; err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if unsupported, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 4, MaxBytes: 16 << 10}); !errors.Is(err, repository.ErrAuditPendingFormat) || unsupported != nil {
		t.Fatalf("unknown journal schema accepted: %v", err)
	}
}

func TestAuditWriterProcessCrashRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "crash.db")
	child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAuditJournalCrashChild$", "-test.v")
	child.Env = append(os.Environ(), "AUDIT_JOURNAL_CRASH_CHILD="+path)
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill() }()
	scanner := bufio.NewScanner(output)
	accepted := false
	for scanner.Scan() {
		if scanner.Text() == "journal_accepted" {
			accepted = true
			break
		}
	}
	if !accepted {
		_ = child.Wait()
		t.Fatalf("child did not durably accept: %v", scanner.Err())
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	j, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 4, MaxBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	entries, err := j.ReadPending(ctx, 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Record.EstimatedCostInUSDTicks != 30 {
		t.Fatalf("process crash lost accepted payload: %+v %v", entries, err)
	}
	database, err := OpenSQLite(ctx, path+".ledger")
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.db.Exec("DROP TRIGGER fail_audit_settlement").Error; err != nil {
		t.Fatal(err)
	}
	writer := auditapp.NewService(NewAuditRepository(database), j, nil, 4, time.Millisecond)
	if err := writer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	waitAuditRecovery(t, func() bool { return j.Snapshot().Records == 0 })
	assertSettlementKey(t, database, 1, 30, 0)

}

func TestAuditJournalCrashChild(t *testing.T) {
	path := os.Getenv("AUDIT_JOURNAL_CRASH_CHILD")
	if path == "" {
		t.Skip("subprocess helper")
	}
	j, err := OpenAuditJournal(context.Background(), path, AuditJournalOptions{MaxRecords: 4, MaxBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	database, err := OpenSQLite(ctx, path+".ledger")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	key := settlementTestKey(t, database, "process-crash")
	value := settlementRecord(key.ID, "process-crash", 30)
	if ok, err := NewClientKeyRepository(database).ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().UTC().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
		t.Fatalf("reserve=%v %v", ok, err)
	}
	installAuditSettlementFailure(t, database)
	writer := auditapp.NewService(NewAuditRepository(database), j, nil, 1, time.Millisecond)
	if err := writer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	go func() { _ = writer.Create(ctx, value) }()
	waitAuditRecovery(t, func() bool { return j.Snapshot().Records == 1 })
	fmt.Println("journal_accepted")
	// The parent kills this process without executing journal.Close.
	<-make(chan struct{})
}

func TestAuditJournalConcurrentCountersAndReplayIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	journal := newTestAuditJournal(t, 128)
	const count = 48
	ids := make([]uint64, count)
	results := make(chan error, count)
	for index := range count {
		go func() {
			value := settlementRecord(1, fmt.Sprintf("journal_parallel_%d", index), 30)
			entry, err := journal.Append(ctx, value)
			if err == nil {
				ids[index] = entry.ID
				value.EstimatedCostInUSDTicks = 999
				repeated, repeatErr := journal.Append(ctx, value)
				if repeatErr != nil {
					err = repeatErr
				} else if repeated.ID != entry.ID || repeated.Record.EstimatedCostInUSDTicks != 30 {
					err = errors.New("concurrent duplicate replaced first payload")
				}
			}
			results <- err
		}()
	}
	for range count {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if state := journal.Snapshot(); state.Records != count || state.Rejected != 0 {
		t.Fatalf("append counters=%+v", state)
	}
	// Delete, park, and append operate on different facts at the same time. A
	// late cache refresh cannot replace a newer committed metadata revision.
	for index := range count {
		go func() {
			var err error
			if index%2 == 0 {
				err = journal.Acknowledge(ctx, []uint64{ids[index], ids[index]})
			} else {
				err = journal.Reject(ctx, ids[index], "retained for repair")
			}
			if err == nil {
				_, err = journal.Append(ctx, settlementRecord(1, fmt.Sprintf("journal_new_%d", index), 40))
			}
			results <- err
		}()
	}
	for range count {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var actual struct {
		Records  int
		Rejected int
		Bytes    int64
	}
	if err := journal.database.db.Raw("SELECT COUNT(*) AS records, COALESCE(SUM(CASE WHEN rejected THEN 1 ELSE 0 END),0) AS rejected, COALESCE(SUM(size),0) AS bytes FROM audit_pending").Scan(&actual).Error; err != nil {
		t.Fatal(err)
	}
	state := journal.Snapshot()
	if actual.Records != count+count/2 || actual.Rejected != count/2 || state.Records != actual.Records || state.Rejected != actual.Rejected || state.Bytes != actual.Bytes {
		t.Fatalf("cached=%+v actual=%+v", state, actual)
	}
	oldRevision := state.Revision
	if err := journal.RetryRejected(ctx); err != nil {
		t.Fatal(err)
	}
	if state := journal.Snapshot(); state.Rejected != 0 || state.Revision <= oldRevision || state.Records != actual.Records {
		t.Fatalf("retry counters=%+v", state)
	}
}
