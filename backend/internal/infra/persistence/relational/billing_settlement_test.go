package relational

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"gorm.io/gorm"
)

func TestBillingSettlementSurvivesAuditRetention(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			db, other := settingsDatabasePair(t, dialect)
			key := clientKeyModel{Name: "settlement", Prefix: "settlement", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8}
			if err := db.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			a, b := NewAuditRepository(db), NewAuditRepository(other)
			record := audit.Record{EventID: "evt_retained_settlement", RequestID: "late-replay", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: 30, CreatedAt: time.Now().UTC().Add(-48 * time.Hour)}
			ctx := context.Background()
			if err := a.Create(ctx, record); err != nil {
				t.Fatal(err)
			}
			if n, err := b.DeleteOlderThan(ctx, time.Now().UTC().Add(-24*time.Hour), 500); err != nil || n != 1 {
				t.Fatalf("delete=%d err=%v", n, err)
			}
			if err := b.Create(ctx, record); err != nil {
				t.Fatal(err)
			}
			var stored clientKeyModel
			if err := other.db.First(&stored, key.ID).Error; err != nil {
				t.Fatal(err)
			}
			if stored.BilledUsageUSDTicks != 30 {
				t.Fatalf("retention replay billed=%d; want first settlement 30", stored.BilledUsageUSDTicks)
			}
			if n := tableRowCount(t, other, "request_audits"); n != 0 {
				t.Fatalf("replay resurrected %d expired audits", n)
			}
		})
	}
}

func settlementTestKey(t *testing.T, db *Database, suffix string) clientKeyModel {
	t.Helper()
	key := clientKeyModel{Name: "settlement-" + suffix, Prefix: suffix, SecretHash: fmt.Sprintf("%x", sha256.Sum256([]byte(suffix))), EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 10000}
	if err := db.db.Create(&key).Error; err != nil {
		t.Fatal(err)
	}
	return key
}

func settlementRecord(keyID uint64, suffix string, amount int64) audit.Record {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return audit.Record{EventID: "evt_settlement_" + suffix, RequestID: suffix, ClientKeyID: keyID, ModelRouteID: 1, StatusCode: 200, EstimatedCostInUSDTicks: amount, CreatedAt: now,
		Attempts: []audit.Attempt{{Number: 1, Source: audit.AttemptSourceCredential, Stage: "credential", StartedAt: now}}}
}

