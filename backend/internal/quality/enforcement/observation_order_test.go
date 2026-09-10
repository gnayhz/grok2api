package enforcement

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

type controlledIPObservation struct {
	ip           string
	revision     uint64
	read, resume chan struct{}
}

func (s controlledIPObservation) CurrentExitIP(context.Context, uint64) (string, uint64, bool, error) {
	if s.read != nil {
		close(s.read)
		<-s.resume
	}
	return s.ip, s.revision, true, nil
}

func TestPollEpochsRejectsDelayedObservation(t *testing.T) {
	r, _ := newBench(t)
	ctx := context.Background()
	if _, _, _, err := r.ObserveExitIP(ctx, 5, "192.0.2.1", 1); err != nil {
		t.Fatal(err)
	}
	nodes := memNodes{profiles: []proxy.NodeProfile{{ID: 5, Enabled: true}}}
	a := New(DefaultConfig(), r, nil, nil, nil)
	b := New(DefaultConfig(), r, nil, nil, nil)
	old := controlledIPObservation{ip: "192.0.2.1", revision: 1, read: make(chan struct{}), resume: make(chan struct{})}
	a.nodes, a.ipSource = nodes, old
	b.nodes, b.ipSource = nodes, controlledIPObservation{ip: "192.0.2.2", revision: 2}
	done := make(chan error, 1)
	go func() {
		changes, err := a.PollEpochs(ctx)
		if len(changes) != 0 {
			t.Error("old observation emitted an epoch change")
		}
		done <- err
	}()
	<-old.read
	defer func() {
		close(old.resume)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	if _, err := b.PollEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	id, err := r.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: 5, Epoch: 1}, time.Now(), `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, id, model.VerdictExitGuilty, `{}`, time.Now(), true); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if allowed, err := r.ExitAllowed(ctx, 5); err != nil || allowed {
			t.Errorf("delayed poll released ban: %v %v", allowed, err)
		}
	})
}

type failingIPObservation func(context.Context, uint64) (string, uint64, bool, error)

func (f failingIPObservation) CurrentExitIP(ctx context.Context, id uint64) (string, uint64, bool, error) {
	return f(ctx, id)
}

func TestPollEpochsKeepsCommittedPrefixOnSourceFailure(t *testing.T) {
	r, s := newBench(t)
	ctx := context.Background()
	for _, id := range []uint64{5, 6} {
		if _, _, _, err := r.ObserveExitIP(ctx, id, "192.0.2.1", 1); err != nil {
			t.Fatal(err)
		}
	}
	fault := errors.New("node facts read unavailable")
	fail := true
	s.nodes = memNodes{profiles: []proxy.NodeProfile{{ID: 5, Enabled: true}, {ID: 6, Enabled: true}}}
	s.ipSource = failingIPObservation(func(_ context.Context, id uint64) (string, uint64, bool, error) {
		if id == 6 && fail {
			return "", 0, false, fault
		}
		return "192.0.2.2", 2, true, nil
	})
	changes, err := s.PollEpochs(ctx)
	if !errors.Is(err, fault) || len(changes) != 1 || changes[0].NodeID != 5 {
		t.Fatalf("lost committed prefix/error: %+v %v", changes, err)
	}
	if r.CurrentEpoch(5) != 1 || r.CurrentEpoch(6) != 0 {
		t.Fatal("unknown observation mutated an epoch")
	}
	fail = false
	changes, err = s.PollEpochs(ctx)
	if err != nil || len(changes) != 1 || changes[0].NodeID != 6 {
		t.Fatalf("retry lost/duplicated changes: %+v %v", changes, err)
	}
}
