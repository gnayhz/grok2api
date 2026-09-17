package relational

import (
	"context"
	"errors"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"path/filepath"
	"strings"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// A real database trigger keeps rejecting every settlement until explicitly
// removed. Closing the writer must not depend on eventual database recovery.
func TestAuditWriterRecoversAfterPersistentSQLFailureAndRestart(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := settlementTestKey(t, a, "recovery")
			value := settlementRecord(key.ID, "recovery", 30)
			keys := NewClientKeyRepository(a)
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().UTC().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			removeFault := installAuditSettlementFailure(t, a)
			path := filepath.Join(t.TempDir(), "pending.db")
			open := func() *AuditJournal {
				t.Helper()
				j, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = j.Close() })
				return j
			}
			journal := open()
			writer := auditapp.NewService(NewAuditRepository(a), journal, nil, 4, time.Millisecond)
			if err := writer.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close(context.Background()) })
			writeCtx, writeCancel := context.WithTimeout(ctx, 100*time.Millisecond)
			err := writer.Create(writeCtx, value)
			writeCancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed SQL acknowledged: %v", err)
			}
			if state := writer.LedgerSnapshot(); state.ConsecutiveFailures == 0 || state.QueueDepth != 1 {
				t.Fatalf("pending state=%+v", state)
			}
			stopCtx, stopCancel := context.WithTimeout(ctx, 500*time.Millisecond)
			err = writer.Close(stopCtx)
			stopCancel()
			if err != nil {
				t.Fatalf("persistent failure prevented shutdown: %v", err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, b, key.ID, 0, 80)
			if tableRowCount(t, b, "request_audits") != 0 {
				t.Fatal("failed SQL leaked an audit")
			}
			// Expiry while the process is absent must not make its recovered pending
			// event eligible for this instance's periodic reservation cleanup.
			if err := b.db.Model(&billingReservationModel{}).Where("event_id = ?", value.EventID).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
				t.Fatal(err)
			}
			recoveredJournal := open()
			recoveredKeys := keyapp.NewService("test-owner", NewClientKeyRepository(b), nil, nil, 120, 8, nil, security.RandomTokenSource{})
			recovered := auditapp.NewService(NewAuditRepository(b), recoveredJournal, nil, 4, time.Millisecond)
			recovered.SetBillingObserver(recoveredKeys)
			if err := recovered.Start(ctx); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = recovered.Close(context.Background()) })
			if n, err := recoveredKeys.CleanupExpiredBilling(ctx, 100); err != nil || n != 0 {
				t.Fatalf("recovered reservation was cleaned: %d %v", n, err)
			}
			removeFault()
			// Replaying the same event with a different estimate cannot replace the
			// first durably accepted payload that is now being recovered.
			replay := value
			replay.EstimatedCostInUSDTicks = 999
			if err := recovered.Create(ctx, replay); err != nil {
				t.Fatal(err)
			}
			waitAuditRecovery(t, func() bool { return recoveredJournal.Snapshot().Records == 0 })
			assertSettlementKey(t, a, key.ID, 30, 0)
			if tableRowCount(t, a, "billing_settlements") != 1 || tableRowCount(t, a, "request_audit_attempts") != 1 {
				t.Fatal("recovery duplicated settlement or details")
			}
			if n, err := recoveredKeys.CleanupExpiredBilling(ctx, 100); err != nil || n != 0 {
				t.Fatalf("cleanup after recovery=%d %v", n, err)
			}
		})
	}
}

