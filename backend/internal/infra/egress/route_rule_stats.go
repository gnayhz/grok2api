package egress

import (
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// RouteRuleOutcome names the observable result of one route-rule decision.
// Transport code records these; the stats collector consumes them. Sharing
// typed constants across both sides keeps a rename from silently zeroing the
// counters (unknown values are dropped on purpose).
type RouteRuleOutcome string

const (
	RouteRuleOutcomeHit               RouteRuleOutcome = "hit"
	RouteRuleOutcomeSkippedBinding    RouteRuleOutcome = "skipped_binding"
	RouteRuleOutcomeNodeUnavailable   RouteRuleOutcome = "node_unavailable"
	RouteRuleOutcomeDirectUnavailable RouteRuleOutcome = "direct_unavailable"
)

// RouteRuleStat is one traffic class's observed route-rule decisions since
// process start. Counts are process-local and reset on restart, matching the
// lifetime of the existing performance_metric counters.
type RouteRuleStat struct {
	Scope             domain.Scope `json:"scope"`
	Class             string       `json:"class"`
	Hit               int64        `json:"hit"`
	SkippedBinding    int64        `json:"skippedBinding"`
	NodeUnavailable   int64        `json:"nodeUnavailable"`
	DirectUnavailable int64        `json:"directUnavailable"`
	LastSeen          *time.Time   `json:"lastSeen,omitempty"`
}

// routeRuleStats accumulates route-rule outcomes in process memory. It is a
// tiny dedicated counter instead of perfmetrics because the registry drains
// counters into logs (CollectAndReset), while the admin UI needs a live
// snapshot that survives the drain cycle.
type routeRuleStatsCollector struct {
	mu      sync.Mutex
	entries map[domain.Scope]map[domain.TrafficClass]*RouteRuleStat
}

var routeRuleStats = &routeRuleStatsCollector{
	entries: make(map[domain.Scope]map[domain.TrafficClass]*RouteRuleStat),
}

func (c *routeRuleStatsCollector) record(scope domain.Scope, class domain.TrafficClass, outcome RouteRuleOutcome) {
	// RouteRuleClasses returns nil for scopes without rule support; recording
	// entries for them would silently allocate memory the snapshot never emits.
	if len(domain.RouteRuleClasses(scope)) == 0 || !class.IsValid() {
		return
	}
	switch outcome {
	case RouteRuleOutcomeHit, RouteRuleOutcomeSkippedBinding, RouteRuleOutcomeNodeUnavailable, RouteRuleOutcomeDirectUnavailable:
	default:
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	byClass, ok := c.entries[scope]
	if !ok {
		byClass = make(map[domain.TrafficClass]*RouteRuleStat, 4)
		c.entries[scope] = byClass
	}
	stat, ok := byClass[class]
	if !ok {
		stat = &RouteRuleStat{Scope: scope, Class: string(class)}
		byClass[class] = stat
	}
	switch outcome {
	case RouteRuleOutcomeHit:
		stat.Hit++
	case RouteRuleOutcomeSkippedBinding:
		stat.SkippedBinding++
	case RouteRuleOutcomeNodeUnavailable:
		stat.NodeUnavailable++
	case RouteRuleOutcomeDirectUnavailable:
		stat.DirectUnavailable++
	}
	now := time.Now().UTC()
	stat.LastSeen = &now
}

// Snapshot returns a stable copy ordered by the canonical class order, so
// API responses are deterministic for the UI.
func (c *routeRuleStatsCollector) Snapshot() []RouteRuleStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]RouteRuleStat, 0, len(c.entries))
	for scope := range c.entries {
		for _, class := range domain.RouteRuleClasses(scope) {
			stat, ok := c.entries[scope][class]
			if !ok {
				continue
			}
			copied := *stat
			if stat.LastSeen != nil {
				seen := *stat.LastSeen
				copied.LastSeen = &seen
			}
			result = append(result, copied)
		}
	}
	return result
}

// RouteRuleStatsSnapshot exposes the live counters for read-only endpoints.
func RouteRuleStatsSnapshot() []RouteRuleStat { return routeRuleStats.Snapshot() }

// RecordRouteRuleOutcome observes route-rule decisions for operators: the
// perfmetrics counter feeds log aggregation while the in-process collector
// feeds the admin UI snapshot.
func RecordRouteRuleOutcome(scope domain.Scope, class domain.TrafficClass, outcome RouteRuleOutcome) {
	if !class.IsValid() {
		return
	}
	routeRuleStats.record(scope, class, outcome)
	perfmetrics.Default.Inc("egress_route_rule_total", perfmetrics.Labels{
		Subsystem: "egress",
		Operation: "route_rule",
		Provider:  string(scope),
		Stage:     string(class),
		Outcome:   string(outcome),
	})
}
