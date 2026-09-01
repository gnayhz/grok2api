package cli

import (
	"fmt"
	"testing"
)

func TestNextGrokTurnIndexMonotonic(t *testing.T) {
	adapter := &Adapter{}
	first := adapter.nextGrokTurnIndex("7:sess-a")
	second := adapter.nextGrokTurnIndex("7:sess-a")
	third := adapter.nextGrokTurnIndex("7:sess-a")
	if first != "1" || second != "2" || third != "3" {
		t.Fatalf("turn index must be monotonic per session: %s %s %s", first, second, third)
	}
	other := adapter.nextGrokTurnIndex("8:sess-b")
	if other != "1" {
		t.Fatalf("distinct session must start at 1: %s", other)
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
