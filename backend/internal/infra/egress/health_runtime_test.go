package egress

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestHealthRuntimeCoalescesBurstAndRejectsOldSuccess(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	var writes atomic.Int32
	stored := domain.HealthState{Health: 1}
	h := newHealthRuntime(func(ctx context.Context, r healthReport) (domain.HealthState, error) {
		if writes.Add(1) == 1 {
			close(entered)
			select {
			case <-gate:
			case <-ctx.Done():
				return domain.HealthState{}, ctx.Err()
			}
		}
		var accepted bool
		stored, accepted = stored.Apply(r.observation)
		if !accepted {
			return stored, errors.New("revision conflict")
		}
		return stored, nil
	}, func() {})
	t.Cleanup(func() { _ = h.close(context.Background()) })
	node := domain.Node{ID: 1, EncryptedProxyURL: "binding", Health: 1}
	report := healthReport{binding: healthBinding{1, "binding", 0}, baseline: node.HealthState(), known: true, scope: domain.ScopeBuild, observation: domain.HealthObservation{NodeID: 1, EncryptedProxyURL: "binding", Kind: domain.HealthTransportFailure, Failures: 1, ObservedAt: time.Now().UTC()}}
	h.submit(report)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("worker did not start")
	}
	for i := 1; i < 1000; i++ {
		h.submit(report)
	}
	oldSuccess := report
	oldSuccess.observation.Kind = domain.HealthSuccess
	h.submit(oldSuccess)
	overlay := h.overlay(node)
	if overlay.FailureCount != 1000 || overlay.CooldownUntil == nil || overlay.HealthRevision != 1000 {
		t.Fatalf("local failure overlay: %+v", overlay.HealthState())
	}
	close(gate)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != 1000 || stored.Revision != 1000 || writes.Load() != 2 {
		t.Fatalf("coalesced writes=%d state=%+v", writes.Load(), stored)
	}
	currentSuccess := oldSuccess
	currentSuccess.baseline = stored
	currentSuccess.observation.ExpectedRevision = stored.Revision
	h.submit(currentSuccess)
	if err := h.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != 0 || stored.CooldownUntil != nil {
		t.Fatalf("current success did not recover: %+v", stored)
	}
}

func TestHealthRuntimeStorageStallHasBoundedQueueAndShutdown(t *testing.T) {
	var active, maxActive atomic.Int32
	storageGate := make(chan struct{})
	var once sync.Once
	releaseStorage := func() { once.Do(func() { close(storageGate) }) }
	defer releaseStorage()
	h := newHealthRuntime(func(ctx context.Context, r healthReport) (domain.HealthState, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := maxActive.Load(); n > old; old = maxActive.Load() {
			if maxActive.CompareAndSwap(old, n) {
				break
			}
		}
		<-ctx.Done()
		// Even an expired write cannot finish until submission has completed.
		// This verifies independence from storage without a CPU-speed assertion.
		<-storageGate
		return domain.HealthState{}, ctx.Err()
	}, func() {})
	t.Cleanup(func() { _ = h.close(context.Background()) })
	submitted := make(chan struct{})
	go func() {
		defer close(submitted)
		for id := uint64(1); id <= 10000; id++ {
			h.submit(healthReport{binding: healthBinding{id, "binding", 0}, known: true, baseline: domain.HealthState{Health: 1}, observation: domain.HealthObservation{NodeID: id, Kind: domain.HealthTransportFailure, Failures: 1, ObservedAt: time.Now().UTC()}})
		}
	}()
	select {
	case <-submitted:
	case <-time.After(10 * time.Second):
		t.Fatal("submission blocked while storage was gated")
	}
	for i := range h.shards {
		s := &h.shards[i]
		s.mu.Lock()
		entries, queued := len(s.entries), len(s.queue)
		s.mu.Unlock()
		if entries > healthEntriesPerShard || queued > healthEntriesPerShard {
			t.Fatalf("shard %d entries=%d queue=%d", i, entries, queued)
		}
	}
	if h.dropped.Load() == 0 {
		t.Fatal("capacity overflow was not observable")
	}
	releaseStorage()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.close(ctx); err != nil {
		t.Fatal(err)
	}
	if active.Load() != 0 || maxActive.Load() > healthShardCount {
		t.Fatalf("workers active=%d peak=%d", active.Load(), maxActive.Load())
	}
}

