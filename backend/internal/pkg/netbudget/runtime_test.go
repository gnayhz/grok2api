package netbudget

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestAdmissionDeadlinePreservesCapacityCause(t *testing.T) {
	r := New(Limits{Connections: 1, QueueTimeout: time.Second})
	defer r.Close()
	var peer net.Conn
	dials := 0
	dial := func(context.Context, string, string) (net.Conn, error) {
		dials++
		conn, other := net.Pipe()
		peer = other
		return conn, nil
	}
	first, err := r.Dial(context.Background(), dial, "tcp", "unused")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	defer first.Close()
	active := MarkActive(first)
	defer active()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := r.Dial(ctx, dial, "tcp", "unused"); !errors.Is(err, ErrCapacity) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("admission deadline lost local classification or original cause: %v", err)
	}
	if dials != 1 || r.Stats().Waiters != 0 {
		t.Fatalf("admission contacted proxy or retained waiter: dials=%d stats=%+v", dials, r.Stats())
	}
}

func TestSocketAndWaiterBudgetCancellationAndClose(t *testing.T) {
	r := New(Limits{Connections: 2, Dialing: 1, Waiters: 1, QueueTimeout: time.Second})
	defer r.Close()
	var peers []net.Conn
	dial := func(context.Context, string, string) (net.Conn, error) {
		c, p := net.Pipe()
		peers = append(peers, p)
		return c, nil
	}
	defer func() {
		for _, p := range peers {
			_ = p.Close()
		}
	}()
	first, err := r.Dial(context.Background(), dial, "tcp", "unused")
	if err != nil {
		t.Fatal(err)
	}
	release := MarkActive(first)
	second, err := r.Dial(context.Background(), dial, "tcp", "unused")
	if err != nil {
		t.Fatal(err)
	}
	s := r.Stats()
	if s.Connections != 2 || s.ActiveConnections != 1 || s.EstablishingConnections != 1 {
		t.Fatalf("counts %+v", s)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := r.Dial(ctx, dial, "tcp", "unused"); done <- err }()
	deadline := time.Now().Add(time.Second)
	for r.Stats().Waiters != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if _, err := r.Dial(context.Background(), dial, "tcp", "unused"); !errors.Is(err, ErrCapacity) {
		t.Fatalf("queue overflow: %v", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	release()
	if s = r.Stats(); s.IdleConnections != 1 || s.Waiters != 0 {
		t.Fatalf("release %+v", s)
	}
	_ = first.Close()
	_ = first.Close()
	_ = second.Close()
	if s = r.Stats(); s.Connections != 0 {
		t.Fatalf("socket permits leaked %+v", s)
	}
}

func TestDrainCancelsQueuedRequestsButKeepsActiveUntilCompletion(t *testing.T) {
	r := New(Limits{Requests: 1, Waiters: 2})
	defer r.Close()
	active, finish, err := r.BeginRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	queued := make(chan error, 1)
	go func() { _, _, err := r.BeginRequest(context.Background()); queued <- err }()
	deadline := time.Now().Add(time.Second)
	for r.Stats().Waiters != 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	r.BeginDrain()
	select {
	case err := <-queued:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("queue drain: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("queue was not canceled")
	}
	if active.Err() != nil {
		t.Fatal("drain canceled live request")
	}
	finish()
	finish()
	if err := r.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestRepeatedConcurrentSocketBatchesReturnToZero(t *testing.T) {
	r := New(Limits{Connections: 16, Dialing: 4, Waiters: 32})
	defer r.Close()
	for batch := 0; batch < 20; batch++ {
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var peer net.Conn
				c, err := r.Dial(context.Background(), func(context.Context, string, string) (net.Conn, error) { a, b := net.Pipe(); peer = b; return a, nil }, "tcp", "unused")
				if err != nil {
					t.Errorf("dial: %v", err)
					return
				}
				_ = c.Close()
				_ = peer.Close()
			}()
		}
		wg.Wait()
		s := r.Stats()
		if s.Connections != 0 || s.Dialing != 0 || s.Waiters != 0 {
			t.Fatalf("batch %d leaked %+v", batch, s)
		}
	}
}