func assertSettlementKey(t *testing.T, db *Database, keyID uint64, billed, reserved int64) {
	t.Helper()
	var stored clientKeyModel
	if err := db.db.First(&stored, keyID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.BilledUsageUSDTicks != billed || stored.ReservedUsageUSDTicks != reserved {
		t.Fatalf("key %d billed/reserved=%d/%d; want %d/%d", keyID, stored.BilledUsageUSDTicks, stored.ReservedUsageUSDTicks, billed, reserved)
	}
}

func TestBillingSettlementMixedReplayOwnershipAndZeroCost(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key, otherKey := settlementTestKey(t, a, "first"), settlementTestKey(t, a, "other")
			keys, audits := NewClientKeyRepository(a), NewAuditRepository(b)
			paid, zero, extra := settlementRecord(key.ID, "paid", 30), settlementRecord(key.ID, "zero", 0), settlementRecord(key.ID, "extra", 10)
			for _, value := range []audit.Record{paid, zero} {
				if ok, err := keys.ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
					t.Fatalf("reserve=%v %v", ok, err)
				}
			}
			duplicate := paid
			duplicate.EstimatedCostInUSDTicks = 999 // Existing first-commit-wins contract.
			if err := audits.CreateBatch(ctx, []audit.Record{paid, zero, duplicate, extra}); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 40, 0)
			if n := tableRowCount(t, a, "billing_settlements"); n != 3 {
				t.Fatalf("settlements=%d", n)
			}
			if n := tableRowCount(t, a, "request_audit_attempts"); n != 3 {
				t.Fatalf("attempts=%d", n)
			}
			if n, err := audits.DeleteOlderThan(ctx, time.Now().Add(time.Hour), 10); err != nil || n != 3 {
				t.Fatalf("delete=%d %v", n, err)
			}
			if err := audits.CreateBatch(ctx, []audit.Record{duplicate, zero, extra}); err != nil {
				t.Fatal(err)
			}
			if n := tableRowCount(t, a, "request_audits"); n != 0 {
				t.Fatalf("replay revived details=%d", n)
			}
			for _, value := range []audit.Record{paid, zero} {
				if ok, err := keys.ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || ok {
					t.Fatalf("completed event reserved again=%v %v", ok, err)
				}
			}
			assertSettlementKey(t, a, key.ID, 40, 0)
			if _, err := keys.ReserveBillingUsage(ctx, otherKey.ID, paid.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("settlement transferred to another Key: %v", err)
			}
			newRecord := settlementRecord(otherKey.ID, "new", 20)
			foreign := paid
			foreign.ClientKeyID = otherKey.ID
			err := audits.CreateBatch(ctx, []audit.Record{newRecord, foreign})
			var invalid *repository.InvalidBatchRecordError
			if !errors.As(err, &invalid) || invalid.Index != 1 || !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("foreign replay was not isolated: %v", err)
			}
			if n := tableRowCount(t, a, "billing_settlements"); n != 3 {
				t.Fatalf("failed batch leaked claim=%d", n)
			}
			if err := audits.Create(ctx, newRecord); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, otherKey.ID, 20, 0)
			// A preexisting reservation also fixes event ownership, before the
			// first settlement is submitted by either Key.
			reserved := settlementRecord(key.ID, "reserved_owner", 15)
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, reserved.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			foreign = reserved
			foreign.ClientKeyID = otherKey.ID
			if err := audits.Create(ctx, foreign); !errors.Is(err, repository.ErrInvalidRecord) || !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("foreign reservation settled: %v", err)
			}
			assertSettlementKey(t, a, key.ID, 40, 80)
			assertSettlementKey(t, a, otherKey.ID, 20, 0)
			if err := audits.Create(ctx, reserved); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 55, 0)
		})
	}
}

func TestBillingSettlementConcurrentReserveCommitAndRetention(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			key := settlementTestKey(t, a, "concurrent")
			var billed int64
			for i := range 12 {
				value := settlementRecord(key.ID, fmt.Sprintf("race_%03d", i), int64(i%3))
				billed += value.EstimatedCostInUSDTicks
				start, results := make(chan struct{}), make(chan error, 8)
				var wg sync.WaitGroup
				for worker := range 8 {
					wg.Add(1)
					go func() {
						defer wg.Done()
						<-start
						db := a
						if worker%2 == 1 {
							db = b
						}
						if worker < 3 {
							_, err := NewClientKeyRepository(db).ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"})
							results <- err
						} else if worker < 7 {
							results <- NewAuditRepository(db).Create(ctx, value)
						} else {
							_, err := NewAuditRepository(db).DeleteOlderThan(ctx, time.Now().Add(time.Hour), 10)
							results <- err
						}
					}()
				}
				close(start)
				wg.Wait()
				close(results)
				for err := range results {
					if err != nil {
						t.Fatal(err)
					}
				}
				assertSettlementKey(t, b, key.ID, billed, 0)
			}
			if n := tableRowCount(t, a, "billing_settlements"); n != 12 {
				t.Fatalf("settlements=%d", n)
			}
			if n := tableRowCount(t, a, "billing_reservations"); n != 0 {
				t.Fatalf("completed reservations remain=%d", n)
			}
		})
	}
}

