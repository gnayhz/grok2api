package egress

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type rejectedRotationReset struct{ *rotationStubRepo }

func (r rejectedRotationReset) UpdateEgressNodeRotationStateForBinding(ctx context.Context, node domain.Node, at *time.Time, attempts int, detail string) error {
	if attempts == 0 {
		return repository.ErrConflict
	}
	return r.rotationStubRepo.UpdateEgressNodeRotationStateForBinding(ctx, node, at, attempts, detail)
}

func TestRotationRejectedCompletionDoesNotNotifySuccess(t *testing.T) {
	for _, lastError := range []string{domain.LastErrorExitIPQuality, domain.LastErrorTransport} {
		t.Run(lastError, func(t *testing.T) {
			node := domain.Node{ID: 3, Enabled: true, ExitIP: "198.51.100.7", LastError: lastError}
			probe := domain.ProbeResult{Status: domain.ProbeStatusHealthy, ExitIP: "203.0.113.9", TestedAt: time.Now()}
			service, repo, _, prober := newRotationTestService(t, node, true, probe)
			service.repository = rejectedRotationReset{repo}
			var log bytes.Buffer
			service.SetRotationLogger(slog.New(slog.NewTextHandler(&log, nil)))
			notifications := 0
			service.SetRotationSuccessObserver(func(context.Context, uint64) { notifications++ })
			service.processRotation(context.Background(), node.ID)
			if prober.calls != 1 || repo.rotationState[1] != 1 {
				t.Fatalf("accepted webhook lost its reserved attempt: probes=%d state=%v", prober.calls, repo.rotationState)
			}
			if notifications != 0 || strings.Contains(log.String(), "egress_rotation_succeeded") {
				t.Fatalf("rejected completion reported success: callbacks=%d log=%s", notifications, log.String())
			}
			if !strings.Contains(log.String(), "egress_rotation_state_failed") {
				t.Fatalf("rejected binding write was silent: %s", log.String())
			}
		})
	}
}

func TestProbeDeadRejectedBudgetResetDoesNotEnqueueRotation(t *testing.T) {
	node := probeDeadTestNode(t, 7, func(node *domain.Node) { node.RotationAttempts = 3 })
	service, repo, _, _ := newProbeDeadTestService(t, node, deadProbeResult())
	service.repository = rejectedRotationReset{repo}
	service.markProbeDead(node, true)
	if rotationQueueLength(service) != 0 || repo.node.RotationAttempts != 3 {
		t.Fatalf("rejected binding reset changed rotation budget or queued work: attempts=%d queue=%d", repo.node.RotationAttempts, rotationQueueLength(service))
	}
}

func TestManualRotationRejectedBudgetResetDoesNotEnqueue(t *testing.T) {
	node := domain.Node{ID: 3, Enabled: true, RotationAttempts: 3}
	service, repo, _, _ := newRotationTestService(t, node, true, domain.ProbeResult{})
	service.repository = rejectedRotationReset{repo}
	if err := service.RotateNode(context.Background(), node.ID); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("manual reset ignored binding rejection: %v", err)
	}
	if rotationQueueLength(service) != 0 || repo.node.RotationAttempts != 3 {
		t.Fatal("rejected manual reset changed rotation state")
	}
}
