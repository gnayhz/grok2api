package egress

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// Expose exactly the application port, without discovering extra methods on
// the concrete SQL implementation. Guarantees must survive this legal adapter.
type declaredOperationsPort struct{ OperationsRepository }
type currentAdminHygieneRace struct {
	*relational.EgressRepository
	admin  *Service
	edited bool
}

func (r *currentAdminHygieneRace) GetEgressOperationsConfig(ctx context.Context) (domain.OperationsConfig, error) {
	snapshot, err := r.EgressRepository.GetEgressOperationsConfig(ctx)
	if err != nil {
		return snapshot, err
	}
	if !r.edited {
		r.edited = true
		direct := RoutingTargetInput{Mode: domain.RoutingTargetDirect}
		if _, err := r.admin.UpdateOperationsConfig(ctx, OperationsConfigInput{ProbeIntervalSeconds: 120, DefaultTarget: &direct}); err != nil {
			return snapshot, err
		}
	}
	return snapshot, nil
}

func TestDeclaredOperationsPortPreservesConcurrentAdministratorRouting(t *testing.T) {
	ctx, service, repo := newFinalReviewHealthFixture(t)
	defer service.Close(ctx)
	proxy := "http://user-{account}:pass@legacy-hygiene.example:8080"
	node, err := service.Create(ctx, Input{Name: "legacy-hygiene-port", Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Delete(ctx, node.ID)
	// Seed a historical fixed template; the concurrent administrator uses the
	// real service to repair it to direct and change the probe interval.
	seedOperationsConfig(ctx, t, repo, domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: node.ID})
	race := &currentAdminHygieneRace{EgressRepository: repo, admin: service}
	operations := declaredOperationsPort{OperationsRepository: race}
	if err := service.enforceRoutingHygieneAfterSync(ctx, operations); err != nil {
		t.Fatal(err)
	}
	config, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !race.edited || config.ProbeIntervalSeconds != 120 || config.DefaultTarget.Mode != domain.RoutingTargetDirect {
		t.Fatalf("declared port overwrote administrator config: %+v", config)
	}
}
