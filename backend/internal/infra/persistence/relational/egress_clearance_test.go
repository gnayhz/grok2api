package relational

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestClearancePublicationRejectsOldBindingAndConcurrentSolve(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openTestDatabase(t))
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "clearance", Enabled: true, Health: 1, EncryptedProxyURL: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	update := egress.ClearanceUpdate{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, BindingRevision: node.BindingRevision, ExpectedRevision: node.ClearanceRevision, EncryptedCookie: "first", UserAgent: "first-UA", RefreshedAt: time.Now()}
	if err := repo.ApplyEgressClearance(ctx, update); err != nil {
		t.Fatal(err)
	}
	update.EncryptedCookie = "late"
	if err := repo.ApplyEgressClearance(ctx, update); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("concurrent solve accepted: %v", err)
	}
	stored, _ := repo.GetEgressNode(ctx, node.ID)
	update.ExpectedRevision = stored.ClearanceRevision
	// Even a rename is a new configuration generation. A -> B -> A proxy
	// changes must not let a very old solve pass a ciphertext-only comparison.
	stored.Name = "edited"
	if _, err = repo.UpdateEgressNodeConfiguration(ctx, stored, func(egress.Node) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.ApplyEgressClearance(ctx, update); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old binding generation accepted: %v", err)
	}
	got, _ := repo.GetEgressNode(ctx, node.ID)
	if got.EncryptedCloudflareCookie != "first" {
		t.Fatalf("cookie overwritten %q", got.EncryptedCloudflareCookie)
	}
}

func TestRotationStateRejectsConfigurationChangedDuringWebhook(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openTestDatabase(t))
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "rotation", Enabled: true, Health: 1, EncryptedProxyURL: "proxy", EncryptedRotationURL: "webhook", RotationEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err = repo.UpdateEgressNodeRotationStateForBinding(ctx, node, &now, 1, "in progress"); err != nil {
		t.Fatal(err)
	}
	edited := node
	edited.EncryptedRotationURL = "new-webhook"
	if _, err = repo.UpdateEgressNodeConfiguration(ctx, edited, func(egress.Node) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdateEgressNodeRotationStateForBinding(ctx, node, &now, 0, ""); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("stale rotation result accepted: %v", err)
	}
	stored, _ := repo.GetEgressNode(ctx, node.ID)
	if stored.RotationAttempts != 1 || stored.LastRotationError != "in progress" || stored.EncryptedRotationURL != "new-webhook" {
		t.Fatalf("old completion changed new configuration: %+v", stored)
	}
}

func TestHealthyProbeDoesNotEraseFailureAfterProbeStarted(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openTestDatabase(t))
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "probe", Enabled: true, Health: 1, EncryptedProxyURL: "binding"})
	if err != nil {
		t.Fatal(err)
	}
	observation := egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, Kind: egress.HealthTransportFailure, ObservedAt: time.Now()}
	if _, err = repo.ApplyEgressHealthObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	revision, err := repo.BeginEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ApplyEgressHealthObservation(ctx, observation); err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, egress.ProbeResult{Revision: revision, Status: egress.ProbeStatusHealthy, TestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stored, _ := repo.GetEgressNode(ctx, node.ID)
	if stored.FailureCount != 2 || stored.CooldownUntil == nil || stored.HealthRevision != 2 {
		t.Fatalf("newer failure erased %+v", stored.HealthState())
	}
	revision, err = repo.BeginEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if err = repo.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, egress.ProbeResult{Revision: revision, Status: egress.ProbeStatusHealthy, TestedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	stored, _ = repo.GetEgressNode(ctx, node.ID)
	if stored.FailureCount != 0 || stored.CooldownUntil != nil || stored.HealthRevision != 3 {
		t.Fatalf("current probe did not recover %+v", stored.HealthState())
	}
}

func TestPostgresEgressHealthClearanceAndRuntimeProjection(t *testing.T) {
	dsn := os.Getenv("TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("TEST_POSTGRES_DSN is not configured")
	}
	ctx := context.Background()
	db, err := OpenPostgres(ctx, dsn, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := NewEgressRepository(db)
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: fmt.Sprintf("state-cas-%d", time.Now().UnixNano()), Enabled: true, Health: 1, EncryptedProxyURL: "pg-binding"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.DeleteEgressNode(context.Background(), node.ID) })
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, Kind: egress.HealthTransportFailure, ObservedAt: time.Now()})
			if err != nil {
				t.Errorf("failure: %v", err)
			}
		}()
	}
	wg.Wait()
	state, err := repo.GetRuntimeEgressNode(ctx, node.ID)
	if err != nil || state.FailureCount != 24 || state.HealthRevision != 24 {
		t.Fatalf("atomic failure: %+v %v", state.HealthState(), err)
	}
	if _, err = repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, Kind: egress.HealthSuccess}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old success: %v", err)
	}
	update := egress.ClearanceUpdate{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, EncryptedCookie: "current", UserAgent: "current-UA", RefreshedAt: time.Now()}
	if err = repo.ApplyEgressClearance(ctx, update); err != nil {
		t.Fatal(err)
	}
	if err = repo.ApplyEgressClearance(ctx, update); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old clearance: %v", err)
	}
	state, err = repo.GetRuntimeEgressNode(ctx, node.ID)
	if err != nil || state.ClearanceRevision != 1 || state.LastError != egress.LastErrorTransport {
		t.Fatalf("clearance altered health: %+v %v", state, err)
	}
	state.Name = "edited"
	if _, err = repo.UpdateEgressNodeConfiguration(ctx, state, func(egress.Node) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ApplyEgressHealthObservation(ctx, egress.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, Kind: egress.HealthTransportFailure}); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old binding revision: %v", err)
	}
}
