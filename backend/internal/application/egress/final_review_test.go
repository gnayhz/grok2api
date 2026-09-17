package egress

import (
	"context"
	"errors"
	netbudget "github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// Final-audit regressions exercise the full observation/persistence path.
func TestFinalReviewLate403PreservesNewerTransportCooldown(t *testing.T) {
	ctx, service, repo := newFinalReviewHealthFixture(t)
	defer service.Close(context.Background())
	proxy := "http://127.0.0.1:1"
	node, err := service.Create(ctx, Input{Name: "mixed-health-audit", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.DeleteEgressNode(context.Background(), node.ID) })
	m := infraegress.NewManagerWithLimits(repo, service.cipher, netbudget.Limits{})
	defer m.Close(context.Background())
	old, err := m.Acquire(ctx, domain.ScopeWeb, "old-request")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	if old.NodeID != node.ID {
		t.Fatalf("unexpected route: %d", old.NodeID)
	}
	until := time.Now().UTC().Add(10 * time.Minute)
	if err := m.CooldownNodeForProbeFailure(ctx, node.ID, until); err != nil {
		t.Fatal(err)
	}
	before, _ := repo.GetEgressNode(ctx, node.ID)
	old.Observe(http.StatusForbidden, nil)
	if err := m.FlushFeedback(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CooldownUntil == nil || after.CooldownUntil.Before(until.Add(-time.Millisecond)) || after.LastError != domain.LastErrorTransport {
		t.Fatalf("late 403 erased newer dead-exit cooldown: before=%+v after=%+v", before.HealthState(), after.HealthState())
	}
}

func TestFinalReviewDelayedFailureDoesNotShortenConfirmedDeadCooldown(t *testing.T) {
	ctx, service, repo := newFinalReviewHealthFixture(t)
	defer service.Close(context.Background())
	proxy := "http://127.0.0.1:1"
	created, err := service.Create(ctx, Input{Name: "old-failure-audit", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	node, err := repo.GetEgressNode(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.DeleteEgressNode(context.Background(), node.ID) })
	m := infraegress.NewManagerWithLimits(repo, service.cipher, netbudget.Limits{})
	defer m.Close(context.Background())
	oldTime := time.Now().UTC().Add(-time.Minute)
	until := time.Now().UTC().Add(10 * time.Minute)
	if err := m.CooldownNodeForProbeFailure(ctx, node.ID, until); err != nil {
		t.Fatal(err)
	}
	after, err := repo.ApplyEgressHealthObservation(ctx, domain.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, BindingRevision: node.BindingRevision, Kind: domain.HealthTransportFailure, ObservedAt: oldTime})
	if err != nil {
		t.Fatal(err)
	}
	if after.CooldownUntil == nil || after.CooldownUntil.Before(until.Add(-time.Millisecond)) {
		t.Fatalf("delayed failure shortened confirmed dead cooldown: want>=%v got=%+v", until, after)
	}
}

func newFinalReviewHealthFixture(t *testing.T) (context.Context, *Service, *relational.EgressRepository) {
	t.Helper()
	dsn := os.Getenv("TEST_EGRESS_FINAL_POSTGRES_DSN")
	if dsn == "" {
		return newPoolServiceFixture(t)
	}
	ctx := context.Background()
	db, err := relational.OpenPostgres(ctx, dsn, 8, 2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err = db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(db)
	return ctx, NewService(repo, newRotationCipher(t)), repo
}

func TestFinalReviewConcurrentFailuresPreserveCooldownAndRecovery(t *testing.T) {
	ctx, service, repo := newFinalReviewHealthFixture(t)
	defer service.Close(context.Background())
	proxy := "http://127.0.0.1:1"
	created, err := service.Create(ctx, Input{Name: "concurrent-cooldown", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.DeleteEgressNode(context.Background(), created.ID) })
	node, err := repo.GetEgressNode(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	until, shorter := now.Add(10*time.Minute), now.Add(time.Minute)
	base := domain.HealthObservation{NodeID: node.ID, EncryptedProxyURL: node.EncryptedProxyURL, BindingRevision: node.BindingRevision, Kind: domain.HealthTransportFailure, ObservedAt: now, CooldownUntil: &until}
	if _, err = repo.ApplyEgressHealthObservation(ctx, base); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			o := base
			o.CooldownUntil, o.ObservedAt = nil, now.Add(-time.Minute)
			switch i % 3 {
			case 0:
				o.Kind = domain.HealthAntiBotRejection
			case 1:
				o.CooldownUntil = &shorter
			}
			state, err := repo.ApplyEgressHealthObservation(ctx, o)
			if err != nil {
				t.Errorf("concurrent observation: %v", err)
				return
			}
			if state.LastError != domain.LastErrorTransport || state.CooldownUntil == nil || state.CooldownUntil.Before(until.Add(-time.Millisecond)) {
				t.Errorf("concurrent observation shortened cooldown: %+v", state)
			}
		}()
	}
	group.Wait()
	stored, err := repo.GetEgressNode(ctx, node.ID)
	if err != nil || stored.FailureCount != 33 || stored.HealthRevision != 33 {
		t.Fatalf("failure accounting: %+v %v", stored.HealthState(), err)
	}
	longer := until.Add(time.Minute)
	base.CooldownUntil = &longer
	state, err := repo.ApplyEgressHealthObservation(ctx, base)
	if err != nil || state.CooldownUntil == nil || state.CooldownUntil.Before(longer.Add(-time.Millisecond)) {
		t.Fatalf("new failure did not extend cooldown: %+v %v", state, err)
	}
	base.Kind, base.CooldownUntil = domain.HealthSuccess, nil
	if _, err = repo.ApplyEgressHealthObservation(ctx, base); !errors.Is(err, repository.ErrConflict) {
		t.Fatalf("old success recovered a newer cooldown: %v", err)
	}
	base.ExpectedRevision = state.Revision
	state, err = repo.ApplyEgressHealthObservation(ctx, base)
	if err != nil || state.CooldownUntil != nil || state.LastError != "" || state.FailureCount != 0 {
		t.Fatalf("current success could not recover: %+v %v", state, err)
	}
}
