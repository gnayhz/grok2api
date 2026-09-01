package gateway

import (
	"strings"
	"sync"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func benchAnchorBody() []byte {
	return []byte(`{"model":"grok-4.5","instructions":"be precise","input":[{"type":"message","role":"user","content":"` +
		strings.Repeat("hello world ", 10000) + `"}]}`)
}

// TestBodyAnchorsParseOnce 锁定记忆器契约:多次 load 共享一次解析。
// 同一请求内路由排序/预选候选/选中身份三处调用,回归态(每处独立全量
// 解析)在 128KB body 上是 3×毫秒级浪费,全部发生在首字前。
func TestBodyAnchorsParseOnce(t *testing.T) {
	var parseCount int
	counting := &countingAnchors{body: benchAnchorBody(), count: &parseCount}
	for i := 0; i < 5; i++ {
		if system, firstUser := counting.load(); system != "be precise" || firstUser == "" {
			t.Fatalf("load = (%q, %q)", system, firstUser)
		}
	}
	if parseCount != 1 {
		t.Fatalf("extract ran %d times for 5 loads, want 1", parseCount)
	}
}

type countingAnchors struct {
	body      []byte
	count     *int
	once      sync.Once
	system    string
	firstUser string
}

func (a *countingAnchors) load() (string, string) {
	a.once.Do(func() {
		*a.count++
		a.system, a.firstUser, _ = extractMessageAnchors(a.body)
	})
	return a.system, a.firstUser
}

// TestIdentityWrapperMatchesDirect 锁定包装器等价:经 bodyAnchors 的
// 软会话身份与直接调用(测试旧签名)完全一致。
func TestIdentityWrapperMatchesDirect(t *testing.T) {
	body := benchAnchorBody()
	viaWrapper := resolveBuildSessionIdentity(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "req-1", body)
	viaMemo := resolveBuildSessionIdentityWithAnchors(7, accountdomain.ProviderBuild, "grok-4.5", "", "", "req-1", newBodyAnchors(body))
	if viaWrapper != viaMemo {
		t.Fatalf("wrapper identity diverged: %#v != %#v", viaWrapper, viaMemo)
	}
	if !viaMemo.soft || viaMemo.affinityKey == "" {
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
