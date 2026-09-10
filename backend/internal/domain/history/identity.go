package history

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// ReplayPlane describes the actual upstream's continuity contract, independent
// of account selection and network routing. Unknown planes cannot claim history.
type ReplayPlane string

const (
	ReplayPlaneBuild ReplayPlane = "build"
	ReplayPlaneXAI   ReplayPlane = "xai"
)

// ReplayScope preserves existing Build v3 (account-independent) and XAI v2
// (account-scoped) keys. Only explicit session seeds may be passed by callers.
func ReplayScope(seed string, accountID uint64, plane ReplayPlane) string {
	seed = strings.TrimSpace(seed)
	if seed == "" || accountID == 0 {
		return ""
	}
	var source string
	switch plane {
	case ReplayPlaneBuild:
		source = "grok2api:reasoning-replay:v3:" + seed + ":build"
	case ReplayPlaneXAI:
		source = fmt.Sprintf("grok2api:reasoning-replay:v2:%s:%d:xai", seed, accountID)
	default:
		return ""
	}
	return scopeDigest(source)
}

// LegacyReplayScopes retains the current and retired Build account keys for
// journal migration. This is the sole compatibility encoding for legacy v2.
func LegacyReplayScopes(seed string, accountID uint64, plane ReplayPlane, retired []uint64) []string {
	if seed == "" || plane != ReplayPlaneBuild {
		return nil
	}
	ids := append([]uint64{accountID}, retired...)
	seen := make(map[uint64]bool)
	keys := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == 0 || seen[id] {
			continue
		}
		seen[id] = true
		keys = append(keys, scopeDigest(fmt.Sprintf("grok2api:reasoning-replay:v2:%s:%d:build", strings.TrimSpace(seed), id)))
	}
	return keys
}
func scopeDigest(source string) string {
	digest := sha256.Sum256([]byte(source))
	return hex.EncodeToString(digest[:])
}

// ReplayCompatibility contains facts from upstream request conversion. Search
// translated into Anthropic tool history has no matching native opaque lineage;
// M12 keeps that representation stateless without discarding its stored chain.
type ReplayCompatibility struct{ TranslatedSearchHistory bool }

func (c ReplayCompatibility) Seed(explicit string) string {
	if c.TranslatedSearchHistory {
		return ""
	}
	return explicit
}