func TestFinalReviewQueuedFailureKindsPreserveCooldown(t *testing.T) {
	for _, known := range []bool{true, false} {
		for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
			t.Run(fmt.Sprintf("known=%v/order=%v", known, order), func(t *testing.T) {
				entered, gate := make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(gate) }) }
				defer release()
				stored := domain.HealthState{Health: 1}
				h := newHealthRuntime(func(ctx context.Context, r healthReport) (domain.HealthState, error) {
					if r.binding.nodeID == 1 {
						close(entered)
						select {
						case <-gate:
						case <-ctx.Done():
							return domain.HealthState{}, ctx.Err()
						}
						return r.baseline, nil
					}
					stored, _ = stored.Apply(r.observation)
					return stored, nil
				}, func() {})
				t.Cleanup(func() { _ = h.close(context.Background()) })
				// A different binding occupies the same shard, making the entire
				// target burst coalesce before its first persistence attempt.
				h.submit(healthReport{binding: healthBinding{nodeID: 1}, observation: domain.HealthObservation{Kind: domain.HealthAntiBotRejection}})
				select {
				case <-entered:
				case <-time.After(time.Second):
					t.Fatal("worker did not block")
				}
				now := time.Now().UTC()
				until := now.Add(10 * time.Minute)
				observations := []domain.HealthObservation{
					{Kind: domain.HealthTransportFailure, ObservedAt: now.Add(-time.Minute)},
					{Kind: domain.HealthTransportFailure, ObservedAt: now, CooldownUntil: &until},
					{Kind: domain.HealthAntiBotRejection, ObservedAt: now.Add(time.Hour)},
				}
				node := domain.Node{ID: 5, Health: 1, EncryptedProxyURL: "binding"}
				for _, i := range order {
					o := observations[i]
					o.NodeID, o.EncryptedProxyURL = node.ID, node.EncryptedProxyURL
					h.submit(healthReport{binding: healthBinding{nodeID: node.ID, proxy: node.EncryptedProxyURL}, baseline: node.HealthState(), known: known, observation: o})
				}
				assertState := func(s domain.HealthState) {
					t.Helper()
					if s.FailureCount != 3 || s.Revision != 3 || s.LastError != domain.LastErrorTransport || s.CooldownUntil == nil || !s.CooldownUntil.Equal(until) {
						t.Fatalf("mixed failures lost transport cooldown or used rejection timestamp: %+v", s)
					}
				}
				if known {
					assertState(h.overlay(node).HealthState())
				}
				release()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := h.flush(ctx); err != nil {
					t.Fatal(err)
				}
				assertState(stored)
				if known {
					assertState(h.overlay(node).HealthState())
				}
			})
		}
	}
}

func TestFinalReviewPendingFailureOverlayPreservesStoredCooldown(t *testing.T) {
	for _, kind := range []domain.HealthObservationKind{domain.HealthAntiBotRejection, domain.HealthTransportFailure} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			now := time.Now().UTC()
			until := now.Add(10 * time.Minute)
			old := domain.Node{ID: 1, Health: 1, EncryptedProxyURL: "binding"}
			current := old
			current.HealthRevision, current.FailureCount = 1, 1
			current.CooldownUntil, current.LastError = &until, domain.LastErrorTransport
			entered, gate := make(chan struct{}), make(chan struct{})
			var once sync.Once
			release := func() { once.Do(func() { close(gate) }) }
			defer release()
			h := newHealthRuntime(func(ctx context.Context, r healthReport) (domain.HealthState, error) {
				close(entered)
				select {
				case <-gate:
				case <-ctx.Done():
					return domain.HealthState{}, ctx.Err()
				}
				state, _ := current.HealthState().Apply(r.observation)
				return state, nil
			}, func() {})
			t.Cleanup(func() { _ = h.close(context.Background()) })
			h.submit(healthReport{binding: healthBinding{nodeID: 1, proxy: "binding"}, baseline: old.HealthState(), known: true, observation: domain.HealthObservation{Kind: kind, ObservedAt: now.Add(-time.Minute)}})
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("health write did not block")
			}
			state := h.overlay(current).HealthState()
			if state.CooldownUntil == nil || !state.CooldownUntil.Equal(until) || state.LastError != domain.LastErrorTransport {
				t.Fatalf("pending old observation masked persisted cooldown: %+v", state)
			}
			release()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := h.flush(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
