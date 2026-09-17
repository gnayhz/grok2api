package model

import (
	"testing"
	"time"
)

// TestCrossValidateAllDegradedNoWitness 锚定 I9:全员降智时无
// "他处健康"者,不得个体定罪——证人集合为空,估计退化为无信息。
func TestCrossValidateAllDegradedNoWitness(t *testing.T) {
	snapshot := Snapshot{
		At: time.Now().UTC(),
		Exits: map[EpochKey]ExitStats{
			{NodeID: 1}: {N: 3, Degraded: 3, Accounts: map[uint64]struct{}{1: {}, 2: {}, 3: {}}},
			{NodeID: 2}: {N: 3, Degraded: 3, Accounts: map[uint64]struct{}{1: {}, 2: {}, 3: {}}},
		},
		Accounts: map[uint64]AccountStats{
			1: {N: 2, Degraded: 2, Exits: map[EpochKey]struct{}{{NodeID: 1}: {}, {NodeID: 2}: {}}},
			2: {N: 2, Degraded: 2, Exits: map[EpochKey]struct{}{{NodeID: 1}: {}, {NodeID: 2}: {}}},
			3: {N: 2, Degraded: 2, Exits: map[EpochKey]struct{}{{NodeID: 1}: {}, {NodeID: 2}: {}}},
		},
		DegradedPairs:  map[PairKey]int{},
		DecidablePairs: map[PairKey]int{},
		Decidable:      6,
		Degraded:       6,
	}
	for _, account := range []uint64{1, 2, 3} {
		for _, node := range []uint64{1, 2} {
			pair := PairKey{AccountID: account, Exit: EpochKey{NodeID: node}}
			snapshot.DecidablePairs[pair] = 1
			snapshot.DegradedPairs[pair] = 1
		}
	}
	estimate := CrossValidate(snapshot, 1)
	for key, subject := range estimate.Exits {
		if subject.Witnesses != 0 || subject.N != 0 {
			t.Fatalf("全员降智时出口 %v 不得有个体定罪输入: %+v", key, subject)
		}
	}
	for accountID, subject := range estimate.Accounts {
		if subject.Witnesses != 0 || subject.N != 0 {
			t.Fatalf("全员降智时账号 %d 不得有个体定罪输入: %+v", accountID, subject)
		}
	}
}

// TestCrossValidateDirtyExitIdentified 锚定互证过滤方向性:
// 多数健康账号经脏出口降 → 出口估计高降智率且有证人;
// 脏出口上的降智不连坐健康账号(为出口做证的账号须他处健康)。
func TestCrossValidateDirtyExitIdentified(t *testing.T) {
	// 布局:出口 10 脏(3 账号经它全降);出口 11/12 干净。
	// 账号 1/2/3 在 11/12 上健康 → 有证人资格。
	snapshot := Snapshot{
		At: time.Now().UTC(),
		Exits: map[EpochKey]ExitStats{
			{NodeID: 10}: {N: 3, Degraded: 3, Accounts: map[uint64]struct{}{1: {}, 2: {}, 3: {}}},
			{NodeID: 11}: {N: 3, Degraded: 0, Accounts: map[uint64]struct{}{1: {}, 2: {}, 3: {}}},
			{NodeID: 12}: {N: 3, Degraded: 0, Accounts: map[uint64]struct{}{1: {}, 2: {}, 3: {}}},
		},
		Accounts:       map[uint64]AccountStats{},
		DegradedPairs:  map[PairKey]int{},
		DecidablePairs: map[PairKey]int{},
	}
	addPair := func(account uint64, node uint64, degraded bool) {
		key := EpochKey{NodeID: node}
		pair := PairKey{AccountID: account, Exit: key}
		snapshot.DecidablePairs[pair]++
		if degraded {
			snapshot.DegradedPairs[pair]++
		}
		stats := snapshot.Accounts[account]
		stats.AccountID = account
		if stats.Exits == nil {
			stats.Exits = map[EpochKey]struct{}{}
		}
		stats.Exits[key] = struct{}{}
		stats.N++
		if degraded {
			stats.Degraded++
		}
		snapshot.Accounts[account] = stats
	}
	for _, account := range []uint64{1, 2, 3} {
		addPair(account, 10, true)
		addPair(account, 11, false)
		addPair(account, 12, false)
	}
	estimate := CrossValidate(snapshot, 1)
	dirty := estimate.Exits[EpochKey{NodeID: 10}]
	if dirty.Witnesses != 3 || dirty.N != 3 || dirty.Degraded != 3 || dirty.Rate != 1 {
		t.Fatalf("脏出口估计 = %+v", dirty)
	}
	clean := estimate.Exits[EpochKey{NodeID: 11}]
	if clean.Degraded != 0 || clean.Rate != 0 {
		t.Fatalf("干净出口估计 = %+v", clean)
	}
	// 账号侧:证人出口(11/12 对他人健康)上的观测全健康 → 账号无罪输入。
	for accountID, subject := range estimate.Accounts {
		if subject.Degraded != 0 {
			t.Fatalf("健康账号 %d 不应有降智定罪输入: %+v", accountID, subject)
		}
		if subject.Witnesses < 1 {
			t.Fatalf("健康账号应有证人出口: %+v", subject)
		}
	}
}

// TestDirtyAccountIdentified 反向:单账号跨多出口全降,出口对他账号
// 健康 → 账号估计高降智率。
func TestDirtyAccountIdentified(t *testing.T) {
	snapshot := Snapshot{
		At:             time.Now().UTC(),
		Exits:          map[EpochKey]ExitStats{},
		Accounts:       map[uint64]AccountStats{},
		DegradedPairs:  map[PairKey]int{},
		DecidablePairs: map[PairKey]int{},
	}
	addPair := func(account uint64, node uint64, degraded bool) {
		key := EpochKey{NodeID: node}
		pair := PairKey{AccountID: account, Exit: key}
		snapshot.DecidablePairs[pair]++
		if degraded {
			snapshot.DegradedPairs[pair]++
		}
		exit := snapshot.Exits[key]
		exit.Key = key
		if exit.Accounts == nil {
			exit.Accounts = map[uint64]struct{}{}
		}
		exit.Accounts[account] = struct{}{}
		exit.N++
		if degraded {
			exit.Degraded++
		}
		snapshot.Exits[key] = exit
		acc := snapshot.Accounts[account]
		acc.AccountID = account
		if acc.Exits == nil {
			acc.Exits = map[EpochKey]struct{}{}
		}
		acc.Exits[key] = struct{}{}
		acc.N++
		if degraded {
			acc.Degraded++
		}
		snapshot.Accounts[account] = acc
	}
	// 账号 1 在三个出口全降;账号 2/3/4 各出口健康。
	for _, node := range []uint64{21, 22, 23} {
		addPair(1, node, true)
		addPair(2, node, false)
		addPair(3, node, false)
	}
	estimate := CrossValidate(snapshot, 1)
	suspect := estimate.Accounts[1]
	if suspect.Witnesses < 2 || suspect.Degraded != 3 || suspect.Rate != 1 {
		t.Fatalf("脏账号估计 = %+v", suspect)
	}
	healthy := estimate.Accounts[2]
	if healthy.Degraded != 0 {
		t.Fatalf("健康账号被冤枉: %+v", healthy)
	}
}
