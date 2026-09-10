package egress

import (
	"context"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestImmediateFailureProbesBoundOutageConcurrency(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	gate := make(chan struct{})
	m.SetFailureProber(func(ctx context.Context, _ uint64) (domain.ProbeResult, error) {
		select {
		case <-gate:
			return domain.ProbeResult{}, nil
		case <-ctx.Done():
			return domain.ProbeResult{}, ctx.Err()
		}
	})
	for id := uint64(1); id <= 100; id++ {
		m.scheduleFailureProbe(domain.Node{ID: id})
	}
	m.failureProbeMu.Lock()
	done := make([]<-chan struct{}, 0, len(m.failureProbes))
	for _, state := range m.failureProbes {
		done = append(done, state.done)
	}
	m.failureProbeMu.Unlock()
	close(gate)
	for _, completed := range done {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatal("probe did not finish after release")
		}
	}
	if len(done) != maxConcurrentFailureProbes {
		t.Fatalf("100 failed nodes scheduled %d simultaneous probes, want %d", len(done), maxConcurrentFailureProbes)
	}
}

func TestImmediateFailureProbeCompletionCacheIsBounded(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	t.Cleanup(func() { _ = m.Close(context.Background()) })
	m.SetFailureProber(func(context.Context, uint64) (domain.ProbeResult, error) {
		return domain.ProbeResult{}, nil
	})
	now := time.Now()
	for id := uint64(1); id <= maxCachedFailureProbes; id++ {
		m.failureProbes[id] = failureProbeState{lastCompleted: now}
	}
	// All entries remain inside the grace period: time-based cleanup alone
	// cannot bound a burst of fast completions across distinct nodes.
	newID := uint64(maxCachedFailureProbes + 1)
	m.scheduleFailureProbe(domain.Node{ID: newID})
	m.failureProbeMu.Lock()
	count := len(m.failureProbes)
	done := m.failureProbes[newID].done
	m.failureProbeMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("probe did not complete")
	}
	if count > maxCachedFailureProbes {
		t.Fatalf("completion cache exceeded capacity: %d", count)
	}
}
