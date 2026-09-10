package registry

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

// AccountEntry 是热缓存中的账号质量状态条目。
type AccountEntry struct {
	State         model.AccountState
	StateSince    time.Time
	CurrentCaseID uint64
}

// ExitEntry 是热缓存中的出口质量状态条目(按 EpochKey 键控)。
type ExitEntry struct {
	State         model.ExitState
	StateSince    time.Time
	CurrentCaseID uint64
}

// cacheSnapshot 是一份不可变的热缓存快照。更新=整体复制后原子置换
// (并发安全优先用不可变快照);读路径无锁。
type cacheSnapshot struct {
	revision  uint64
	versioned bool
	// accounts 只含有行的账号;缺席=ACTIVE(稀疏表示)。
	accounts map[uint64]AccountEntry
	// exitStates 只含有行的出口;缺席=AVAILABLE。
	exitStates map[model.EpochKey]ExitEntry
	// nodeEpoch 节点当前 epoch;缺席=0(未有探测档案)。
	nodeEpoch map[uint64]uint64
	// groupOf 多成员身份组映射;缺席=账号自成一组。
	groupOf      map[uint64]uint64
	groupMembers map[uint64][]uint64
}

func newEmptySnapshot() *cacheSnapshot {
	return &cacheSnapshot{
		accounts:     map[uint64]AccountEntry{},
		exitStates:   map[model.EpochKey]ExitEntry{},
		nodeEpoch:    map[uint64]uint64{},
		groupOf:      map[uint64]uint64{},
		groupMembers: map[uint64][]uint64{},
	}
}

// atomicSnapshot 包装 atomic.Pointer,提供值语义的 Load/Store。
type atomicSnapshot struct {
	ptr atomic.Pointer[cacheSnapshot]
}

func (a *atomicSnapshot) load() *cacheSnapshot {
	if snap := a.ptr.Load(); snap != nil {
		return snap
	}
	return newEmptySnapshot()
}

func (a *atomicSnapshot) store(snap *cacheSnapshot) { a.ptr.Store(snap) }

// clone 复制一份可变快照(写路径专用,转移是低频事件)。
func (c *cacheSnapshot) clone() *cacheSnapshot {
	next := &cacheSnapshot{
		revision: c.revision, versioned: c.versioned,
		accounts:     make(map[uint64]AccountEntry, len(c.accounts)+1),
		exitStates:   make(map[model.EpochKey]ExitEntry, len(c.exitStates)+1),
		nodeEpoch:    make(map[uint64]uint64, len(c.nodeEpoch)+1),
		groupOf:      make(map[uint64]uint64, len(c.groupOf)+1),
		groupMembers: make(map[uint64][]uint64, len(c.groupMembers)+1),
	}
	for k, v := range c.accounts {
		next.accounts[k] = v
	}
	for k, v := range c.exitStates {
		next.exitStates[k] = v
	}
	for k, v := range c.nodeEpoch {
		next.nodeEpoch[k] = v
	}
	for k, v := range c.groupOf {
		next.groupOf[k] = v
	}
	for k, v := range c.groupMembers {
		next.groupMembers[k] = v
	}
	return next
}

// QualityUnschedulableCounts 返回质量轴不可调度计数(court 容量下限
// 消费):账号=状态不可调度数;出口=当前 epoch 不可调度数(直连节点 0
// 不计,历史 epoch 状态不计)。
func (r *Registry) QualityUnschedulableCounts() (accounts, exits int) {
	snap := r.snapshot.load()
	for _, entry := range snap.accounts {
		if !entry.State.Schedulable() {
			accounts++
		}
	}
	for key, entry := range snap.exitStates {
		if key.NodeID != 0 && !entry.State.Schedulable() && key.Epoch == snap.nodeEpoch[key.NodeID] {
			exits++
		}
	}
	return accounts, exits
}

// rebuildCache 启动时从库重建热缓存(I17:库为真相源)。
// 表为空时得到空快照(一切缺省可用)。
func (r *Registry) rebuildCache(ctx context.Context) error {
	snap := newEmptySnapshot()
	var accountRows []qAccountStateModel
	if err := r.db.WithContext(ctx).Find(&accountRows).Error; err != nil {
		return err
	}
	for _, row := range accountRows {
		snap.accounts[row.AccountID] = AccountEntry{
			State:         model.AccountState(row.State),
			StateSince:    row.StateSince,
			CurrentCaseID: row.CurrentCaseID,
		}
	}
	var exitRows []qExitStateModel
	if err := r.db.WithContext(ctx).Find(&exitRows).Error; err != nil {
		return err
	}
	for _, row := range exitRows {
		snap.exitStates[model.EpochKey{NodeID: row.NodeID, Epoch: row.Epoch}] = ExitEntry{
			State:         model.ExitState(row.State),
			StateSince:    row.StateSince,
			CurrentCaseID: row.CurrentCaseID,
		}
	}
	var epochRows []qNodeEpochModel
	if err := r.db.WithContext(ctx).Find(&epochRows).Error; err != nil {
		return err
	}
	for _, row := range epochRows {
		if current, ok := snap.nodeEpoch[row.NodeID]; !ok || row.Epoch > current {
			snap.nodeEpoch[row.NodeID] = row.Epoch
		}
	}
	var groupRows []qIdentityGroupModel
	if err := r.db.WithContext(ctx).Find(&groupRows).Error; err != nil {
		return err
	}
	for _, row := range groupRows {
		snap.groupOf[row.AccountID] = row.GroupID
		snap.groupMembers[row.GroupID] = append(snap.groupMembers[row.GroupID], row.AccountID)
	}
	r.snapshot.store(snap)
	return nil
}
