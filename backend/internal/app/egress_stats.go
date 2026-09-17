package app

import (
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
)

func liveEgressStats(manager *infraegress.Manager) egressapp.LiveStats {
	return egressapp.LiveStats{
		Runtime: func() egressapp.RuntimeStats {
			src := manager.RuntimeStats()
			return egressapp.RuntimeStats{
				Network: src.Network, CachedClients: src.CachedClients, RetiredClients: src.RetiredClients,
				Tasks: src.Tasks, RejectedTasks: src.RejectedTasks,
				DroppedObservations: src.DroppedObservations, HealthWriteErrors: src.HealthWriteErrors,
			}
		},
		Routing: func() []egressapp.RoutingStat {
			src := infraegress.RoutingStatsSnapshot()
			out := make([]egressapp.RoutingStat, len(src))
			for i, item := range src {
				out[i] = egressapp.RoutingStat{Level: item.Level, Mode: item.Mode, Hit: item.Hit, Fallback: item.Fallback, LastSeen: item.LastSeen}
			}
			return out
		},
		Pool: func(poolID uint64) ([]egressapp.PoolNodeStat, time.Time) {
			src, since := infraegress.PoolStatsSnapshot(poolID)
			out := make([]egressapp.PoolNodeStat, len(src))
			for i, item := range src {
				out[i] = egressapp.PoolNodeStat{PoolID: item.PoolID, NodeID: item.NodeID, Selections: item.Selections, Failures: item.Failures, LastSelectedAt: item.LastSelectedAt}
			}
			return out, since
		},
		ResetPool: infraegress.ResetPoolStats,
	}
}
