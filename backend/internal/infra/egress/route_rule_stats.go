package egress

import (
	"sort"
	"sync"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// RoutingOutcome names the observable result of one routing decision.
// Transport code records these; the stats collector consumes them. Sharing
// typed constants across both sides keeps a rename from silently zeroing the
// counters (unknown values are dropped on purpose).
type RoutingOutcome string

const (
	RoutingOutcomeHit      RoutingOutcome = "hit"
	RoutingOutcomeFallback RoutingOutcome = "fallback"
)

// RoutingStat is one routing level's observed decisions since process
// start. Counts are process-local and reset on restart, matching the
// lifetime of the existing performance_metric counters.
type RoutingStat struct {
	Level    string     `json:"level"`
	Mode     string     `json:"mode"`
	Hit      int64      `json:"hit"`
	Fallback int64      `json:"fallback"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

// routingStats accumulates routing outcomes in process memory. It is a
// tiny dedicated counter instead of perfmetrics because the registry drains
// counters into logs (CollectAndReset), while the admin UI needs a live
// snapshot that survives the drain cycle.
type routingStatsCollector struct {
	mu      sync.Mutex
	entries map[string]*RoutingStat
}

var routingStats = &routingStatsCollector{
	entries: make(map[string]*RoutingStat),
}

// decidingRoutingLevel delegates to the domain ladder shared with TargetFor:
// 归因层级必须来自与实际决策同一条路径,任何本地复制都会随演化静默漂移。
func decidingRoutingLevel(config domain.OperationsConfig, scope domain.Scope, class domain.TrafficClass) (string, bool) {
	return config.DecidingLevel(scope, class)
}

func (c *routingStatsCollector) record(level string, mode string, outcome RoutingOutcome) {
	switch outcome {
	case RoutingOutcomeHit, RoutingOutcomeFallback:
	default:
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := level + "|" + mode
	stat, ok := c.entries[key]
	if !ok {
		stat = &RoutingStat{Level: level, Mode: mode}
		c.entries[key] = stat
	}
	switch outcome {
	case RoutingOutcomeHit:
		stat.Hit++
	case RoutingOutcomeFallback:
		stat.Fallback++
	}
	now := time.Now().UTC()
	stat.LastSeen = &now
}

// Snapshot returns a stable copy ordered by level then mode so API
// responses are deterministic for the UI.
func (c *routingStatsCollector) Snapshot() []RoutingStat {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := make([]RoutingStat, 0, len(c.entries))
	for _, stat := range c.entries {
		copied := *stat
		if stat.LastSeen != nil {
			seen := *stat.LastSeen
			copied.LastSeen = &seen
		}
		result = append(result, copied)
	}
	// 排序兑现上方承诺:条目来自 map 遍历,顺序随机;UI 每次轮询行序跳动
	// 会让操作者误以为路由活动在变化。按 level 再 mode 稳定排序。
	sort.Slice(result, func(i, j int) bool {
		if result[i].Level != result[j].Level {
			return result[i].Level < result[j].Level
		}
		return result[i].Mode < result[j].Mode
	})
	return result
}

// RoutingStatsSnapshot exposes the live counters for read-only endpoints.
func RoutingStatsSnapshot() []RoutingStat { return routingStats.Snapshot() }

// RecordRoutingOutcome observes routing decisions for operators: the
// perfmetrics counter feeds log aggregation while the in-process collector
// feeds the admin UI snapshot.
func RecordRoutingOutcome(level string, target domain.RoutingTarget, outcome RoutingOutcome) {
	routingStats.record(level, string(target.Mode.Normalized()), outcome)
	perfmetrics.Default.Inc("egress_route_rule_total", perfmetrics.Labels{
		Subsystem: "egress",
		Operation: "route_rule",
		Stage:     level,
		Outcome:   string(outcome),
	})
}
