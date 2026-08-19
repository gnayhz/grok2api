package egress

import (
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestRouteRuleStatsCollectorCounts(t *testing.T) {
	collector := &routeRuleStatsCollector{entries: make(map[domain.Scope]map[domain.TrafficClass]*RouteRuleStat)}
	collector.record(domain.ScopeBuild, domain.TrafficClassInference, "hit")
	collector.record(domain.ScopeBuild, domain.TrafficClassInference, "hit")
	collector.record(domain.ScopeBuild, domain.TrafficClassInference, "skipped_binding")
	collector.record(domain.ScopeBuild, domain.TrafficClassBilling, "node_unavailable")
	collector.record(domain.ScopeBuild, domain.TrafficClassCredential, "direct_unavailable")
	collector.record(domain.ScopeBuild, domain.TrafficClassModelSync, "unknown_outcome")

	snapshot := collector.Snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("snapshot entries = %d, want 3 (unknown outcome ignored)", len(snapshot))
	}
	byClass := make(map[string]RouteRuleStat, len(snapshot))
	for _, stat := range snapshot {
		byClass[stat.Class] = stat
	}
	inference := byClass[string(domain.TrafficClassInference)]
	if inference.Hit != 2 || inference.SkippedBinding != 1 || inference.LastSeen == nil {
		t.Fatalf("inference stat = %+v", inference)
	}
	if byClass[string(domain.TrafficClassBilling)].NodeUnavailable != 1 {
		t.Fatalf("billing stat = %+v", byClass[string(domain.TrafficClassBilling)])
	}
	if byClass[string(domain.TrafficClassCredential)].DirectUnavailable != 1 {
		t.Fatalf("credential stat = %+v", byClass[string(domain.TrafficClassCredential)])
	}
	if _, exists := byClass[string(domain.TrafficClassModelSync)]; exists {
		t.Fatal("unknown outcome must not create an entry")
	}
}

func TestRouteRuleStatsSnapshotIsCopy(t *testing.T) {
	collector := &routeRuleStatsCollector{entries: make(map[domain.Scope]map[domain.TrafficClass]*RouteRuleStat)}
	collector.record(domain.ScopeBuild, domain.TrafficClassInference, "hit")
	first := collector.Snapshot()
	first[0].Hit = 999
	second := collector.Snapshot()
	if second[0].Hit != 1 {
		t.Fatalf("snapshot must be a copy, got hit=%d after external mutation", second[0].Hit)
	}
}

func TestRouteRuleStatsSnapshotCanonicalOrder(t *testing.T) {
	collector := &routeRuleStatsCollector{entries: make(map[domain.Scope]map[domain.TrafficClass]*RouteRuleStat)}
	// Insert in reverse order; snapshot must come back in canonical class order.
	collector.record(domain.ScopeBuild, domain.TrafficClassVideo, "hit")
	collector.record(domain.ScopeBuild, domain.TrafficClassInference, "hit")
	snapshot := collector.Snapshot()
	if len(snapshot) != 2 || snapshot[0].Class != string(domain.TrafficClassInference) {
		t.Fatalf("snapshot order = %+v, want inference first", snapshot)
	}
}
