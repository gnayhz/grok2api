package relational

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestHealthObservationsAreAtomicAndRejectOldBindingAndSuccess(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openTestDatabase(t))
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "atomic-health", Enabled: true, Health: 1, EncryptedProxyURL: "original-binding"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	const failures = 32
	var wg sync.WaitGroup
	for i := 0; i < failures; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, Kind: egress.HealthTransportFailure, Failures: 1, ObservedAt: now})
			if err != nil {
				t.Errorf("failure write: %v", err)
			}
		}()
	}
	wg.Wait()
	stored, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != failures || stored.HealthRevision != failures || stored.Health != 0.05 {
		t.Fatalf("concurrent failures lost: count=%d revision=%d health=%v", stored.FailureCount, stored.HealthRevision, stored.Health)
	}
	if stored.CooldownUntil == nil || !stored.CooldownUntil.Equal(now.Add(8*time.Minute)) {
		t.Fatalf("cooldown %v", stored.CooldownUntil)
	}
	_, err = repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, ExpectedRevision: node.HealthRevision, Kind: egress.HealthSuccess})
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old success accepted: %v", err)
	}
	state, err := repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, ExpectedRevision: stored.HealthRevision, Kind: egress.HealthSuccess})
	if err != nil || state.FailureCount != 0 || state.CooldownUntil != nil || state.LastError != "" {
		t.Fatalf("current success: %+v %v", state, err)
	}
	stored.EncryptedProxyURL = "replacement-binding"
	if _, err = repo.UpdateEgressNodeConfiguration(ctx, stored, func(egress.Node) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err = repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, Kind: egress.HealthTransportFailure})
	if !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old binding accepted: %v", err)
	}
}

func TestHealthObservationPreservesNewerQualityQuarantine(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openTestDatabase(t))
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "quarantine", Enabled: true, Health: 1, EncryptedProxyURL: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	until, now := time.Now().UTC().Add(time.Hour), time.Now().UTC()
	if err := seedLegacyEgressQuality(repo, ctx, node.ID, 0.2, 3, &until, egress.LastErrorExitIPQuality, 1, &now); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []egress.HealthObservationKind{egress.HealthSuccess, egress.HealthTransportFailure, egress.HealthAntiBotRejection} {
		_, err := repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, ExpectedRevision: node.HealthRevision, Kind: kind, ObservedAt: now})
		if kind == egress.HealthSuccess && !errors.Is(err, repository.ErrConflict) {
			t.Fatalf("old success after quarantine: %v", err)
		}
		if kind != egress.HealthSuccess && err != nil {
			t.Fatal(err)
		}
		stored, err := repo.GetEgressNode(ctx, node.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.LastError != egress.LastErrorExitIPQuality || stored.CooldownUntil == nil || !stored.CooldownUntil.Equal(until) {
			t.Fatalf("quarantine lost: %+v", stored.HealthState())
		}
	}
}
