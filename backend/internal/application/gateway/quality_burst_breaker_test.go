package gateway

import (
	"testing"
	"time"
)

func TestTerminalBurstSignature(t *testing.T) {
	first := int64(20362)
	if !terminalBurstSignature(&first, 20362, 0, 339) {
		t.Fatal("incident row shape must match")
	}
	early := int64(5000)
	if terminalBurstSignature(&early, 20362, 0, 339) {
		t.Fatal("incremental stream (first<dur) must not match")
	}
	if terminalBurstSignature(&first, 20362, 13, 339) {
		t.Fatal("reasoning evidence must not match")
	}
	if terminalBurstSignature(&first, 20362, 0, 8) {
		t.Fatal("tiny output must not match")
	}
	if terminalBurstSignature(nil, 20362, 0, 339) {
		t.Fatal("missing first token must not match")
	}
	if terminalBurstSignature(&first, 0, 0, 339) {
		t.Fatal("missing duration must not match")
	}
}

func TestTerminalBurstTrackerCountsWindowAndReset(t *testing.T) {
	tracker := newTerminalBurstTracker()
	if got := tracker.observeBurst(7); got != 1 {
		t.Fatalf("first burst count = %d", got)
	}
	if got := tracker.observeBurst(7); got != 2 {
		t.Fatalf("second burst count = %d", got)
	}
	tracker.observeHealthy(7)
	if got := tracker.observeBurst(7); got != 1 {
		t.Fatalf("healthy must reset the streak, got %d", got)
	}
	tracker.mu.Lock()
	if entry, ok := tracker.entries[7]; ok {
		entry.lastSeen = time.Now().Add(-terminalBurstWindow - time.Minute)
	}
	tracker.mu.Unlock()
	if got := tracker.observeBurst(7); got != 1 {
		t.Fatalf("stale streak must not carry over, got %d", got)
	}
	tracker.reset(7)
	if got := tracker.observeBurst(7); got != 1 {
		t.Fatalf("reset must clear the streak, got %d", got)
	}
	if got := tracker.observeBurst(0); got != 0 {
		t.Fatalf("zero account = %d", got)
	}
	var nilTracker *terminalBurstTracker
	nilTracker.observeBurst(1)
	nilTracker.observeHealthy(1)
	nilTracker.reset(1)
}

func TestTerminalBurstTrackerDropsExpiredAccounts(t *testing.T) {
	tracker := newTerminalBurstTracker()
	if tracker.observeBurst(1) != 1 || tracker.observeBurst(2) != 1 {
		t.Fatal("seed bursts")
	}
	tracker.mu.Lock()
	tracker.entries[1].lastSeen = time.Now().Add(-terminalBurstWindow - time.Minute)
	tracker.mu.Unlock()
	if got := tracker.observeBurst(2); got != 2 {
		t.Fatalf("fresh account streak = %d", got)
	}
	tracker.mu.Lock()
	_, stale := tracker.entries[1]
	tracker.mu.Unlock()
	if stale {
		t.Fatal("expired account must be dropped from the tracker")
	}
}
