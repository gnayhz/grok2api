package relational

import (
	"context"
	"errors"
	security "github.com/chenyme/grok2api/backend/internal/infra/security"
	"path/filepath"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	keyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestOfflineAuditOwnerReservationCannotBeExpiredByAnotherInstance(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "offline-owner")
			keyValue, err := NewClientKeyRepository(a).Get(ctx, key.ID)
			if err != nil {
				t.Fatal(err)
			}
			ownerA := keyapp.NewService("owner-a", NewClientKeyRepository(a), nil, nil, 120, 8, nil, security.RandomTokenSource{})
			ownerB := keyapp.NewService("owner-b", NewClientKeyRepository(b), nil, nil, 120, 8, nil, security.RandomTokenSource{})
			value := settlementRecord(key.ID, "offline-owner", 30)
			if ok, err := ownerA.ReserveBilling(ctx, keyValue, value.EventID, 80, time.Hour); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			removeFault := installAuditSettlementFailure(t, a)
			path := filepath.Join(t.TempDir(), "pending.db")
			options := AuditJournalOptions{MaxRecords: 8, MaxBytes: 1 << 20}
			journal, err := OpenAuditJournal(ctx, path, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = journal.Close() })
			writer := auditapp.NewService(NewAuditRepository(a), journal, nil, 1, time.Millisecond)
			writer.SetBillingObserver(ownerA)
			if err := writer.Start(ctx); err != nil {
				t.Fatal(err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
			err = writer.Create(waitCtx, value)
			cancel()
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("failed SQL acknowledged: %v", err)
			}
			if err := writer.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			if err := b.db.Model(&billingReservationModel{}).Where("event_id = ?", value.EventID).Update("expires_at", time.Now().UTC().Add(-time.Hour)).Error; err != nil {
				t.Fatal(err)
			}
			if n, err := ownerB.CleanupExpiredBilling(ctx, 100); err != nil || n != 0 {
				t.Fatalf("other instance expired durable pending owner: cleaned=%d err=%v", n, err)
			}
			assertSettlementKey(t, a, key.ID, 0, 80)
			// The original owner returns with its persistent volume. Restore pending
			// protection before cleanup, while permitting its unrelated expired orphan.
			keys := NewClientKeyRepository(a)
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_owner_a_unused_crash", 10, time.Now().UTC().Add(-time.Hour), repository.BillingReservationScope{OwnerID: "owner-a"}); err != nil || !ok {
				t.Fatalf("orphan reserve=%v %v", ok, err)
			}
			reopened, err := OpenAuditJournal(ctx, path, options)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			restoredOwner := keyapp.NewService("owner-a", keys, nil, nil, 120, 8, nil, security.RandomTokenSource{})
			restoredWriter := auditapp.NewService(NewAuditRepository(a), reopened, nil, 1, time.Millisecond)
			restoredWriter.SetBillingObserver(restoredOwner)
			if err := restoredWriter.Start(ctx); err != nil {
				t.Fatal(err)
			}
			defer restoredWriter.Close(context.Background())
			if n, err := restoredOwner.CleanupExpiredBilling(ctx, 100); err != nil || n != 1 {
				t.Fatalf("owner recovery cleaned=%d %v", n, err)
			}
			assertSettlementKey(t, b, key.ID, 0, 80)
			removeFault()
			settleCtx, settleCancel := context.WithTimeout(ctx, 5*time.Second)
			defer settleCancel()
			if err := restoredWriter.Create(settleCtx, value); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, b, key.ID, 30, 0)

		})
	}
}

