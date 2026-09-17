package egress

import (
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

type RuntimeStats struct {
	Network             netbudget.Stats `json:"network"`
	CachedClients       int             `json:"cachedClients"`
	RetiredClients      int             `json:"retiredClients"`
	Tasks               map[string]int  `json:"tasks"`
	RejectedTasks       uint64          `json:"rejectedTasks"`
	DroppedObservations uint64          `json:"droppedObservations"`
	HealthWriteErrors   uint64          `json:"healthWriteErrors"`
}

type RoutingStat struct {
	Level    string     `json:"level"`
	Mode     string     `json:"mode"`
	Hit      int64      `json:"hit"`
	Fallback int64      `json:"fallback"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}

type PoolNodeStat struct {
	PoolID         uint64    `json:"poolId,string"`
	NodeID         uint64    `json:"nodeId,string"`
	Selections     uint64    `json:"selections"`
	Failures       uint64    `json:"failures"`
	LastSelectedAt time.Time `json:"lastSelectedAt"`
}

type LiveStats struct {
	Runtime   func() RuntimeStats
	Routing   func() []RoutingStat
	Pool      func(poolID uint64) ([]PoolNodeStat, time.Time)
	ResetPool func(poolID uint64)
}
