package evidence

import (
	"encoding/json"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"sort"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// windowMatrix 是内存滑窗矩阵:append-only + 按时间前缀裁剪。
// Record callers serialize memory updates; Snapshot returns immutable
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

// Len 返回窗口内事件数(测试与容量观测)。
func (m *windowMatrix) Len() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.events)
}

// ExitStats 是一个出口单元在窗口内的聚合(I10:error 已剔除)。
type ExitStats struct {
	Key      model.EpochKey
	N        int
	Degraded int
	Accounts map[uint64]struct{}
	LastAt   time.Time
}

// AccountStats 是一个账号在窗口内的聚合。
type AccountStats struct {
	AccountID uint64
	N         int
	Degraded  int
	Exits     map[model.EpochKey]struct{}
	LastAt    time.Time
}

type pairKey struct {
	AccountID uint64
	Exit      model.EpochKey
}

// Snapshot 是某时刻的窗口聚合(不可变)。
type Snapshot struct {
	At             time.Time
	Exits          map[model.EpochKey]ExitStats
	Accounts       map[uint64]AccountStats
	DegradedPairs  map[pairKey]int
	DecidablePairs map[pairKey]int
	// degradedTrafficPairs 仅流量源的降智对(批8:案件事件=立案/并案
	// /再犯只认流量源——探针观测是"关于被告的证据",不是"该出口的
	// 新降智事件",把探针结论并案会把调查局自己的差分目标押进去)。
	degradedTrafficPairs map[pairKey]int
	// trafficPairLastAt 流量源降智对的最近时刻(再起诉抑制消费).
	trafficPairLastAt      map[pairKey]time.Time
	trafficPairObservation map[pairKey]model.Observation
	Decidable              int
	Degraded               int
}

// PairDegraded 报告 (账号,出口) 对窗口内是否出现过降智观测。
func (s Snapshot) PairDegraded(accountID uint64, key model.EpochKey) bool {
	return s.DegradedPairs[pairKey{AccountID: accountID, Exit: key}] > 0
}

// TrafficPairDegraded 报告 (账号,出口) 对窗口内是否出现过**流量源**
// 降智观测(案件事件口径:立案/并案/T5 再犯只消费流量降智;探针源
// 降智是证据,不产生新罪名)。
func (s Snapshot) TrafficPairDegraded(accountID uint64, key model.EpochKey) bool {
	return s.degradedTrafficPairs[pairKey{AccountID: accountID, Exit: key}] > 0
}

// TrafficDegradedPairs 枚举流量源降智对(立案遍历消费)。
func (s Snapshot) TrafficDegradedPairs() []TrafficPair {
	pairs := make([]TrafficPair, 0, len(s.degradedTrafficPairs))
	for pair, count := range s.degradedTrafficPairs {
		pairs = append(pairs, TrafficPair{AccountID: pair.AccountID, Exit: pair.Exit, Observation: s.trafficPairObservation[pair], Count: count, LastAt: s.trafficPairLastAt[pair]})
	}
	return pairs
}

// TrafficPair 一条流量源降智对。
type TrafficPair struct {
	Observation model.Observation
	AccountID   uint64
	Exit        model.EpochKey
	Count       int
	// LastAt 窗口内最近一次降智时刻(再起诉抑制消费:销案后没有
	// 比结案更新的降智,不得再立——批9 一次请求循环出几十案的根)。
	LastAt time.Time
}

// PairStat 一个 (账号,出口) 对的窗口内观测构成(证据矩阵消费:
// clean=可判定-降智,error 观测不计入可判定)。
type PairStat struct {
	AccountID uint64
	Exit      model.EpochKey
	Decidable int
	Degraded  int
}

// Clean 干净观测数(可判定中非降智部分)。
func (p PairStat) Clean() int {
	c := p.Decidable - p.Degraded
	if c < 0 {
		return 0
	}
	return c
}

