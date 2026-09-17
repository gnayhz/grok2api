package settings

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func TestApplySnapshotVisibleAndNewerUpdateSerialized(t *testing.T) {
	started, resume := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	}()
	repo := &runtimeSettingsRepositoryStub{}
	var installed atomic.Int64
	service := NewService(runtimeOf(testConfig(t)), time.Time{}, 0, repo, nil, []ApplyTarget{{Name: "consumer", Apply: func(_ context.Context, cfg settingsdomain.Config) error {
		if cfg.Server.MaxConcurrentRequests == 2 {
			close(started)
			<-resume
			return errors.New("temporary failure")
		}
		installed.Store(int64(cfg.Server.MaxConcurrentRequests))
		return nil
	}}})
	input := service.Get().Config
	input.Server.MaxConcurrentRequests = 2
	first := make(chan error, 1)
	go func() { _, err := service.Update(context.Background(), 0, input); first <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("apply not started")
	}
	snapshot := service.Get()
	if snapshot.Revision != 1 || !snapshot.ApplyPending || !snapshot.ApplyTargets[0].Pending {
		t.Fatalf("inflight=%+v", snapshot)
	}
	next := snapshot.Config
	next.Server.MaxConcurrentRequests = 3
	second := make(chan error, 1)
	go func() { _, err := service.Update(context.Background(), 1, next); second <- err }()
	close(resume)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if service.Get().AppliedRevision != 2 || service.Get().ApplyPending || installed.Load() != 3 {
		t.Fatal("old failure overtook newer apply")
	}
}

func TestSavedCancellationAndNotificationFailureRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := [2]int{}
	notificationCalls := 0
	service := NewService(runtimeOf(testConfig(t)), time.Time{}, 0, &runtimeSettingsRepositoryStub{}, func(ctx context.Context) error {
		if ctx.Err() != nil {
			t.Error("saved notification inherited caller cancellation")
		}
		notificationCalls++
		if notificationCalls == 1 {
			panic("private-token")
		}
		return nil
	}, []ApplyTarget{
		{Name: "first", Apply: func(context.Context, settingsdomain.Config) error { calls[0]++; cancel(); return nil }},
		{Name: "second", Apply: func(context.Context, settingsdomain.Config) error {
			calls[1]++
			if calls[1] == 1 {
				return errors.New("private-token")
			}
			return nil
		}},
	})
	saved, err := service.Update(ctx, 0, service.Get().Config)
	if err != nil || !saved.ApplyPending || saved.ApplyTargets[1].Error != "cancelled" || saved.Notification.Error != "panic" {
		t.Fatalf("cancelled save=%+v %v", saved, err)
	}
	if err := service.ReloadPersisted(context.Background()); !errors.Is(err, ErrApplyPending) {
		t.Fatalf("failed retry=%v", err)
	}
	snapshot := service.Get()
	if snapshot.ApplyTargets[1].Error != "apply_failed" || snapshot.Notification.State != "published" {
		t.Fatalf("retry=%+v", snapshot)
	}
	if strings.Contains(snapshot.ApplyTargets[1].Error, "private") {
		t.Fatal("sensitive error leaked")
	}
	snapshot, err = service.Read(context.Background())
	if err != nil || snapshot.ApplyPending || snapshot.AppliedRevision != 1 || calls != [2]int{1, 2} || notificationCalls != 2 {
		t.Fatalf("recovery=%+v err=%v calls=%v", snapshot, err, calls)
	}
}
