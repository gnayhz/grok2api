package relational

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"gorm.io/gorm"
)

func generationUsageFixture() []audit.GenerationUsage {
	return []audit.GenerationUsage{
		{PhysicalID: "generation-request/1", Ordinal: 1, AccountID: 11, AccountName: "first", Model: "grok-4.5", Outcome: "completed", UsageSource: audit.UsageSourceUpstream, InputTokens: 100, CachedInputTokens: 30, CacheCreationTokens: 2, OutputTokens: 15, ReasoningTokens: 4, TotalTokens: 115, ContextInputTokens: 120, ContextOutputTokens: 15, CostInUSDTicks: 12_345},
		{PhysicalID: "generation-request/2", Ordinal: 2, AccountID: 22, AccountName: "selected", Model: "grok-4.5", Selected: true, Outcome: "completed", UsageSource: audit.UsageSourceUpstream, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 55},
	}
}

func TestGenerationUsageMigrationAndSettlement(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repoA, repoB := NewAuditRepository(a), NewAuditRepository(b)
			legacy := audit.Record{EventID: "evt_generation_legacy_0001", RequestID: "legacy", ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, InputTokens: 7, CreatedAt: time.Now().UTC()}
			if err := repoA.Create(ctx, legacy); err != nil {
				t.Fatal(err)
			}
			// Reconstruct the previous schema after storing an old row, then
			// upgrade from a second database connection.
			if err := a.db.Migrator().DropTable(&requestAuditGenerationModel{}); err != nil {
				t.Fatal(err)
			}
			if err := b.InitializeSchema(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := repoB.Get(ctx, 1)
			if err != nil || old.InputTokens != 7 || len(old.GenerationUsages) != 0 {
				t.Fatalf("legacy changed: %+v %v", old, err)
			}
			key := clientKeyModel{Name: "generation-billing", Prefix: "generation", SecretHash: testSecretHash, EncryptedSecret: testEncryptedToken, Enabled: true, RPMLimit: 120, MaxConcurrent: 8, BillingLimitUSDTicks: 1000}
			if err := a.db.Create(&key).Error; err != nil {
				t.Fatal(err)
			}
			event := "evt_generation_selected_0001"
			keys := NewClientKeyRepository(a)
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, event, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reservation: %v %v", ok, err)
			}
			value := audit.Record{EventID: event, RequestID: "selected", ClientKeyID: key.ID, ModelRouteID: 1, StatusCode: 200, InputTokens: 20, OutputTokens: 5, TotalTokens: 25, CostInUSDTicks: 55, GenerationUsages: generationUsageFixture(), CreatedAt: time.Now().UTC()}
			var wg sync.WaitGroup
			results := make(chan error, 16)
			for i := 0; i < 16; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					repo := repoA
					if i%2 == 1 {
						repo = repoB
					}
					results <- repo.Create(ctx, value)
				}(i)
			}
			wg.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal(err)
				}
			}
			var row requestAuditModel
			if err := b.db.Where("event_id = ?", event).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			stored, err := repoB.Get(ctx, row.ID)
			if err != nil || !reflect.DeepEqual(stored.GenerationUsages, value.GenerationUsages) {
				t.Fatalf("lost facts: %+v %v", stored.GenerationUsages, err)
			}
			var billed clientKeyModel
			if err := b.db.First(&billed, key.ID).Error; err != nil {
				t.Fatal(err)
			}
			if billed.BilledUsageUSDTicks != 55 || billed.ReservedUsageUSDTicks != 0 {
				t.Fatalf("details changed billing: %+v", billed)
			}
			listed, count, err := repoA.List(ctx, 0, 20)
			if err != nil || count != 2 {
				t.Fatalf("logical request count=%d %v", count, err)
			}
			for _, v := range listed {
				if len(v.GenerationUsages) != 0 {
					t.Fatal("list loaded generation detail")
				}
			}
			totals, err := repoA.SumTokensByAccountsSince(ctx, []uint64{11, 22}, time.Now().Add(-time.Hour))
			if err != nil || totals[11] != 115 || totals[22] != 25 {
				var debug []requestAuditGenerationModel
				a.db.Find(&debug)
				t.Fatalf("account consumption=%v err=%v stored=%+v", totals, err, debug)
			}
			if n := tableRowCount(t, a, "request_audit_generations"); n != 2 {
				t.Fatalf("duplicate generation rows: %d", n)
			}
			// A billing write failure occurs after the audit insert in the same
			// transaction. Neither the new facts nor settlement may escape it.
			value.EventID, value.RequestID = "evt_generation_rollback_0001", "rollback"
			for i := range value.GenerationUsages {
				value.GenerationUsages[i].PhysicalID = fmt.Sprintf("rollback/%d", i+1)
			}
			if ok, err := keys.ReserveBillingUsage(ctx, key.ID, value.EventID, 80, time.Now().Add(time.Hour), repository.BillingReservationScope{OwnerID: "test-owner"}); err != nil || !ok {
				t.Fatalf("reservation: %v %v", ok, err)
			}
			const callback = "generation_billing_failure"
			if err := a.db.Callback().Update().Before("gorm:update").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "client_keys" {
					tx.AddError(errors.New("injected billing update failure"))
				}
			}); err != nil {
				t.Fatal(err)
			}
			err = repoA.Create(ctx, value)
			if removeErr := a.db.Callback().Update().Remove(callback); removeErr != nil {
				t.Fatal(removeErr)
			}
			if err == nil {
				t.Fatal("billing fault did not abort audit")
			}
			var n int64
			if err := b.db.Model(&requestAuditModel{}).Where("event_id = ?", value.EventID).Count(&n).Error; err != nil || n != 0 {
				t.Fatalf("rolled back audit survived: %d %v", n, err)
			}
			if err := b.db.First(&billed, key.ID).Error; err != nil {
				t.Fatal(err)
			}
			if n := tableRowCount(t, a, "request_audit_generations"); n != 2 {
				t.Fatalf("rollback leaked generation rows: %d", n)
			}
			if billed.BilledUsageUSDTicks != 55 || billed.ReservedUsageUSDTicks != 80 {
				t.Fatal("billing rollback lost reservation")
			}
			if err := repoB.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			// The storage bound rejects malformed facts before accepting a row.
			value.EventID, value.RequestID = "evt_generation_invalid_0001", "invalid"
			value.GenerationUsages = append(value.GenerationUsages, value.GenerationUsages[0])
			if err := repoA.Create(ctx, value); err == nil {
				t.Fatal("duplicate physical identity accepted")
			}
			// The full request budget fits without truncating the JSON document.
			value.EventID, value.RequestID = "evt_generation_maximum_0001", "maximum"
			value.GenerationUsages = nil
			for i := 1; i <= 128; i++ {
				v := generationUsageFixture()[0]
				v.PhysicalID = fmt.Sprintf("bounded/%d", i)
				v.Ordinal = uint64(i)
				value.GenerationUsages = append(value.GenerationUsages, v)
			}
			if err := repoB.Create(ctx, value); err != nil {
				t.Fatal(err)
			}
			row = requestAuditModel{}
			if err := a.db.Where("event_id = ?", value.EventID).First(&row).Error; err != nil {
				t.Fatal(err)
			}
			stored, err = repoA.Get(ctx, row.ID)
			if err != nil || len(stored.GenerationUsages) != 128 {
				t.Fatalf("truncated bound: %d %v", len(stored.GenerationUsages), err)
			}
			if deleted, err := repoA.DeleteOlderThan(ctx, time.Now().Add(time.Hour), 100); err != nil || deleted != 4 {
				t.Fatalf("delete audits=%d err=%v", deleted, err)
			}
			if n := tableRowCount(t, b, "request_audit_generations"); n != 0 {
				t.Fatalf("retention orphaned generations: %d", n)
			}
		})
	}
}