func TestBillingOwnerLimitsCapacityReclamationAndAllowsCrossOwnerSettlement(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "owner-pressure")
			if err := a.db.Model(&clientKeyModel{}).Where("id = ?", key.ID).Update("billing_limit_usd_ticks", 100).Error; err != nil {
				t.Fatal(err)
			}
			keysA, keysB := NewClientKeyRepository(a), NewClientKeyRepository(b)
			ownerA := repository.BillingReservationScope{OwnerID: "owner-a"}
			ownerB := repository.BillingReservationScope{OwnerID: "owner-b"}
			value := settlementRecord(key.ID, "owner-pressure", 30)
			now := time.Now().UTC()
			if ok, err := keysA.ReserveBillingUsage(ctx, key.ID, value.EventID, 80, now.Add(-time.Hour), ownerA); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			if ok, err := keysB.ReserveBillingUsage(ctx, key.ID, "evt_owner_pressure_other", 40, now.Add(time.Hour), ownerB); ok || !errors.Is(err, repository.ErrLimitExceeded) {
				t.Fatalf("foreign capacity reclaim=%v %v", ok, err)
			}
			if ok, err := keysB.ReserveBillingUsage(ctx, key.ID, value.EventID, 80, now.Add(time.Hour), ownerB); err != nil || !ok {
				t.Fatalf("idempotent same-event cross-owner=%v %v", ok, err)
			}
			if _, err := keysB.ReserveBillingUsage(ctx, key.ID, value.EventID, 90, now.Add(time.Hour), ownerB); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("changed another owner's amount: %v", err)
			}
			var row billingReservationModel
			if err := b.db.First(&row, "event_id = ?", value.EventID).Error; err != nil {
				t.Fatal(err)
			}
			if row.OwnerID != "owner-a" || row.Amount != 80 {
				t.Fatalf("retry transferred reservation: %+v", row)
			}
			if err := NewAuditRepository(b).Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 30, 0)
			if ok, err := keysA.ReserveBillingUsage(ctx, key.ID, "evt_owner_a_reclaim_own", 60, now.Add(-time.Hour), ownerA); err != nil || !ok {
				t.Fatalf("reserve own orphan=%v %v", ok, err)
			}
			if n, err := keysB.CleanupExpiredBillingReservations(ctx, now, 100, ownerB); err != nil || n != 0 {
				t.Fatalf("foreign cleanup=%d %v", n, err)
			}
			if n, err := keysA.CleanupExpiredBillingReservations(ctx, now, 100, ownerA); err != nil || n != 1 {
				t.Fatalf("owner lost orphan recovery=%d %v", n, err)
			}
			assertSettlementKey(t, a, key.ID, 30, 0)
		})
	}
}

type legacyBillingOwnerReservation struct {
	EventID     string          `gorm:"size:64;primaryKey"`
	ClientKeyID uint64          `gorm:"not null"`
	Amount      int64           `gorm:"not null"`
	ExpiresAt   time.Time       `gorm:"not null"`
	CreatedAt   time.Time       `gorm:"not null"`
	ClientKey   *clientKeyModel `gorm:"foreignKey:ClientKeyID;references:ID;constraint:OnUpdate:CASCADE,OnDelete:CASCADE"`
}

func (legacyBillingOwnerReservation) TableName() string { return "billing_reservations" }

func TestLegacyReservationOwnerMigrationRetainsUnknownBudget(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "legacy-owner")
			keys := NewClientKeyRepository(a)
			value := settlementRecord(key.ID, "legacy-owner", 30)
			if err := a.db.Model(&clientKeyModel{}).Where("id = ?", key.ID).Updates(map[string]any{"billing_limit_usd_ticks": 100, "reserved_usage_usd_ticks": 80}).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().DropTable(&billingReservationModel{}); err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().CreateTable(&legacyBillingOwnerReservation{}); err != nil {
				t.Fatal(err)
			}
			old := legacyBillingOwnerReservation{EventID: value.EventID, ClientKeyID: key.ID, Amount: 80, ExpiresAt: time.Now().UTC().Add(-time.Hour), CreatedAt: time.Now().UTC().Add(-2 * time.Hour)}
			if err := a.db.Create(&old).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			var row billingReservationModel
			if err := b.db.First(&row, "event_id = ?", value.EventID).Error; err != nil {
				t.Fatal(err)
			}
			if row.OwnerID != "" || row.Amount != 80 {
				t.Fatalf("migration invented old ownership: %+v", row)
			}
			for _, scope := range []repository.BillingReservationScope{{OwnerID: "a"}, {OwnerID: "b"}, {}} {
				if n, err := keys.CleanupExpiredBillingReservations(ctx, time.Now().UTC(), 100, scope); err != nil || n != 0 {
					t.Fatalf("unknown legacy owner cleaned=%d %v", n, err)
				}
			}
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, "evt_legacy_owner_pressure", 40, time.Now().UTC().Add(time.Hour), repository.BillingReservationScope{OwnerID: "a"}); ok || !errors.Is(err, repository.ErrLimitExceeded) {
				t.Fatalf("unknown legacy budget released under pressure=%v %v", ok, err)
			}
			assertSettlementKey(t, a, key.ID, 0, 80)
			if err := NewAuditRepository(b).Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 30, 0)
			if n := tableRowCount(t, b, "billing_reservations"); n != 0 {
				t.Fatalf("real settlement could not finish legacy reservation: %d", n)
			}
		})
	}
}
