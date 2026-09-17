package events

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/journal"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

func TestReceiptPhasesRemainAtomicAndDistinct(t *testing.T) {
	for _, driver := range []string{"sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			reg, _ := eventRegistries(t, driver)
			svc, store, _, _ := eventServices(t, reg)
			ctx := context.Background()
			for _, test := range []struct {
				name                  string
				outcome               Outcome
				code                  string
				count                 int
				admission, completion string
				hold                  bool
			}{
				{"admission", Admitted, "", 1, "delivered", "", false},
				{"completed", Completed, "", 1, "", "completed", false},
				{"interrupted", Interrupted, "transport", 1, "", "interrupted", false},
				{"canceled", Canceled, "request_canceled", 1, "", "canceled", false},
				{"degraded", Degraded, "", 2, "degraded", "interrupted", true},
				{"rejected", Rejected, "provider_rejected", 2, "rejected", "interrupted", false},
				{"rejected_cancel", Rejected, "request_canceled", 2, "rejected", "canceled", false},
			} {
				t.Run(test.name, func(t *testing.T) {
					receipt := incidentReceipt()
					receipt.Attempt.ID = test.name
					receipt.Outcome = test.outcome
					receipt.ErrorCode = test.code
					if err := svc.RecordQualityEvent(ctx, receipt, time.Minute); err != nil {
						t.Fatal(err)
					}
					// Same immutable receipt must not duplicate facts, holds or pending slots.
					if err := svc.RecordQualityEvent(ctx, receipt, time.Minute); err != nil {
						t.Fatal(err)
					}
					var rows []journal.EventRow
					if err := reg.DB().Where("attempt_id = ?", receipt.Attempt.ID).Find(&rows).Error; err != nil || len(rows) != test.count {
						t.Fatalf("rows=%v err=%v", rows, err)
					}
					seen := map[string]string{}
					for _, row := range rows {
						var event qualitymodel.Event
						if err := json.Unmarshal([]byte(row.Payload), &event); err != nil {
							t.Fatal(err)
						}
						if event.Attempt != receipt.Attempt || !event.At.Equal(receipt.At) {
							t.Fatalf("receipt identity/time changed: %+v", event)
						}
						seen[event.Stage] = event.Outcome
					}
					if seen["admission"] != test.admission || seen["completion"] != test.completion {
						t.Fatalf("stages=%v", seen)
					}
					var count int64
					if err := reg.DB().Model(&journal.RestrictionRow{}).Where("owner = ?", receipt.Attempt.ID+"/admission").Count(&count).Error; err != nil || (count == 1) != test.hold {
						t.Fatalf("hold count=%d err=%v", count, err)
					}
				})
			}
			before, err := store.Stats(ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name   string
				modify func(*Receipt)
				ttl    time.Duration
			}{
				{"unknown", func(v *Receipt) { v.Outcome = "unexpected" }, time.Minute},
				{"missing_attempt", func(v *Receipt) { v.Attempt.ID = "" }, time.Minute},
				{"zero_time", func(v *Receipt) { v.At = time.Time{} }, time.Minute},
				{"zero_ttl", func(*Receipt) {}, 0},
				{"long_ttl", func(*Receipt) {}, 25 * time.Hour},
			} {
				t.Run(test.name, func(t *testing.T) {
					receipt := incidentReceipt()
					receipt.Attempt.ID = test.name
					test.modify(&receipt)
					if err := svc.RecordQualityEvent(ctx, receipt, test.ttl); err == nil {
						t.Fatal("invalid receipt accepted")
					}
					var count int64
					if err := reg.DB().Model(&journal.EventRow{}).Where("attempt_id = ?", receipt.Attempt.ID).Count(&count).Error; err != nil || count != 0 {
						t.Fatalf("partial receipt %d %v", count, err)
					}
				})
			}
			after, err := store.Stats(ctx)
			if err != nil || after.Pending != before.Pending || after.InFlight != before.InFlight {
				t.Fatalf("invalid receipt changed capacity before=%+v after=%+v err=%v", before, after, err)
			}
			// Duplicate-stage conflict in the second row must roll the admission back.
			conflicting := incidentReceipt()
			conflicting.Attempt.ID = "batch-conflict"
			completed := conflicting
			completed.Outcome = Completed
			if err := svc.RecordQualityEvent(ctx, completed, 0); err != nil {
				t.Fatal(err)
			}
			if err := svc.RecordQualityEvent(ctx, conflicting, time.Minute); err == nil {
				t.Fatal("conflicting batch accepted")
			}
			for _, table := range []string{"q_guard_event", "q_guard_outbox", "q_guard_restriction"} {
				key := "id"
				if table == "q_guard_outbox" {
					key = "event_id"
				}
				if table == "q_guard_restriction" {
					key = "owner"
				}
				var n int64
				if err := reg.DB().Table(table).Where(key+" = ?", conflicting.Attempt.ID+"/admission").Count(&n).Error; err != nil || n != 0 {
					t.Fatalf("partial %s count=%d err=%v", table, n, err)
				}
			}
			// Physical facts archive separately and cannot become an incident vote.
			identity := incidentReceipt().Attempt
			identity.ID = "physical-only"
			fact := attemptmeta.PhysicalFact{Attempt: identity, At: time.Now().UTC()}
			if err := svc.RecordPhysicalEvents(ctx, []attemptmeta.PhysicalFact{fact}); err != nil {
				t.Fatal(err)
			}
			var event journal.EventRow
			if err := reg.DB().First(&event, "id = ?", identity.ID+"/exchange").Error; err != nil || event.Outcome != "observed" {
				t.Fatalf("physical=%+v err=%v", event, err)
			}
			t.Log(fmt.Sprintf("receipt matrix: driver=%s phases=7 invalid=5 conflict=1 physical=1", driver))
		})
	}
}
