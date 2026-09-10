package clientkey

import (
	"fmt"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

func TestAuthCacheInvalidationFencesBothKindsOfUncachedReads(t *testing.T) {
	for _, kind := range []string{"id", "ids", "prefix", "all"} {
		t.Run(kind, func(t *testing.T) {
			cache := newAuthKeyCache()
			now := time.Now()
			const prefix = "target"
			old := cache.lookup(prefix, now)
			other := clientkeydomain.Key{ID: 2, Prefix: "other"}
			cache.put(other.Prefix, other, old.generation, now)
			switch kind {
			case "id":
				cache.deleteID(1)
			case "ids":
				cache.deleteIDs([]uint64{1})
			case "prefix":
				cache.deletePrefix(prefix)
			case "all":
				cache.clear()
			}
			cache.putNegative(prefix, old.generation, now)
			if cache.lookup(prefix, now).negative {
				t.Fatal("pre-invalidation SQL miss refilled a negative entry")
			}
			cache.put(prefix, clientkeydomain.Key{ID: 1, Prefix: prefix}, old.generation, now)
			if cache.lookup(prefix, now).found {
				t.Fatal("pre-invalidation SQL result refilled positive entry")
			}
			if kind != "all" && !cache.lookup(other.Prefix, now).found {
				t.Fatal("targeted invalidation unnecessarily evicted unrelated cached authorization")
			}
			current := cache.lookup(prefix, now)
			cache.put(prefix, clientkeydomain.Key{ID: 1, Prefix: prefix}, current.generation, now)
			if !cache.lookup(prefix, now).found {
				t.Fatal("new generation did not admit a fresh result")
			}
		})
	}
}

func TestAuthCacheSnapshotIsolationTTLsAndCapacity(t *testing.T) {
	now := time.Now()
	cache := newAuthKeyCache()
	value := clientkeydomain.Key{ID: 1, Prefix: "key", EncryptedSecret: "encrypted-test", ModelScope: clientkeydomain.ModelScopeRestricted, AllowedModels: []uint64{1, 2}}
	read := cache.lookup(value.Prefix, now)
	cache.put(value.Prefix, value, read.generation, now)
	value.AllowedModels[0] = 99
	first := cache.lookup(value.Prefix, now)
	if !first.found || first.value.EncryptedSecret != "" || !first.value.AllowsModel(1) {
		t.Fatal("cache retained secret or caller-owned permission storage")
	}
	first.value.AllowedModels[0] = 98
	if !cache.lookup(value.Prefix, now).value.AllowsModel(1) {
		t.Fatal("cache output allowed caller to mutate subsequent authorization")
	}
	if !cache.lookup(value.Prefix, now.Add(keyAuthCacheTTL-time.Nanosecond)).found || cache.lookup(value.Prefix, now.Add(keyAuthCacheTTL)).found {
		t.Fatal("positive cache TTL changed or boundary is inclusive")
	}
	later := now.Add(keyAuthCacheTTL)
	current := cache.lookup(value.Prefix, later)
	cache.put(value.Prefix, value, current.generation, later)
	if !cache.lookup(value.Prefix, later).found {
		t.Fatal("expired lookup removed a fresh fill")
	}
	cache.putNegative("unknown", read.generation, now)
	if !cache.lookup("unknown", now.Add(keyAuthNegativeTTL-time.Nanosecond)).negative || cache.lookup("unknown", now.Add(keyAuthNegativeTTL)).negative {
		t.Fatal("negative cache TTL changed or boundary is inclusive")
	}
	limited := value
	limited.Prefix, limited.BillingLimitUSDTicks = "finite", 1
	cache.put(limited.Prefix, limited, read.generation, now)
	if cache.lookup(limited.Prefix, now).found {
		t.Fatal("finite-budget identity entered positive cache")
	}
	cache.clear()
	for i := 0; i < keyAuthCacheMaxEntries; i++ {
		prefix := fmt.Sprintf("entry-%d", i)
		cache.byPrefix[prefix] = cachedAuthKey{expiresAt: now.Add(keyAuthCacheTTL)}
		cache.negatives[prefix] = now.Add(keyAuthNegativeTTL)
	}
	current = cache.lookup("new", now)
	cache.put("new", value, current.generation, now)
	cache.putNegative("new-negative", current.generation, now)
	if len(cache.byPrefix) > keyAuthCacheMaxEntries || len(cache.negatives) > keyAuthCacheMaxEntries {
		t.Fatal("cache capacity grew beyond the existing bound")
	}
	cache.deleteID(value.ID)
	if len(cache.negatives) != 0 {
		t.Fatal("ID-only creation notification retained unidentified negative entries")
	}
}
