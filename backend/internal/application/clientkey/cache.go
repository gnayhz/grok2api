package clientkey

import (
	"sync"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

const (
	// 正缓存 TTL:本进程的 Key 变更经仓储事件同步清缓存(即时),跨副本经
	// 失效总线;TTL 只是总线降级(如 Redis 不可用)时的兜底。3s 让 DB 短暂
	// 抖动不再放大为鉴权 503 风暴(此前 1s 内缓存全过期,抖动期所有 /v1
	// 请求直落 DB 并失败),代价是降级窗口内禁用 Key 的生效延迟同延。
	keyAuthCacheTTL        = 3 * time.Second
	keyAuthCacheMaxEntries = 10000
	// 未知前缀的负缓存 TTL:伪造 key 的每个请求原本都会打一次 GetByPrefix
	// DB 查询(正缓存只存命中), 无凭据流量可借此放大数据库压力。短 TTL 让
	// "先失败、随后创建同前缀 key"的窗口几乎不可察觉(前缀为随机 6 字节 hex,
	// 碰撞概率本身可忽略; Create 仍会显式失效)。
	keyAuthNegativeTTL = 2 * time.Second
)

type cachedAuthKey struct {
	value     clientkeydomain.Key
	expiresAt time.Time
}

type authCacheLookup struct {
	value      clientkeydomain.Key
	found      bool
	negative   bool
	generation uint64
}

type authKeyCache struct {
	mu sync.RWMutex
	// A generation is local cache ordering, not a durable authorization version.
	// It also fences reads whose key ID is not known until SQL returns.
	generation uint64
	byPrefix   map[string]cachedAuthKey
	negatives  map[string]time.Time
}

func newAuthKeyCache() *authKeyCache {
	return &authKeyCache{byPrefix: make(map[string]cachedAuthKey), negatives: make(map[string]time.Time)}
}

// lookup captures the cached fact and the generation before any SQL starts.
// Expired entries are reclaimed by bounded fills, not a delayed delete that
// could remove a newer value inserted after this read lock is released.
func (c *authKeyCache) lookup(prefix string, now time.Time) authCacheLookup {
	c.mu.RLock()
	result := authCacheLookup{generation: c.generation}
	if until, ok := c.negatives[prefix]; ok && now.Before(until) {
		result.negative = true
	} else if entry, ok := c.byPrefix[prefix]; ok && now.Before(entry.expiresAt) {
		result.value, result.found = entry.value, true
	}
	c.mu.RUnlock()
	if result.found {
		result.value.AllowedModels = append([]uint64(nil), result.value.AllowedModels...)
	}
	return result
}

func (c *authKeyCache) put(prefix string, value clientkeydomain.Key, generation uint64, now time.Time) {
	if prefix == "" || value.BillingLimitUSDTicks > 0 {
		return
	}
	value.EncryptedSecret = ""
	value.AllowedModels = append([]uint64(nil), value.AllowedModels...)
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	delete(c.negatives, prefix)
	c.byPrefix[prefix] = cachedAuthKey{value: value, expiresAt: now.Add(keyAuthCacheTTL)}
	if len(c.byPrefix) <= keyAuthCacheMaxEntries {
		return
	}
	for candidate, entry := range c.byPrefix {
		if !now.Before(entry.expiresAt) {
			delete(c.byPrefix, candidate)
		}
	}
	for len(c.byPrefix) > keyAuthCacheMaxEntries {
		for candidate := range c.byPrefix {
			delete(c.byPrefix, candidate)
			break
		}
	}
}

// putNegative 记录一次"前缀不存在"的查询结果。
func (c *authKeyCache) putNegative(prefix string, generation uint64, now time.Time) {
	if prefix == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if generation != c.generation {
		return
	}
	c.negatives[prefix] = now.Add(keyAuthNegativeTTL)
	for candidate, until := range c.negatives {
		if !now.Before(until) {
			delete(c.negatives, candidate)
		}
	}
	for len(c.negatives) > keyAuthCacheMaxEntries {
		for candidate := range c.negatives {
			delete(c.negatives, candidate)
			break
		}
	}
}

func (c *authKeyCache) deleteID(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	// Unknown-prefix entries carry no key ID. A committed creation or restored
	// identity must invalidate them without publishing credential metadata.
	clear(c.negatives)
	for prefix, entry := range c.byPrefix {
		if entry.value.ID == id {
			delete(c.byPrefix, prefix)
		}
	}
}

func (c *authKeyCache) deleteIDs(ids []uint64) {
	set := make(map[uint64]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	clear(c.negatives)
	for prefix, entry := range c.byPrefix {
		if _, ok := set[entry.value.ID]; ok {
			delete(c.byPrefix, prefix)
		}
	}
}

func (c *authKeyCache) deletePrefix(prefix string) {
	if prefix == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.generation++
	delete(c.byPrefix, prefix)
	delete(c.negatives, prefix)
}

func (c *authKeyCache) clear() {
	c.mu.Lock()
	c.generation++
	clear(c.byPrefix)
	clear(c.negatives)
	c.mu.Unlock()
}
