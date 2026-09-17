package model

import "time"

// ExitStats 是一个出口单元在窗口内的聚合，不包含错误观测。
type ExitStats struct {
	Key      EpochKey
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
	Exits     map[EpochKey]struct{}
	LastAt    time.Time
}

type PairKey struct {
	AccountID uint64
	Exit      EpochKey
}

// Snapshot 是某时刻的窗口聚合(不可变)。
type Snapshot struct {
	At             time.Time
	Exits          map[EpochKey]ExitStats
	Accounts       map[uint64]AccountStats
	DegradedPairs  map[PairKey]int
	DecidablePairs map[PairKey]int
	// degradedTrafficPairs 仅包含流量源的降智对。立案、并案和再犯
	// 只消费流量源；探针观测用于已有案件的比较，不能触发新案件。
	degradedTrafficPairs map[PairKey]int
	// trafficPairLastAt 流量源降智对的最近时刻(再起诉抑制消费).
	trafficPairLastAt      map[PairKey]time.Time
	trafficPairObservation map[PairKey]Observation
	Decidable              int
	Degraded               int
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
	Observation Observation
	AccountID   uint64
	Exit        EpochKey
	Count       int
	// LastAt 是窗口内最近一次降智时刻；只有比结案更新的降智才能再立案。
	LastAt time.Time
}