func TestGenerationUsageBatchTransactions(t *testing.T) {
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			a, b := settingsDatabasePair(t, dialect)
			ctx := context.Background()
			repo := NewAuditRepository(a)
			makeRecord := func(n int) audit.Record {
				v := audit.Record{EventID: fmt.Sprintf("evt_generation_batch_%04d", n), RequestID: fmt.Sprintf("batch-%d", n), ClientKeyID: 1, ModelRouteID: 1, StatusCode: 200, CreatedAt: time.Now().UTC(), GenerationUsages: generationUsageFixture()}
				for i := range v.GenerationUsages {
					v.GenerationUsages[i].PhysicalID = fmt.Sprintf("batch-%d/%d", n, i+1)
				}
				return v
			}
			first, second, third := makeRecord(1), makeRecord(2), makeRecord(3)
			if err := repo.CreateBatch(ctx, []audit.Record{first, second}); err != nil {
				t.Fatal(err)
			}
			if err := NewAuditRepository(b).CreateBatch(ctx, []audit.Record{first, third}); err != nil {
				t.Fatal(err)
			}
			if n := tableRowCount(t, a, "request_audits"); n != 3 {
				t.Fatalf("logical rows=%d", n)
			}
			if n := tableRowCount(t, b, "request_audit_generations"); n != 6 {
				t.Fatalf("generation rows=%d", n)
			}
			fourth := makeRecord(4)
			fourth.GenerationUsages[0].PhysicalID = first.GenerationUsages[0].PhysicalID
			if err := repo.CreateBatch(ctx, []audit.Record{makeRecord(5), fourth}); err == nil {
				t.Fatal("conflicting physical identity accepted")
			}
			if n := tableRowCount(t, b, "request_audits"); n != 3 {
				t.Fatalf("batch rollback leaked logical rows=%d", n)
			}
			if n := tableRowCount(t, b, "request_audit_generations"); n != 6 {
				t.Fatalf("batch rollback leaked generation rows=%d", n)
			}
		})
	}
}
