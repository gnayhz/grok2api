package evidence

import (
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// windowMatrix 是内存滑窗矩阵:append-only + 按时间前缀裁剪。
// Record callers serialize memory updates; model.Snapshot returns immutable
// aggregates to concurrent readers.
type windowMatrix struct {
	mu     sync.RWMutex
	window time.Duration
	events []model.Observation
}

func newWindowMatrix(window time.Duration) *windowMatrix {
	return &windowMatrix{window: window, events: make([]model.Observation, 0, 1024)}
}

// setWindow 热应用统计窗口口径(SetConfig 调用;下一次聚合/裁剪生效)。
func (m *windowMatrix) setWindow(window time.Duration) {
	if window <= 0 {
		return
	}
	m.mu.Lock()
	m.window = window
	cutoff := time.Now().UTC().Add(-window)
	drop := 0
	for drop < len(m.events) && m.events[drop].At.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		m.events = m.events[drop:]
	}
	m.mu.Unlock()
}

func (m *windowMatrix) record(obs model.Observation) {
	obs = normalizedObservation(obs)
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.events) == 0 || !obs.At.Before(m.events[len(m.events)-1].At) {
		// 正常的时间递增路径保持 append-only,不增加热路径复杂度。
		m.events = append(m.events, obs)
	} else {
		// Concurrent producers may commit in completion order, not event time.
		// 乱序是低频路径,插入排序后继续维持“时间有序+前缀裁剪”不变量。
		index := sort.Search(len(m.events), func(index int) bool {
			return !m.events[index].At.Before(obs.At)
		})
		m.events = append(m.events, model.Observation{})
		copy(m.events[index+1:], m.events[index:])
		m.events[index] = obs
	}
	cutoff := m.events[len(m.events)-1].At.Add(-m.window)
	drop := 0
	for drop < len(m.events) && m.events[drop].At.Before(cutoff) {
		drop++
	}
	if drop > 0 {
		m.events = m.events[drop:]
	}
}

func (s *Store) SnapshotWindow(now time.Time) model.Snapshot {
	return s.snapshotWindow(now, false)
}

// AttributionWindow excludes historical traffic admissions that older versions
// stored as delivered. Keep those original rows for diagnostics, but never
// promote their first-thinking signal into a healthy comparison witness.
func (s *Store) AttributionWindow(now time.Time) model.Snapshot {
	return s.snapshotWindow(now, true)
}

func (s *Store) snapshotWindow(now time.Time, attribution bool) model.Snapshot {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.window.mu.RLock()
	defer s.window.mu.RUnlock()
	return model.AggregateObservations(s.window.events, now, s.window.window, attribution)
}

// normalizedObservation 补齐缺省字段,保证聚合一致性。
func normalizedObservation(obs model.Observation) model.Observation {
	if obs.At.IsZero() {
		obs.At = time.Now().UTC()
	}
	if obs.Source == "" {
		obs.Source = model.SourceTraffic
	}
	return obs
}

// observationFromRow 行转领域观测。
func observationFromRow(row ObservationModel) model.Observation {
	var identity attemptmeta.Identity
	_ = json.Unmarshal([]byte(row.AttemptJSON), &identity)
	eventID := ""
	if row.EventID != nil {
		eventID = *row.EventID
	}
	return model.Observation{
		EventID: eventID,
		Attempt: identity, At: row.At,
		AccountID: row.AccountID,
		Exit:      model.EpochKey{NodeID: row.NodeID, Epoch: row.Epoch},
		Outcome:   model.Outcome(row.Outcome),
		Rule:      row.Rule,
		Source:    model.Source(row.Source),
	}
}
