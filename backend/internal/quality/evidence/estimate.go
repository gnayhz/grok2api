package evidence

import (
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

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
	Exits    map[model.EpochKey]SubjectEstimate
}

// CrossValidate 对快照做互证过滤估计(纯函数)。
// minWitnessObs 为见证资格的最低他处观测量。
func CrossValidate(snapshot Snapshot, minWitnessObs int) Estimate {
	if minWitnessObs <= 0 {
		minWitnessObs = 1
	}
	estimate := Estimate{
		Accounts: make(map[uint64]SubjectEstimate, len(snapshot.Accounts)),
		Exits:    make(map[model.EpochKey]SubjectEstimate, len(snapshot.Exits)),
	}
	// 账号 a 作为反对出口 e 的证人 ⟺ a 在 e 之外的他处可判定观测
	// 数量达 minWitnessObs 且全部健康(边际总量减本对计数,O(1))。
	accountHealthyElsewhere := func(accountID uint64, except model.EpochKey) bool {
		stats, ok := snapshot.Accounts[accountID]
		if !ok {
			return false
		}
		pair := pairKey{AccountID: accountID, Exit: except}
		elsewhereDecidable := stats.N - snapshot.DecidablePairs[pair]
		elsewhereDegraded := stats.Degraded - snapshot.DegradedPairs[pair]
		return elsewhereDecidable >= minWitnessObs && elsewhereDegraded == 0
	}
	// 出口 e 作为反对账号 a 的证人 ⟺ e 对其他账号的可判定观测
	// 数量达 minWitnessObs 且全部健康。
	exitHealthyForOthers := func(key model.EpochKey, exceptAccount uint64) bool {
		stats, ok := snapshot.Exits[key]
		if !ok {
			return false
		}
		pair := pairKey{AccountID: exceptAccount, Exit: key}
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
			pair := pairKey{AccountID: accountID, Exit: key}
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
			pair := pairKey{AccountID: accountID, Exit: key}
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

// CrossValidateWithConfig 用证据局配置执行互证过滤。
func (s *Store) CrossValidate(snapshot Snapshot) Estimate {
	return CrossValidate(snapshot, s.config().MinWitnessObs)
}
