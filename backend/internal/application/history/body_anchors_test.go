package history

import (
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func benchAnchorBody() []byte {
	return []byte(`{"model":"grok-4.5","instructions":"be precise","input":[{"type":"message","role":"user","content":"` +
		strings.Repeat("hello world ", 10000) + `"}]}`)
}

// The real request memo retains its first parse across repeated and concurrent reads.
func TestBodyAnchorsParseOnce(t *testing.T) {
	anchors := newBodyAnchors(benchAnchorBody())
	system, user, assistant := anchors.load()
	if system != "be precise" || user == "" {
		t.Fatal("missing anchors")
	}
	anchors.body = []byte(`{"input":"replacement"}`)
	for i := 0; i < 5; i++ {
		a, b, c := anchors.load()
		if a != system || b != user || c != assistant {
			t.Fatal("request reparsed")
		}
	}
}

// TestIdentityWrapperMatchesDirect 锁定包装器等价:经 bodyAnchors 的
// 软会话身份与直接调用(测试旧签名)完全一致。
func TestIdentityWrapperMatchesDirect(t *testing.T) {
	resolver := &IdentityResolver{}
	body := benchAnchorBody()
	viaWrapper := resolver.resolveTestIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "req-1", body)
	viaMemo := resolver.resolveTestIdentityWithAnchors(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "req-1", newBodyAnchors(body))
	if viaWrapper != viaMemo {
		t.Fatalf("wrapper identity diverged: %#v != %#v", viaWrapper, viaMemo)
	}
	if !viaMemo.Soft || viaMemo.AffinityKey == "" {
		t.Fatalf("soft identity missing: %#v", viaMemo)
	}
}

// BenchmarkAnchorsMemoVsRepeat 量化:同一 body 三处使用(路由排序+预选+选中)
// 的锚点成本。memo 路径 1 次解析;回归态(独立解析)3 次。
func BenchmarkAnchorsMemoVsRepeat(b *testing.B) {
	body := benchAnchorBody()
	b.Run("memo-3-loads", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			anchors := newBodyAnchors(body)
			anchors.load()
			anchors.load()
			anchors.load()
		}
	})
	b.Run("repeat-3-parses", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			extractMessageAnchors(body)
			extractMessageAnchors(body)
			extractMessageAnchors(body)
		}
	})
}