// PairStats 枚举窗口内全部 (账号,出口) 对的观测构成。
func (s Snapshot) PairStats() []PairStat {
	pairs := make([]PairStat, 0, len(s.DecidablePairs))
	for pair, decidable := range s.DecidablePairs {
		pairs = append(pairs, PairStat{
			AccountID: pair.AccountID,
			Exit:      pair.Exit,
			Decidable: decidable,
			Degraded:  s.DegradedPairs[pair],
		})
	}
	return pairs
}

// Incidence 返回全局降智发病率(可判定观测中降智占比)与覆盖面。
// 分母为 0 时返回 0(无信息 ≠ 健康,调用方按弃权处理)。
func (s Snapshot) Incidence() (rate float64, nodes, accounts int) {
	nodeSet := make(map[uint64]struct{}, len(s.Exits))
	for key := range s.Exits {
		nodeSet[key.NodeID] = struct{}{}
	}
	if s.Decidable == 0 {
		return 0, len(nodeSet), len(s.Accounts)
	}
	return float64(s.Degraded) / float64(s.Decidable), len(nodeSet), len(s.Accounts)
}

// SnapshotWindow 聚合当前窗口(注册于 Store,供 court 节拍消费)。
func (s *Store) SnapshotWindow(now time.Time) Snapshot {
	return s.snapshotWindow(now, false)
}

// AttributionWindow excludes historical traffic admissions that older versions
// stored as delivered. Keep those original rows for diagnostics, but never
// promote their first-thinking signal into a healthy comparison witness.
func (s *Store) AttributionWindow(now time.Time) Snapshot {
	return s.snapshotWindow(now, true)
}

func (s *Store) snapshotWindow(now time.Time, attribution bool) Snapshot {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	s.window.mu.RLock()
	defer s.window.mu.RUnlock()
	cutoff := now.Add(-s.window.window)
	snapshot := Snapshot{
		At:                     now,
		Exits:                  make(map[model.EpochKey]ExitStats),
		Accounts:               make(map[uint64]AccountStats),
		DegradedPairs:          make(map[pairKey]int),
		DecidablePairs:         make(map[pairKey]int),
		degradedTrafficPairs:   make(map[pairKey]int),
		trafficPairLastAt:      make(map[pairKey]time.Time),
		trafficPairObservation: make(map[pairKey]model.Observation),
	}
	for _, event := range s.window.events {
		if attribution && event.Source == model.SourceTraffic && event.Outcome == model.OutcomeDelivered {
			continue
		}
		if attribution && event.Source == model.SourceProbe && event.Attempt.ID == "" {
			continue
		}
		if event.At.Before(cutoff) || !event.Outcome.Decidable() {
			continue
		}
		exit := snapshot.Exits[event.Exit]
		if exit.Accounts == nil {
			exit.Key = event.Exit
			exit.Accounts = make(map[uint64]struct{})
		}
		exit.N++
		if event.Outcome == model.OutcomeDegraded {
			exit.Degraded++
		}
		exit.Accounts[event.AccountID] = struct{}{}
		if event.At.After(exit.LastAt) {
			exit.LastAt = event.At
		}
		snapshot.Exits[event.Exit] = exit

		account := snapshot.Accounts[event.AccountID]
		if account.Exits == nil {
			account.AccountID = event.AccountID
			account.Exits = make(map[model.EpochKey]struct{})
		}
		account.N++
		if event.Outcome == model.OutcomeDegraded {
			account.Degraded++
		}
		account.Exits[event.Exit] = struct{}{}
		if event.At.After(account.LastAt) {
			account.LastAt = event.At
		}
		snapshot.Accounts[event.AccountID] = account

		snapshot.Decidable++
		pair := pairKey{AccountID: event.AccountID, Exit: event.Exit}
		snapshot.DecidablePairs[pair]++
		if event.Outcome == model.OutcomeDegraded {
			snapshot.Degraded++
			snapshot.DegradedPairs[pair]++
			if event.Source == model.SourceTraffic {
				snapshot.degradedTrafficPairs[pair]++
				if event.At.After(snapshot.trafficPairLastAt[pair]) {
					snapshot.trafficPairLastAt[pair] = event.At
					snapshot.trafficPairObservation[pair] = event
				}
			}
		}
	}
	return snapshot
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
func observationFromRow(row qObservationModel) model.Observation {
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