// PairStat 一个 (账号,出口) 对的窗口内观测构成(证据矩阵消费:
// clean=可判定-降智,error 观测不计入可判定)。
type PairStat struct {
	AccountID uint64
	Exit      EpochKey
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

// AggregateObservations builds a fresh snapshot from observations and an explicit
// clock/window. Attribution excludes legacy admissions and untracked probes.
func AggregateObservations(events []Observation, now time.Time, window time.Duration, attribution bool) Snapshot {
	cutoff := now.Add(-window)
	snapshot := Snapshot{
		At:                     now,
		Exits:                  make(map[EpochKey]ExitStats),
		Accounts:               make(map[uint64]AccountStats),
		DegradedPairs:          make(map[PairKey]int),
		DecidablePairs:         make(map[PairKey]int),
		degradedTrafficPairs:   make(map[PairKey]int),
		trafficPairLastAt:      make(map[PairKey]time.Time),
		trafficPairObservation: make(map[PairKey]Observation),
	}
	for _, event := range events {
		if attribution && event.Source == SourceTraffic && event.Outcome == OutcomeDelivered {
			continue
		}
		if attribution && event.Source == SourceProbe && event.Attempt.ID == "" {
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
		if event.Outcome == OutcomeDegraded {
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
			account.Exits = make(map[EpochKey]struct{})
		}
		account.N++
		if event.Outcome == OutcomeDegraded {
			account.Degraded++
		}
		account.Exits[event.Exit] = struct{}{}
		if event.At.After(account.LastAt) {
			account.LastAt = event.At
		}
		snapshot.Accounts[event.AccountID] = account

		snapshot.Decidable++
		pair := PairKey{AccountID: event.AccountID, Exit: event.Exit}
		snapshot.DecidablePairs[pair]++
		if event.Outcome == OutcomeDegraded {
			snapshot.Degraded++
			snapshot.DegradedPairs[pair]++
			if event.Source == SourceTraffic {
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

// Cross-validation summarizes the rolling evidence window for incident
// discovery and comparison-candidate planning. It excludes implicated witness
// relationships and reports observed rates; these are not calibrated causal
// probabilities. Court evaluates retained controlled experiments to decide
// a case, rather than sentencing a resource from this rolling estimate.

// SubjectEstimate 是一个被估对象(账号或出口)的过滤后估计。
type SubjectEstimate struct {
	// N/Degraded 过滤后可判定观测数与其中降智数。
	N        int
	Degraded int
	// Rate 降智率;N=0 时为 0(无信息,调用方弃权)。
	Rate float64
	// Witnesses 为该估计作证的健康对方数(可解释性)。
	// 出口对象=健康他处账号数;账号对象=对他人健康出口数。
	Witnesses int
	// DegradedUnits 见证对方中被降智牵连的去重单元数:
	// 出口对象=被降的见证账号数(陪审团降票);账号对象=被告被降的
	// 见证出口数。定罪门槛按"去重单元"而非观测次数计(基准 3.5:
	// ≥N 出口全降,不是 ≥N 次降)。
	DegradedUnits int
	// SpanNodes 仅账号对象有意义:被降见证出口跨越的节点数
	// (账号定罪须跨节点,排除单节点故障的替代解释)。
	SpanNodes int
}

// Estimate 是互证过滤后的全体估计。
type Estimate struct {
	Accounts map[uint64]SubjectEstimate
	Exits    map[EpochKey]SubjectEstimate
}

// CrossValidate 对快照做互证过滤估计(纯函数)。
// minWitnessObs 为见证资格的最低他处观测量。
func CrossValidate(snapshot Snapshot, minWitnessObs int) Estimate {
	if minWitnessObs <= 0 {
		minWitnessObs = 1
	}
	estimate := Estimate{
		Accounts: make(map[uint64]SubjectEstimate, len(snapshot.Accounts)),
		Exits:    make(map[EpochKey]SubjectEstimate, len(snapshot.Exits)),
	}
	// 账号 a 作为反对出口 e 的证人 ⟺ a 在 e 之外的他处可判定观测
	// 数量达 minWitnessObs 且全部健康(边际总量减本对计数,O(1))。
	accountHealthyElsewhere := func(accountID uint64, except EpochKey) bool {
		stats, ok := snapshot.Accounts[accountID]
		if !ok {
			return false
		}
		pair := PairKey{AccountID: accountID, Exit: except}
		elsewhereDecidable := stats.N - snapshot.DecidablePairs[pair]
		elsewhereDegraded := stats.Degraded - snapshot.DegradedPairs[pair]
		return elsewhereDecidable >= minWitnessObs && elsewhereDegraded == 0
	}
	// 出口 e 作为反对账号 a 的证人 ⟺ e 对其他账号的可判定观测
	// 数量达 minWitnessObs 且全部健康。
	exitHealthyForOthers := func(key EpochKey, exceptAccount uint64) bool {
		stats, ok := snapshot.Exits[key]
		if !ok {
			return false
		}
		pair := PairKey{AccountID: exceptAccount, Exit: key}
		othersDecidable := stats.N - snapshot.DecidablePairs[pair]
		othersDegraded := stats.Degraded - snapshot.DegradedPairs[pair]
		return othersDecidable >= minWitnessObs && othersDegraded == 0
	}
	for key, stats := range snapshot.Exits {
		subject := SubjectEstimate{}
		for accountID := range stats.Accounts {
			if !accountHealthyElsewhere(accountID, key) {
				continue
			}
			subject.Witnesses++
			// 该证人在本出口的可判定/降智观测(按对统计)。
			pair := PairKey{AccountID: accountID, Exit: key}
			subject.N += snapshot.DecidablePairs[pair]
			if snapshot.DegradedPairs[pair] > 0 {
				subject.DegradedUnits++
				subject.Degraded += snapshot.DegradedPairs[pair]
			}
		}
		if subject.N > 0 {
			subject.Rate = float64(subject.Degraded) / float64(subject.N)
		}
		estimate.Exits[key] = subject
	}
	for accountID, stats := range snapshot.Accounts {
		subject := SubjectEstimate{}
		spanNodes := map[uint64]struct{}{}
		for key := range stats.Exits {
			if !exitHealthyForOthers(key, accountID) {
				continue
			}
			subject.Witnesses++
			pair := PairKey{AccountID: accountID, Exit: key}
			subject.N += snapshot.DecidablePairs[pair]
			if snapshot.DegradedPairs[pair] > 0 {
				subject.DegradedUnits++
				subject.Degraded += snapshot.DegradedPairs[pair]
				spanNodes[key.NodeID] = struct{}{}
			}
		}
		subject.SpanNodes = len(spanNodes)
		if subject.N > 0 {
			subject.Rate = float64(subject.Degraded) / float64(subject.N)
		}
		estimate.Accounts[accountID] = subject
	}
	return estimate
}
