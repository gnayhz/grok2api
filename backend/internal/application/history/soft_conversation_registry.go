package history

import (
	"sort"
	"sync"
	"time"
)

// softConversationRegistry 是无会话客户端(软会话)的对话登记表。
//
// 两个需求的交界处需要一点运行时状态:
//   - 同一会话全程热缓存:会话键按「租户+模型+开场白」确定性派生,不含
//     助手回复——第 1 轮与后续每一轮的键都相同,缓存从第 2 轮起直接命中;
//   - 不同会话不串:同租户、相同开场白的两条对话,其助手回复必然不同。
//     登记表把开场白签名与"第一条绑定它的回复签名"关联;当出现不同的
//     回复签名时,判定为另一条对话,改用按该回复确定性派生的分叉键。
//
// 键本身全部确定性派生(无随机量):登记表丢失(进程重启/多副本)时,
// 首条对话的键原样重建;分叉对话在下一次请求时被重新识别并分叉,期间
// 最多出现一次同开场白对话短暂共享基线键——请求内容按各自完整输入
// 计算,共享的只是相同前缀的预填充复用,不会串内容。
const (
	softConversationRegistryCap   = 32768
	softConversationIdleTTL       = 2 * time.Hour
	softConversationSweepInterval = time.Minute
)

// softConversationEntry 一条已登记的对话。openingSig 与 fullSig 两个索引键
// 指向同一 entry;linkedFullSig 记录已绑定该对话的回复签名(空=尚未出现
// 带回复的轮次)。
type softConversationEntry struct {
	convID        string
	linkedFullSig string
	lastSeen      time.Time
}

type softConversationRegistry struct {
	mu        sync.Mutex
	entries   map[string]*softConversationEntry
	lastSweep time.Time
}

// resolveSoftConversation 返回该请求所属对话的会话键。
//   - openingSig:开场白签名(租户+模型+system+首条 user),恒可计算;
//   - fullSig:含首条助手回复的签名;首轮无回复时等于 openingSig;
//   - openingConvID / forkConvID:由调用方确定性派生,前者用于开场白的
//     第一条对话,后者用于同开场白的分叉对话。
func (r *softConversationRegistry) resolveSoftConversation(openingSig, fullSig, openingConvID, forkConvID string) string {
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = make(map[string]*softConversationEntry)
		r.lastSweep = now
	}
	r.sweepLocked(now)
	if entry, ok := r.entries[fullSig]; ok {
		entry.lastSeen = now
		return entry.convID
	}
	if entry, ok := r.entries[openingSig]; ok {
		entry.lastSeen = now
		switch {
		case fullSig == openingSig:
			// 尚无回复的重复请求(如相同问题重复提问):与开场白第一条对话
			// 共享键,内容一致,仅共享前缀预填充。
			return entry.convID
		case entry.linkedFullSig == "":
			// 该对话第一个带回复的轮次:把回复签名绑定到既有对话。
			entry.linkedFullSig = fullSig
			r.entries[fullSig] = entry
			return entry.convID
		default:
			// 同开场白、不同回复:另一条对话,分叉到独立键。
			forked := &softConversationEntry{convID: forkConvID, linkedFullSig: fullSig, lastSeen: now}
			r.entries[fullSig] = forked
			return forkConvID
		}
	}
	created := &softConversationEntry{convID: openingConvID, lastSeen: now}
	r.entries[openingSig] = created
	if fullSig != openingSig {
		created.linkedFullSig = fullSig
		r.entries[fullSig] = created
	}
	return openingConvID
}

// sweepLocked 回收空闲条目并在超限时按最近活跃淘汰(调用方持锁)。
func (r *softConversationRegistry) sweepLocked(now time.Time) {
	if now.Sub(r.lastSweep) < softConversationSweepInterval && len(r.entries) < softConversationRegistryCap {
		return
	}
	r.lastSweep = now
	for key, entry := range r.entries {
		if now.Sub(entry.lastSeen) > softConversationIdleTTL {
			delete(r.entries, key)
		}
	}
	if len(r.entries) < softConversationRegistryCap {
		return
	}
	type aged struct {
		key  string
		seen time.Time
	}
	candidates := make([]aged, 0, len(r.entries))
	for key, entry := range r.entries {
		candidates = append(candidates, aged{key: key, seen: entry.lastSeen})
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].seen.Before(candidates[j].seen) })
	remove := len(candidates) / 4
	if remove < 1 {
		remove = 1
	}
	for _, candidate := range candidates[:remove] {
		delete(r.entries, candidate.key)
	}
}