func TestBillingSettlementMigrationDeletionAndRestart(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "migration")
			values := make([]audit.Record, 501)
			for i := range values {
				values[i] = settlementRecord(key.ID, fmt.Sprintf("legacy_%04d", i), 1)
			}
			values[0].EventID = ""
			if err := NewAuditRepository(a).CreateBatch(ctx, values); err != nil {
				t.Fatal(err)
			}
			// Restore an old schema, including a retained pre-event-ID audit.
			if err := a.db.Model(&requestAuditModel{}).Where("request_id = ?", values[0].RequestID).UpdateColumn("event_id", "").Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Migrator().DropTable(&billingSettlementModel{}); err != nil {
				t.Fatal(err)
			}
			// A failure after creating the new table must roll back both schema
			// and partial backfill, leaving the old ledger available for retry.
			const faultName = "settlement_legacy_adoption_fault"
			fault := func(tx *gorm.DB) {
				if tx.Statement.Table == "billing_settlements" {
					tx.AddError(errors.New("injected legacy identity write failure"))
				}
			}
			if err := b.db.Callback().Create().Before("gorm:create").Register(faultName, fault); err != nil {
				t.Fatal(err)
			}
			migrationErr := b.InitializeSchema(ctx)
			if err := b.db.Callback().Create().Remove(faultName); err != nil {
				t.Fatal(err)
			}
			if migrationErr == nil || b.db.Migrator().HasTable(&billingSettlementModel{}) {
				t.Fatalf("failed migration persisted partial state: %v", migrationErr)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, b, key.ID, 501, 0)
			if n := tableRowCount(t, b, "billing_settlements"); n != 501 {
				t.Fatalf("migration identities=%d", n)
			}
			if err := NewAuditRepository(b).CreateBatch(ctx, values); err != nil {
				t.Fatal(err)
			}
			if n := tableRowCount(t, a, "request_audits"); n != 501 {
				t.Fatalf("migration replay created audits=%d", n)
			}
			// A legacy row that reaches retention without an identity must be
			// adopted in that same deletion transaction, without a second bill.
			if err := a.db.Where("event_id = ?", values[1].EventID).Delete(&billingSettlementModel{}).Error; err != nil {
				t.Fatal(err)
			}
			if err := a.db.Callback().Create().Before("gorm:create").Register(faultName, fault); err != nil {
				t.Fatal(err)
			}
			deleted, retentionErr := NewAuditRepository(a).DeleteOlderThan(ctx, time.Now().Add(time.Hour), 1000)
			if err := a.db.Callback().Create().Remove(faultName); err != nil {
				t.Fatal(err)
			}
			if retentionErr == nil || deleted != 0 || tableRowCount(t, b, "request_audits") != 501 || tableRowCount(t, b, "request_audit_attempts") != 501 {
				t.Fatalf("retention removed unrecoverable legacy identity: %d %v", deleted, retentionErr)
			}
			if n, err := NewAuditRepository(a).DeleteOlderThan(ctx, time.Now().Add(time.Hour), 1000); err != nil || n != 501 {
				t.Fatalf("delete=%d %v", n, err)
			}
			if err := a.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			if err := NewAuditRepository(a).CreateBatch(ctx, values); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 501, 0)
			if err := NewClientKeyRepository(a).Delete(ctx, key.ID); err != nil {
				t.Fatal(err)
			}
			if err := NewAuditRepository(b).CreateBatch(ctx, values); err != nil {
				t.Fatal(err)
			}
			if n := tableRowCount(t, b, "billing_settlements"); n != 501 {
				t.Fatalf("Key deletion removed identity=%d", n)
			}
			if n := tableRowCount(t, b, "request_audits"); n != 0 {
				t.Fatalf("restart/deletion replay revived audit=%d", n)
			}
			otherKey := settlementTestKey(t, a, "replacement")
			values[1].ClientKeyID = otherKey.ID
			if err := NewAuditRepository(b).Create(ctx, values[1]); !errors.Is(err, repository.ErrConflict) {
				t.Fatalf("deleted identity reassigned: %v", err)
			}
			assertSettlementKey(t, b, otherKey.ID, 0, 0)
		})
	}
}

type settlementLostAcknowledgement struct {
	repository.AuditRepository
	calls atomic.Int32
}

func (r *settlementLostAcknowledgement) CreateBatch(ctx context.Context, values []audit.Record) error {
	if err := r.AuditRepository.CreateBatch(ctx, values); err != nil {
		return err
	}
	if r.calls.Add(1) == 1 {
		return errors.New("injected lost transaction acknowledgement")
	}
	return nil
}

