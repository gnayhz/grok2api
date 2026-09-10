package settings

import (
	"context"
	"log/slog"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// ApplyTarget installs one consumer's hot configuration. It must be repeatable,
// bounded and honor ctx for any blocking work. A failed attempt may have partial
// effects; it is retried with the complete saved intent, never an inverse update.
type ApplyTarget struct {
	Name  string
	Apply func(context.Context, config.Config) error
}

type ApplyStatus struct {
	Name            string
	AppliedRevision uint64
	Pending         bool
	Error           string
	LastAttemptAt   time.Time
}

// NotificationStatus describes publication by this instance only. "observed"
// means the revision was loaded, not published here. No state is a cluster ack.
type NotificationStatus struct {
	Revision      uint64
	State         string
	Error         string
	LastAttemptAt time.Time
}

// Caller holds updateMu across persistence and all effects. Snapshot readers use
// only mu, so they can observe saved/pending while a consumer is applying.
func (s *Service) runApply(ctx context.Context, cfg config.Config, revision uint64) {
	for i, target := range s.targets {
		s.mu.RLock()
		alreadyApplied := s.applyStates[i].AppliedRevision == revision
		s.mu.RUnlock()
		if alreadyApplied {
			continue
		}
		attemptedAt := time.Now().UTC()
		failure := invokeApply(ctx, target, cfg, revision)
		s.mu.Lock()
		state := &s.applyStates[i]
		state.LastAttemptAt = attemptedAt
		state.Error = failure
		if failure == "" {
			state.AppliedRevision = revision
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, state := range s.applyStates {
		if state.AppliedRevision != revision {
			return
		}
	}
	s.lastAppliedRevision = revision
}

func invokeApply(ctx context.Context, target ApplyTarget, cfg config.Config, revision uint64) (failure string) {
	defer func() {
		if recover() != nil {
			failure = "panic"
			// Callback errors/panics may contain secrets from configuration. Only emit
			// stable categories and target names to both logs and the management API.
			slog.Error("settings_apply_failed", "target", target.Name, "revision", revision, "reason", failure)
		}
	}()
	if ctx.Err() != nil {
		return "cancelled"
	}
	if err := target.Apply(ctx, cfg); err != nil {
		failure = "apply_failed"
		if ctx.Err() != nil {
			failure = "cancelled"
		}
		slog.Error("settings_apply_failed", "target", target.Name, "revision", revision, "reason", failure)
	}
	return failure
}

func (s *Service) applyStatusesLocked() []ApplyStatus {
	result := make([]ApplyStatus, len(s.applyStates))
	copy(result, s.applyStates)
	for i := range result {
		result[i].Pending = result[i].AppliedRevision != s.revision
	}
	return result
}

func (s *Service) markNotificationLocked(revision uint64, localWrite bool) {
	state := "observed"
	if s.notify == nil {
		state = "disabled"
	} else if localWrite {
		state = "pending"
	}
	s.notification = NotificationStatus{Revision: revision, State: state}
}

func (s *Service) publish(ctx context.Context) {
	s.mu.RLock()
	status := s.notification
	s.mu.RUnlock()
	if status.State != "pending" && status.State != "failed" {
		return
	}
	publishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	status.LastAttemptAt = time.Now().UTC()
	status.Error = invokeNotification(publishCtx, s.notify)
	status.State = "published"
	if status.Error != "" {
		status.State = "failed"
		slog.Warn("settings_change_publish_failed", "revision", status.Revision, "reason", status.Error)
	}
	s.mu.Lock()
	s.notification = status
	s.mu.Unlock()
}

func invokeNotification(ctx context.Context, notify func(context.Context) error) (failure string) {
	defer func() {
		if recover() != nil {
			failure = "panic"
		}
	}()
	if err := notify(ctx); err != nil {
		return "publish_failed"
	}
	return ""
}
