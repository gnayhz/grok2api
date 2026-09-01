package cli

import (
	"fmt"
	"testing"
)

func TestNextGrokTurnIndexMonotonic(t *testing.T) {
	adapter := &Adapter{}
	first := adapter.nextGrokTurnIndex("sess-a")
	second := adapter.nextGrokTurnIndex("sess-a")
	third := adapter.nextGrokTurnIndex("sess-a")
	if first != "1" || second != "2" || third != "3" {
		t.Fatalf("turn index must be monotonic per session: %s %s %s", first, second, third)
	}
	other := adapter.nextGrokTurnIndex("sess-b")
	if other != "1" {
		t.Fatalf("distinct session must start at 1: %s", other)
	}
}

// 轮次号按会话而非账号计数:上游以「会话+轮次」定位缓存检查点,网关侧
// 换号若重置轮次号,同一会话回跳小轮次会导致整段提示缓存失配。此不变
// 式由调用点直接传 PromptCacheKey 保证,这里锁定键语义本身。
func TestNextGrokTurnIndexAccountIndependent(t *testing.T) {
	adapter := &Adapter{}
	if got := adapter.nextGrokTurnIndex("sess-shared"); got != "1" {
		t.Fatalf("首轮应为 1,得到 %s", got)
	}
	// 模拟同一会话换号:键不含账号分量,计数必须延续而不是重置。
	if got := adapter.nextGrokTurnIndex("sess-shared"); got != "2" {
		t.Fatalf("同会话第二轮应为 2,得到 %s", got)
	}
}

func TestNextGrokTurnIndexBounded(t *testing.T) {
	adapter := &Adapter{}
	for i := 0; i < maxTrackedGrokTurns+grokTurnPruneBatch; i++ {
		_ = adapter.nextGrokTurnIndex(fmt.Sprintf("acct:%d", i))
	}
	adapter.turnsMu.Lock()
	size := len(adapter.turns)
	adapter.turnsMu.Unlock()
	if size > maxTrackedGrokTurns {
		t.Fatalf("turn table must stay bounded: %d > %d", size, maxTrackedGrokTurns)
	}
}