func TestBillingSettlementWriterLostAcknowledgementAndReplay(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			key := settlementTestKey(t, a, "writer")
			value := settlementRecord(key.ID, "writer", 30)
			if ok, err := NewClientKeyRepository(a).ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reserve=%v %v", ok, err)
			}
			repo := &settlementLostAcknowledgement{AuditRepository: NewAuditRepository(a)}
			writer := auditapp.NewService(repo, newTestAuditJournal(t, 8), slog.Default(), 4, time.Millisecond)
			if err := writer.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := writer.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			if err := writer.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if repo.calls.Load() < 2 {
				t.Fatal("lost acknowledgement did not retry")
			}
			assertSettlementKey(t, b, key.ID, 30, 0)
			if deleted, err := NewAuditRepository(b).DeleteOlderThan(ctx, time.Now().Add(time.Hour), 10); err != nil || deleted != 1 {
				t.Fatalf("delete=%d %v", deleted, err)
			}
			restarted := auditapp.NewService(NewAuditRepository(b), newTestAuditJournal(t, 8), slog.Default(), 4, time.Millisecond)
			if err := restarted.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			value.EstimatedCostInUSDTicks = 999
			if err := restarted.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			if err := restarted.Close(ctx); err != nil {
				t.Fatal(err)
			}
			assertSettlementKey(t, a, key.ID, 30, 0)
			if tableRowCount(t, a, "request_audits") != 0 || tableRowCount(t, a, "billing_settlements") != 1 {
				t.Fatal("writer replay did not preserve retained settlement")
			}
		})
	}
}

func TestBillingSettlementWriteFailureRollsBackIdentity(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			key := settlementTestKey(t, a, "rollback")
			for i, stage := range []string{"audit", "attempt", "generation", "bill", "reservation"} {
				t.Run(stage, func(t *testing.T) {
					value := settlementRecord(key.ID, "rollback_"+stage, 30)
					value.GenerationUsages = generationUsageFixture()
					for n := range value.GenerationUsages {
						value.GenerationUsages[n].PhysicalID = fmt.Sprintf("%s/%d", stage, n)
					}
					if ok, err := NewClientKeyRepository(a).ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
						t.Fatalf("reserve=%v %v", ok, err)
					}
					const callback = "settlement_rollback"
					fault := func(tx *gorm.DB) {
						table := map[string]string{"audit": "request_audits", "attempt": "request_audit_attempts", "generation": "request_audit_generations", "bill": "client_keys", "reservation": "billing_reservations"}[stage]
						if tx.Statement.Table == table {
							tx.AddError(errors.New("injected settlement transaction failure"))
						}
					}
					var remove func() error
					switch stage {
					case "bill":
						if err := a.db.Callback().Update().Before("gorm:update").Register(callback, fault); err != nil {
							t.Fatal(err)
						}
						remove = func() error { return a.db.Callback().Update().Remove(callback) }
					case "reservation":
						if err := a.db.Callback().Delete().Before("gorm:delete").Register(callback, fault); err != nil {
							t.Fatal(err)
						}
						remove = func() error { return a.db.Callback().Delete().Remove(callback) }
					default:
						if err := a.db.Callback().Create().Before("gorm:create").Register(callback, fault); err != nil {
							t.Fatal(err)
						}
						remove = func() error { return a.db.Callback().Create().Remove(callback) }
					}
					err := NewAuditRepository(a).Create(ctx, value)
					if removeErr := remove(); removeErr != nil {
						t.Fatal(removeErr)
					}
					if err == nil {
						t.Fatal("write fault was ignored")
					}
					for _, table := range []string{"request_audits", "billing_settlements"} {
						var n int64
						if err := b.db.Table(table).Where("event_id = ?", value.EventID).Count(&n).Error; err != nil || n != 0 {
							t.Fatalf("%s survived rollback=%d %v", table, n, err)
						}
					}
					assertSettlementKey(t, b, key.ID, int64(i)*30, 80)
					if err := NewAuditRepository(b).Create(ctx, value); err != nil {
						t.Fatal(err)
					}
					assertSettlementKey(t, b, key.ID, int64(i+1)*30, 0)
				})
			}
		})
	}
}
