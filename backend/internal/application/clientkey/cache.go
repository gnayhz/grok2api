package clientkey

import (
	"sync"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

const (
	keyTouchInterval       = time.Minute
	touchTrackerMaxEntries = 10000
	keyAuthCacheTTL        = time.Second
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

type authKeyCache struct {
	mu        sync.RWMutex
	byPrefix  map[string]cachedAuthKey
	negatives map[string]time.Time
}

func newAuthKeyCache() *authKeyCache {
	return &authKeyCache{byPrefix: make(map[string]cachedAuthKey), negatives: make(map[string]time.Time)}
}

func (c *authKeyCache) get(prefix string, now time.Time) (clientkeydomain.Key, bool) {
	c.mu.RLock()
	entry, ok := c.byPrefix[prefix]
	c.mu.RUnlock()
	if !ok || !now.Before(entry.expiresAt) {
		if ok {
			c.mu.Lock()
			delete(c.byPrefix, prefix)
			c.mu.Unlock()
		}
		return clientkeydomain.Key{}, false
	}
	value := entry.value
	value.AllowedModels = append([]uint64(nil), entry.value.AllowedModels...)
	return value, true
}

func (c *authKeyCache) put(prefix string, value clientkeydomain.Key, now time.Time) {
	if prefix == "" || value.BillingLimitUSDTicks > 0 {
		return
	}
	value.EncryptedSecret = ""
	value.AllowedModels = append([]uint64(nil), value.AllowedModels...)
	c.mu.Lock()
	defer c.mu.Unlock()
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

// getNegative 报告该前缀在负缓存窗口内(近期确认不存在)。
func (c *authKeyCache) getNegative(prefix string, now time.Time) bool {
	c.mu.RLock()
	until, ok := c.negatives[prefix]
	c.mu.RUnlock()
	return ok && now.Before(until)
}

// putNegative 记录一次"前缀不存在"的查询结果。
func (c *authKeyCache) putNegative(prefix string, now time.Time) {
	if prefix == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
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

// deleteNegative 在创建新 key 时清掉同前缀的负缓存记录。
func (c *authKeyCache) deleteNegative(prefix string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.negatives, prefix)
}

func (c *authKeyCache) deleteID(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	for prefix, entry := range c.byPrefix {
		if _, ok := set[entry.value.ID]; ok {
			delete(c.byPrefix, prefix)
		}
	}
}

func (c *authKeyCache) clear() {
	c.mu.Lock()
	clear(c.byPrefix)
	c.mu.Unlock()
}

// touchTracker 合并非关键的最近使用时间写入。
type touchTracker struct {
	mu          sync.Mutex
	lastTouched map[uint64]time.Time
}

func newTouchTracker() *touchTracker {
	return &touchTracker{lastTouched: make(map[uint64]time.Time)}
}

func (c *touchTracker) deleteID(id uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.lastTouched, id)
}

func (c *touchTracker) deleteIDs(ids []uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.lastTouched, id)
	}
}

func (c *touchTracker) shouldTouch(id uint64, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last := c.lastTouched[id]; !last.IsZero() && now.Sub(last) < keyTouchInterval {
		return false
	}
	c.lastTouched[id] = now
	if len(c.lastTouched) > touchTrackerMaxEntries {
		var oldestID uint64
		var oldest time.Time
		for candidateID, touchedAt := range c.lastTouched {
			if oldestID == 0 || touchedAt.Before(oldest) {
				oldestID = candidateID
				oldest = touchedAt
			}
		}
		delete(c.lastTouched, oldestID)
	}
	return true
}
