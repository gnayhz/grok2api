package relational

import (
	"context"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestRuntimeNodeProjectionRetainsRoutingAndPoolPriority(t *testing.T) {
	ctx := context.Background()
	repo := NewEgressRepository(openTestDatabase(t))
	node, err := repo.CreateEgressNode(ctx, egress.Node{Name: "runtime", Enabled: true, Health: 0.8, HealthRevision: 7, FailureCount: 1, EncryptedProxyURL: "binding", EncryptedRotationURL: "maintenance-only", UserAgent: "UA"})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := repo.CreateEgressPool(ctx, egress.Pool{Name: "priority", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMembers(ctx, pool.ID, []uint64{node.ID}); err != nil {
		t.Fatal(err)
	}
	if err := repo.SetEgressPoolMemberPriority(ctx, pool.ID, node.ID, 3); err != nil {
		t.Fatal(err)
	}
	single, err := repo.GetRuntimeEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	all, err := repo.ListRuntimeEgressNodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	members, err := repo.ListRuntimeEgressPoolNodes(ctx, pool.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || len(members) != 1 || members[0].PoolPriority != 3 {
		t.Fatalf("all=%+v members=%+v", all, members)
	}
	for _, value := range []egress.Node{single, all[0], members[0]} {
		if value.ID != node.ID || value.Name != node.Name || !value.Enabled || value.Health != 0.8 || value.HealthRevision != 7 || value.EncryptedProxyURL != "binding" || value.UserAgent != "UA" {
			t.Fatalf("runtime projection lost routing fields: %+v", value)
		}
		if len(value.PoolIDs) != 0 || value.SourceName != "" || value.EncryptedRotationURL != "" {
			t.Fatalf("runtime loaded administration/maintenance projection: %+v", value)
		}
	}
}
