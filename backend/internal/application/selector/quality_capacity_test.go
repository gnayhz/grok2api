package selector

import (
	"fmt"
	"testing"
	"time"
)

func TestLocalHoldCapacityNeverEvictsAnActiveOwner(t *testing.T) {
	now := time.Now()
	selector := &Selector{qualityHolds: map[uint64]map[string]time.Time{}}
	owners := make(map[string]time.Time, maxLocalQualityOwners)
	for i := 0; i < maxLocalQualityOwners; i++ {
		owners[fmt.Sprint(i)] = now.Add(time.Minute)
	}
	selector.qualityHolds[1] = owners
	selector.holdLocalQuality(2, "overflow", now.Add(2*time.Minute))
	if len(selector.qualityHolds) != 1 || len(selector.qualityHolds[1]) != maxLocalQualityOwners {
		t.Fatal("capacity changed/evicted active protection")
	}
	if selector.localQualityAllowed(2, now) || selector.localQualityAllowed(3, now) {
		t.Fatal("overflow silently lost protection")
	}
	stats := selector.localProtectionStats()
	if stats.Owners != maxLocalQualityOwners || stats.Overflows != 1 || stats.OverflowUntil == nil {
		t.Fatalf("stats=%+v", stats)
	}
	if !selector.localQualityAllowed(3, now.Add(3*time.Minute)) {
		t.Fatal("overflow fence has no finite expiry")
	}
}
