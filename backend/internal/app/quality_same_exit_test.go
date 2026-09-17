package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

// TestManagerExitIPResolverKnownSameExitUsesResolvedSnapshotOnly 固定组合根
// 适配缝的保守语义:只有快照里两侧都已解析、且按族判等为同一出口时才回答
// "已知同出口";未探活节点、无可比族、零节点 ID 与读取失败一律不排除——
// 该判定只是派发前的建议性预筛,活体按节点核实仍是唯一权威。
func TestManagerExitIPResolverKnownSameExitUsesResolvedSnapshotOnly(t *testing.T) {
	ctx := context.Background()
	db, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "same-exit.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewEgressRepository(db)
	manager := infraegress.NewManagerWithLimits(repo, nil, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(ctx) })
	resolver := managerExitIPResolver{Manager: manager}

	create := func(name string) domain.Node {
		t.Helper()
		node, err := repo.CreateEgressNode(ctx, domain.Node{Name: name, Enabled: true, Health: 1})
		if err != nil {
			t.Fatal(err)
		}
		return node
	}
	probe := func(node domain.Node, ipv4, ipv6 string) {
		t.Helper()
		revision, err := repo.BeginEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL)
		if err != nil {
			t.Fatal(err)
		}
		result := domain.ProbeResult{
			Revision: revision, Status: domain.ProbeStatusHealthy, TestedAt: time.Now().UTC(),
			IPv4: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: ipv4},
			IPv6: domain.ProbeFamilyResult{Status: domain.ProbeStatusHealthy, ExitIP: ipv6},
		}
		if err := repo.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, result); err != nil {
			t.Fatal(err)
		}
	}
	sharedA, sharedB := create("same-exit-a"), create("same-exit-b")
	distinctV6, unprobed := create("same-exit-distinct-v6"), create("same-exit-unprobed")
	onlyV4, onlyV6 := create("same-exit-v4"), create("same-exit-v6")
	probe(sharedA, "198.51.100.10", "2001:db8::1")
	probe(sharedB, "198.51.100.10", "2001:db8::1")
	probe(distinctV6, "198.51.100.10", "2001:db8::2")
	probe(onlyV4, "203.0.113.7", "")
	probe(onlyV6, "", "2001:db8::9")
	manager.InvalidateNodeSnapshots()

	if addrs, err := manager.KnownNodeExitAddrs(ctx); err != nil || len(addrs) != 5 {
		t.Fatalf("known exit snapshot = %+v err=%v, want five resolved nodes", addrs, err)
	}
	if !resolver.KnownSameExit(ctx, sharedA.ID, sharedB.ID) {
		t.Fatal("identical known egress addresses must be excluded as a group")
	}
	if resolver.KnownSameExit(ctx, sharedA.ID, distinctV6.ID) {
		t.Fatal("shared CGNAT IPv4 with distinct IPv6 is not the same exit")
	}
	if resolver.KnownSameExit(ctx, sharedA.ID, unprobed.ID) {
		t.Fatal("a node without any known address must not be excluded")
	}
	if resolver.KnownSameExit(ctx, onlyV4.ID, onlyV6.ID) {
		t.Fatal("families with no comparable address must not be excluded")
	}
	if resolver.KnownSameExit(ctx, 0, sharedA.ID) || resolver.KnownSameExit(ctx, sharedA.ID, 0) {
		t.Fatal("a zero node ID carries no information")
	}
	// A read failure is "unknown": the candidate stays admissible. The
	// snapshot is invalidated first so the failure is observed, not cached.
	manager.InvalidateNodeSnapshots()
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if resolver.KnownSameExit(ctx, sharedA.ID, sharedB.ID) {
		t.Fatal("an unreadable snapshot must exclude nothing")
	}
}