func installAuditSettlementFailure(t *testing.T, db *Database) func() {
	t.Helper()
	if db.Dialect() == "postgres" {
		if err := db.db.Exec(`CREATE FUNCTION fail_audit_settlement() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'persistent settlement database failure'; END; $$`).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.db.Exec(`CREATE TRIGGER fail_audit_settlement BEFORE INSERT ON billing_settlements FOR EACH ROW EXECUTE FUNCTION fail_audit_settlement()`).Error; err != nil {
			t.Fatal(err)
		}
		return func() {
			t.Helper()
			if err := db.db.Exec(`DROP TRIGGER fail_audit_settlement ON billing_settlements`).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := db.db.Exec(`CREATE TRIGGER fail_audit_settlement BEFORE INSERT ON billing_settlements BEGIN SELECT RAISE(ABORT, 'persistent settlement database failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	return func() {
		t.Helper()
		if err := db.db.Exec(`DROP TRIGGER fail_audit_settlement`).Error; err != nil {
			t.Fatal(err)
		}
	}
}

func waitAuditRecovery(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().UTC().Add(5 * time.Second)
	for !ready() {
		if time.Now().UTC().After(deadline) {
			t.Fatal("audit recovery did not converge")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAuditWriterJournalAcknowledgementFailureReplaysAfterRetention(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := settlementTestKey(t, a, "journal-ack")
			value := settlementRecord(key.ID, "journal-ack", 30)
			path := filepath.Join(t.TempDir(), "pending.db")
			journal, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			if err := journal.database.db.Exec(`CREATE TRIGGER fail_pending_ack BEFORE DELETE ON audit_pending BEGIN SELECT RAISE(ABORT, 'pending deletion failed'); END`).Error; err != nil {
				t.Fatal(err)
			}
			writer := auditapp.NewService(NewAuditRepository(a), journal, nil, 4, time.Millisecond)
			if err := writer.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer writer.Close(context.Background())
			// SQL success is acknowledged even when removing the redundant local copy
			// fails. The retained copy is safe to replay and must remain observable.
			if err := writer.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if journal.Snapshot().Records != 1 {
				t.Fatal("failed acknowledgement discarded recovery fact")
			}
			assertSettlementKey(t, b, key.ID, 30, 0)
			if n, err := NewAuditRepository(b).DeleteOlderThan(ctx, time.Now().UTC().Add(time.Hour), 10); err != nil || n != 1 {
				t.Fatalf("retention=%d %v", n, err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			journal2, err := OpenAuditJournal(ctx, path, AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20})
			if err != nil {
				t.Fatal(err)
			}
			defer journal2.Close()
			if err := journal2.database.db.Exec(`DROP TRIGGER fail_pending_ack`).Error; err != nil {
				t.Fatal(err)
			}
			writer2 := auditapp.NewService(NewAuditRepository(b), journal2, nil, 4, time.Millisecond)
			if err := writer2.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer writer2.Close(context.Background())
			waitAuditRecovery(t, func() bool { return journal2.Snapshot().Records == 0 })
			assertSettlementKey(t, a, key.ID, 30, 0)
			if tableRowCount(t, a, "request_audits") != 0 || tableRowCount(t, a, "billing_settlements") != 1 {
				t.Fatal("journal replay resurrected expired audit or duplicated billing")
			}
			if _, err := NewClientKeyRepository(a).ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().UTC().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil && !errors.Is(err, repository.ErrConflict) {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 30, 0)
		})
	}
}

func TestAuditPendingReservationSurvivesCapacityPressure(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "pending-pressure")
			if err := a.db.Model(&clientKeyModel{}).Where("id = ?", key.ID).Update("billing_limit_usd_ticks", 100).Error; err != nil {
				t.Fatal(err)
			}
			keys := NewClientKeyRepository(a)
			eventID := "evt_pending_pressure_first"
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 80, time.Now().UTC().Add(-time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			journal := newTestAuditJournal(t, 8)
			value := settlementRecord(key.ID, "pending-pressure", 30)
			value.EventID = eventID
			if _, err := journal.Append(ctx, value); err != nil {
				t.Fatal(err)
			}
			installAuditSettlementFailure(t, a)
			clientKeys := keyapp.NewService("test-owner", NewClientKeyRepository(b), nil, nil, 120, 8, nil, security.RandomTokenSource{})
			writer := auditapp.NewService(NewAuditRepository(b), journal, nil, 4, time.Millisecond)
			writer.UpdateWriterConfig(4, time.Millisecond, time.Hour)
			writer.SetBillingObserver(clientKeys)
			if err := writer.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer writer.Close(ctx)
			keyValue, err := NewClientKeyRepository(b).Get(ctx, key.ID)
			if err != nil {
				t.Fatal(err)
			}
			if ok, err := clientKeys.ReserveBilling(ctx, keyValue, "evt_pending_pressure_next", 40, time.Hour); ok || !errors.Is(err, keyapp.ErrBillingLimit) {
				t.Fatalf("capacity pressure removed pending protection: reserved=%v err=%v", ok, err)
			}
			assertSettlementKey(t, a, key.ID, 0, 80)
		})
	}
}

func TestMediaReservationSurvivesCapacityPressure(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "media-pressure")
			if err := a.db.Model(&clientKeyModel{}).Where("id = ?", key.ID).Update("billing_limit_usd_ticks", 100).Error; err != nil {
				t.Fatal(err)
			}
			keys := NewClientKeyRepository(a)
			now := time.Now().UTC()
			job := mediaJobModel{ID: "video_pending_pressure", RequestID: "media-pressure", ClientKeyID: key.ID, Provider: "grok_web", Model: "video", ModelRouteID: 1, UpstreamModel: "video", Prompt: "test", Seconds: 6, Size: "16:9", Quality: "720p", Status: "completed", InputJSON: "{}", CreatedAt: now, UpdatedAt: now}
			eventID := "video_usage_" + job.ID
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, eventID, 80, now.Add(-time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			if err := a.db.Create(&job).Error; err != nil {
				t.Fatal(err)
			}
			// Another instance observes the durable media marker without a process-local
			// activity list; capacity cleanup must obey it just like periodic cleanup.
			other := NewClientKeyRepository(b)
			if ok, err := other.ReserveBillingUsage(ctx, key.ID, "evt_media_pressure_next", 40, now.Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); ok || !errors.Is(err, repository.ErrLimitExceeded) {
				t.Fatalf("media protection lost under pressure: %v %v", ok, err)
			}
			if ok, err := other.ReserveBillingUsage(ctx, key.ID, eventID, 80, now.Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); !ok || err != nil {
				t.Fatalf("same-event retry changed protected reservation: %v %v", ok, err)
			}
			if _, err := other.ReserveBillingUsage(ctx, key.ID, eventID, 90, now.Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("same-event retry changed protected amount: %v", err)
			}
			assertSettlementKey(t, a, key.ID, 0, 80)
		})
	}
}

func TestAuditWriterRetainsOversizedIdentityWithoutAliasing(t *testing.T) {
	a, _ := settingsDatabasePair(t, "sqlite")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	key := settlementTestKey(t, a, "identity-width")
	first := settlementRecord(key.ID, "identity-width", 30)
	first.EventID = strings.Repeat("x", 64)
	if err := NewAuditRepository(a).Create(ctx, first); err != nil {
		t.Fatal(err)
	}
	journal := newTestAuditJournal(t, 8)
	writer := auditapp.NewService(NewAuditRepository(a), journal, nil, 4, time.Millisecond)
	if err := writer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer writer.Close(context.Background())
	invalid := first
	invalid.EventID += "different"
	invalid.EstimatedCostInUSDTicks = 40
	if err := writer.Create(ctx, invalid); !errors.Is(err, repository.ErrInvalidRecord) {
		t.Fatalf("oversized identity aliased existing event: %v", err)
	}
	state := journal.Snapshot()
	if state.Records != 1 || state.Rejected != 1 {
		t.Fatalf("invalid identity was discarded: %+v", state)
	}
	assertSettlementKey(t, a, key.ID, 30, 0)
	if tableRowCount(t, a, "billing_settlements") != 1 {
		t.Fatal("invalid identity changed settlement identity")
	}
}
