package history

import (
	"encoding/json"
	"testing"
)

// TestSoftAnchorDelimiterAmbiguity：v4 长度前缀编码后，移位对
// （system="a:b"+user="c" vs system="a"+user="b:c"）必须推导出
// 不同 AffinityKey——v3 裸拼接下它们坍缩（round 71 PoC 证实），导致
// 同一 client key 下两个对话共享账号亲和。upstream 会话始终按
// requestScope 隔离（两版皆然）。
func TestSoftAnchorDelimiterAmbiguity(t *testing.T) {
	resolver := &IdentityResolver{}
	bodyA, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "a:b"},
		map[string]any{"role": "user", "content": "c"},
	}})
	bodyB, _ := json.Marshal(map[string]any{"messages": []any{
		map[string]any{"role": "system", "content": "a"},
		map[string]any{"role": "user", "content": "b:c"},
	}})
	identityA := resolver.resolveTestIdentity(7, "grok_build", "grok-4.6", "", "", "scope-a", bodyA)
	identityB := resolver.resolveTestIdentity(7, "grok_build", "grok-4.6", "", "", "scope-b", bodyB)
	if identityA.AffinityKey == identityB.AffinityKey {
		t.Fatal("shift-pair (system,user) anchors must not collapse onto one AffinityKey")
	}
	if identityA.UpstreamID == identityB.UpstreamID {
		t.Fatal("upstream session must stay request-scoped even under anchor shifts")
	}
}
