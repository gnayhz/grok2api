package relational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func TestEgressOperationsBatchUpdatesEnabledState(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	first := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-enable-first")
	second := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-enable-second")
	second.Enabled = false
	if _, err := nodes.UpdateEgressNode(ctx, second); err != nil {
		t.Fatal(err)
	}

	service := egressapp.NewService(nodes, cipher)
	updated, err := service.UpdateManyEnabled(ctx, []uint64{first.ID, second.ID, first.ID}, false)
	if err != nil || updated != 1 {
		t.Fatalf("disable updated = %d, err = %v", updated, err)
	}
	updated, err = service.UpdateManyEnabled(ctx, []uint64{first.ID, second.ID}, true)
	if err != nil || updated != 2 {
		t.Fatalf("enable updated = %d, err = %v", updated, err)
	}
	for _, id := range []uint64{first.ID, second.ID} {
		stored, err := nodes.GetEgressNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.Enabled {
			t.Fatalf("node %d remained disabled", id)
		}
	}
}

// 批量禁用不得绕过路由目标保护:被配置为固定出口的节点禁用被拒绝。
func TestEgressOperationsBatchDisableRejectsRoutingTarget(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-target")
	other := createHealthyEgressNode(t, ctx, nodes, cipher, "batch-other")
	service := egressapp.NewService(nodes, cipher)
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		DefaultTarget:        &egressapp.RoutingTargetInput{Mode: egress.RoutingTargetNode, NodeID: target.ID},
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := service.UpdateManyEnabled(ctx, []uint64{other.ID, target.ID}, false); !errors.Is(err, egressapp.ErrInvalidInput) {
		t.Fatalf("routing target disable error = %v, want ErrInvalidInput", err)
	}
	for _, id := range []uint64{target.ID, other.ID} {
		stored, err := nodes.GetEgressNode(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.Enabled {
			t.Fatalf("node %d changed despite rejected batch", id)
		}
	}
}

func TestEgressOperationsBatchDeleteRemovesNodes(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	first := createHealthyEgressNode(t, ctx, nodes, cipher, "delete-first")
	second := createHealthyEgressNode(t, ctx, nodes, cipher, "delete-second")

	service := egressapp.NewService(nodes, cipher)
	deleted, err := service.DeleteMany(ctx, []uint64{first.ID, second.ID, first.ID})
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d", deleted)
	}
	for _, id := range []uint64{first.ID, second.ID} {
		if _, err := nodes.GetEgressNode(ctx, id); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("node %d still exists: %v", id, err)
		}
	}
}

func TestEgressOperationsCleanupDeletesOnlyDualStackUnhealthyNodes(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)

	source, err := nodes.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "cleanup-source", Enabled: true, RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	manual := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-manual")
	managed := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-managed")
	managed.SourceID = source.ID
	managed.SourceKey = "managed"
	if managed, err = nodes.UpdateEgressNode(ctx, managed); err != nil {
		t.Fatal(err)
	}
	v4Healthy := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-v4-healthy")
	v6Healthy := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-v6-healthy")
	untested := createHealthyEgressNode(t, ctx, nodes, cipher, "cleanup-untested")

	service := egressapp.NewService(nodes, cipher)

	setEgressProbeFamilies(t, ctx, nodes, manual, egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
	setEgressProbeFamilies(t, ctx, nodes, managed, egress.ProbeStatusUnhealthy, egress.ProbeStatusUnhealthy)
	setEgressProbeFamilies(t, ctx, nodes, v4Healthy, egress.ProbeStatusHealthy, egress.ProbeStatusUnhealthy)
	setEgressProbeFamilies(t, ctx, nodes, v6Healthy, egress.ProbeStatusUnhealthy, egress.ProbeStatusHealthy)
	setEgressProbeFamilies(t, ctx, nodes, untested, egress.ProbeStatusUnknown, egress.ProbeStatusUnknown)

	preview, err := service.PreviewUnhealthyCleanup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if preview.Nodes != 2 || preview.SubscriptionManaged != 1 {
		t.Fatalf("cleanup preview = %#v", preview)
	}
	deleted, err := service.DeleteUnhealthy(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d", deleted)
	}
	for _, id := range []uint64{manual.ID, managed.ID} {
		if _, err := nodes.GetEgressNode(ctx, id); !errors.Is(err, repository.ErrNotFound) {
			t.Fatalf("dual-stack unhealthy node %d still exists: %v", id, err)
		}
	}
	for _, id := range []uint64{v4Healthy.ID, v6Healthy.ID, untested.ID} {
		if _, err := nodes.GetEgressNode(ctx, id); err != nil {
			t.Fatalf("preserved node %d: %v", id, err)
		}
	}
}

func TestEgressOperationsPersistsProbeResult(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "probe")
	cooldown := time.Now().UTC().Add(time.Minute)
	if err := nodes.UpdateEgressNodeHealth(ctx, node.ID, 0.7, 1, &cooldown, egress.LastErrorTransport); err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC().Truncate(time.Millisecond)
	service := egressapp.NewService(nodes, cipher)
	service.SetNodeProber(egressProbeStub{result: egress.ProbeResult{
		Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 42, ExitIP: "1.1.1.1",
		Provider: egress.ProbeProviderCloudflare,
		IPv4:     egress.ProbeFamilyResult{Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 40, ExitIP: "1.1.1.1"},
		IPv6:     egress.ProbeFamilyResult{Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 42, ExitIP: "2606:4700:4700::1111"},
	}})

	result, err := service.TestNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != egress.ProbeStatusHealthy || result.ExitIP != "1.1.1.1" {
		t.Fatalf("probe result = %#v", result)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProbeStatus != egress.ProbeStatusHealthy || stored.ProbeProvider != egress.ProbeProviderCloudflare || stored.ProbeLatencyMS != 42 || stored.ExitIP != "1.1.1.1" || stored.LastProbedAt == nil {
		t.Fatalf("stored probe = %#v", stored)
	}
	if stored.IPv4Probe.ExitIP != "1.1.1.1" || stored.IPv6Probe.ExitIP != "2606:4700:4700::1111" || stored.IPv6Probe.Status != egress.ProbeStatusHealthy {
		t.Fatalf("stored family probes = ipv4:%#v ipv6:%#v", stored.IPv4Probe, stored.IPv6Probe)
	}
	if stored.Health != 1 || stored.FailureCount != 0 || stored.CooldownUntil != nil || stored.LastError != "" {
		t.Fatalf("healthy probe did not recover transport failure: %#v", stored)
	}
	if err := nodes.UpdateEgressNodeHealth(ctx, node.ID, 0.7, 1, &cooldown, "anti-bot rejection"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.TestNode(ctx, node.ID); err != nil {
		t.Fatal(err)
	}
	stored, err = nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Health != 0.7 || stored.FailureCount != 1 || stored.CooldownUntil == nil || stored.LastError != "anti-bot rejection" {
		t.Fatalf("healthy probe cleared a non-transport failure: %#v", stored)
	}
	updatedConfig, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeProvider: egress.ProbeProviderIPInfo, ProbeIntervalSeconds: 900,
	})
	if err != nil || updatedConfig.ProbeProvider != egress.ProbeProviderIPInfo {
		t.Fatalf("updated probe provider = %#v, err=%v", updatedConfig, err)
	}
	stored, err = nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProbeProvider != egress.ProbeProviderCloudflare {
		t.Fatalf("stored result provider changed with future probe configuration: %q", stored.ProbeProvider)
	}
}

func TestEgressOperationsDiscardsProbeAfterProxyConfigurationChanges(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "probe-stale")
	replacementProxy, err := cipher.Encrypt("http://replacement.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC().Truncate(time.Millisecond)
	service := egressapp.NewService(nodes, cipher)
	service.SetNodeProber(mutatingEgressProbeStub{
		repository:  nodes,
		replacement: replacementProxy,
		result: egress.ProbeResult{
			Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 10, ExitIP: "198.51.100.20", Provider: egress.ProbeProviderCloudflare,
			IPv4: egress.ProbeFamilyResult{Status: egress.ProbeStatusHealthy, TestedAt: probedAt, LatencyMS: 10, ExitIP: "198.51.100.20"},
			IPv6: egress.ProbeFamilyResult{Status: egress.ProbeStatusUnhealthy, TestedAt: probedAt, LatencyMS: 10, Error: "代理连接失败"},
		},
	})

	_, err = service.TestNode(ctx, node.ID)
	if !errors.Is(err, egressapp.ErrProbeStale) {
		t.Fatalf("stale probe error = %v", err)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL != replacementProxy || stored.ProbeStatus != egress.ProbeStatusUnknown || stored.ProbeProvider != "" || stored.IPv4Probe.Status != egress.ProbeStatusUnknown {
		t.Fatalf("stale probe overwrote edited node: %#v", stored)
	}
}

func TestEgressOperationsReturnsPersistedUnhealthyProbeAsResult(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	node := createHealthyEgressNode(t, ctx, nodes, cipher, "unreachable")
	service := egressapp.NewService(nodes, cipher)
	service.SetNodeProber(egressProbeStub{err: errors.New("connection refused")})

	result, err := service.TestNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != egress.ProbeStatusUnhealthy || result.Error == "" {
		t.Fatalf("failed probe result = %#v", result)
	}
	stored, err := nodes.GetEgressNode(ctx, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ProbeStatus != egress.ProbeStatusUnhealthy || stored.ProbeError == "" || stored.LastProbedAt == nil {
		t.Fatalf("stored failed probe = %#v", stored)
	}
}

func TestEgressOperationsStoresSubscriptionURLEncrypted(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	service := egressapp.NewService(nodes, cipher)
	url := "https://subscription.example/proxies?token=subscription-token"
	proxyURL := "socks5h://proxy-user:proxy-secret@proxy.example:1080"
	interval := 900
	created, err := service.CreateSource(ctx, egressapp.SubscriptionSourceInput{
		Name: "source", Enabled: true, URL: &url, ProxyURL: &proxyURL,
		RefreshIntervalSeconds: &interval,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created.URLConfigured || !created.ProxyConfigured || created.RefreshIntervalSeconds != interval {
		t.Fatalf("public source = %#v", created)
	}
	stored, err := nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedURL == url || strings.Contains(stored.EncryptedURL, "subscription-token") {
		t.Fatalf("subscription URL stored in plaintext: %q", stored.EncryptedURL)
	}
	if stored.EncryptedProxyURL == proxyURL || strings.Contains(stored.EncryptedProxyURL, "proxy-secret") {
		t.Fatalf("subscription proxy URL stored in plaintext: %q", stored.EncryptedProxyURL)
	}
	originalEncryptedProxyURL := stored.EncryptedProxyURL

	updated, err := service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
		Name: created.Name, Enabled: created.Enabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !updated.ProxyConfigured {
		t.Fatal("omitted proxy update cleared the configured proxy")
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL != originalEncryptedProxyURL {
		t.Fatal("omitted proxy update replaced the encrypted proxy")
	}

	replacementProxyURL := "http://replacement-user:replacement-secret@proxy.example:8080"
	updated, err = service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
		Name: created.Name, Enabled: created.Enabled, ProxyURL: &replacementProxyURL,
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	decryptedProxyURL, err := cipher.Decrypt(stored.EncryptedProxyURL)
	if err != nil {
		t.Fatal(err)
	}
	if decryptedProxyURL != replacementProxyURL || !updated.ProxyConfigured {
		t.Fatalf("replacement proxy = %q, public source = %#v", decryptedProxyURL, updated)
	}

	for _, invalidProxyURL := range []string{"", "socks5h://Default.{account}:secret@proxy.example:1080"} {
		_, updateErr := service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
			Name: created.Name, Enabled: created.Enabled, ProxyURL: &invalidProxyURL,
		})
		if !errors.Is(updateErr, egressapp.ErrInvalidInput) {
			t.Fatalf("invalid proxy %q error = %v", invalidProxyURL, updateErr)
		}
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL == "" {
		t.Fatal("invalid proxy update modified the configured proxy")
	}

	updated, err = service.UpdateSource(ctx, created.ID, egressapp.SubscriptionSourceInput{
		Name: created.Name, Enabled: created.Enabled, ClearProxyURL: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ProxyConfigured {
		t.Fatal("cleared subscription proxy remains publicly configured")
	}
	stored, err = nodes.GetEgressSource(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.EncryptedProxyURL != "" {
		t.Fatal("cleared subscription proxy remains encrypted at rest")
	}
}

func TestEgressOperationsSubscriptionImportCountsOnlyNewNodes(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	source, err := nodes.CreateEgressSource(ctx, egress.SubscriptionSource{
		Name: "count-source", Enabled: true, EncryptedURL: "encrypted",
		RefreshIntervalSeconds: 900,
	})
	if err != nil {
		t.Fatal(err)
	}
	proxy, err := cipher.Encrypt("http://count-source.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	values := []egress.Node{{
		Name: "count-node", Enabled: true, SourceID: source.ID,
		SourceKey: "count-node", EncryptedProxyURL: proxy,
	}}
	firstValues := append(append([]egress.Node(nil), values...), values[0])
	first, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, firstValues)
	if err != nil {
		t.Fatal(err)
	}
	second, err := nodes.UpsertEgressNodesFromSource(ctx, source.ID, values)
	if err != nil {
		t.Fatal(err)
	}
	if first != 1 || second != 0 {
		t.Fatalf("import counts = first %d, second %d", first, second)
	}
}

func TestEgressOperationsListsSourcePagesBySearch(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	service := egressapp.NewService(nodes, egressOperationsCipher(t))
	url := "https://subscription.example/proxies"
	for _, input := range []egressapp.SubscriptionSourceInput{
		{Name: "Alpha Build", Enabled: true, URL: &url},
		{Name: "beta build", Enabled: true, URL: &url},
		{Name: "Alpha Web", Enabled: true, URL: &url},
	} {
		if _, err := service.CreateSource(ctx, input); err != nil {
			t.Fatal(err)
		}
	}

	first, total, err := service.ListSourcePage(ctx, 1, 1, "ALPHA")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(first) != 1 {
		t.Fatalf("first page = %#v, total = %d", first, total)
	}
	second, total, err := service.ListSourcePage(ctx, 2, 1, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(second) != 1 {
		t.Fatalf("second page = %#v, total = %d", second, total)
	}
	if len(first)+len(second) != 2 || first[0].ID == second[0].ID {
		t.Fatalf("pages overlapped: %#v vs %#v", first, second)
	}
	web, total, err := service.ListSourcePage(ctx, 1, 100, "web")
	if err != nil || total != 1 || len(web) != 1 || web[0].Name != "Alpha Web" {
		t.Fatalf("web page = %#v, total = %d, err = %v", web, total, err)
	}
}

// 服务层路由配置语义:Nil 层级保留存量;显式 auto 重置该层级。
func TestEgressOperationsConfigRoutingUpdateSemantics(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "svc-routing-target")
	service := egressapp.NewService(nodes, cipher)

	saved, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		ScopeTargets: map[egress.Scope]egressapp.RoutingTargetInput{
			egress.ScopeBuild: {Mode: egress.RoutingTargetNode, NodeID: target.ID},
			egress.ScopeWeb:   {Mode: egress.RoutingTargetDirect},
		},
		ClassTargets: map[egress.TrafficClass]egressapp.RoutingTargetInput{
			egress.TrafficClassBilling: {Mode: egress.RoutingTargetNode, NodeID: target.ID},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := saved.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("saved build target = %+v", got)
	}
	if got := saved.TargetFor(egress.ScopeWeb, egress.TrafficClassInference); got.Mode != egress.RoutingTargetDirect {
		t.Fatalf("saved web target = %+v", got)
	}
	if got := saved.TargetFor(egress.ScopeConsole, egress.TrafficClassBilling); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("saved billing class target = %+v", got)
	}

	// A sparse update without routing levels keeps the stored configuration.
	kept, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	if kept.ProbeIntervalSeconds != 600 {
		t.Fatalf("probe interval = %d", kept.ProbeIntervalSeconds)
	}
	if got := kept.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetNode || got.NodeID != target.ID {
		t.Fatalf("sparse update dropped build target: %+v", got)
	}
	if got := kept.TargetFor(egress.ScopeConsole, egress.TrafficClassBilling); got.NodeID != target.ID {
		t.Fatalf("sparse update dropped class target: %+v", got)
	}

	// An explicit auto scope target pins that level to the automatic schedule.
	updated, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 600,
		DefaultTarget:        &egressapp.RoutingTargetInput{Mode: egress.RoutingTargetDirect},
		ScopeTargets: map[egress.Scope]egressapp.RoutingTargetInput{
			egress.ScopeBuild: {Mode: egress.RoutingTargetAuto},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := updated.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("auto scope target must resolve to the automatic schedule: %+v", got)
	}
	if got := updated.TargetFor(egress.ScopeWeb, egress.TrafficClassInference); got.Mode != egress.RoutingTargetDirect {
		t.Fatalf("web target must survive: %+v", got)
	}

	// Structurally invalid routing is rejected with the input error.
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 600,
		ScopeTargets: map[egress.Scope]egressapp.RoutingTargetInput{
			// Asset scopes are not independently routable (they follow the
			// parent family), so they must be rejected.
			egress.ScopeWebAsset: {Mode: egress.RoutingTargetDirect},
		},
	}); err == nil {
		t.Fatal("expected validation error for asset-scope routing target")
	}
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 600,
		DefaultTarget:        &egressapp.RoutingTargetInput{Mode: egress.RoutingTargetNode},
	}); err == nil {
		t.Fatal("expected validation error for node target without node id")
	}
}

// 删除固定目标节点后,配置中指向它的目标被清空(各层级逐级下落)。
func TestEgressOperationsDeleteClearsRoutingTargetReferences(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	nodes := NewEgressRepository(database)
	cipher := egressOperationsCipher(t)
	target := createHealthyEgressNode(t, ctx, nodes, cipher, "delete-target")
	service := egressapp.NewService(nodes, cipher)
	if _, err := service.UpdateOperationsConfig(ctx, egressapp.OperationsConfigInput{
		ProbeIntervalSeconds: 900,
		DefaultTarget:        &egressapp.RoutingTargetInput{Mode: egress.RoutingTargetNode, NodeID: target.ID},
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(ctx, target.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := nodes.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := stored.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("deleted target reference = %#v", got)
	}
}

// TargetFor 解析优先级:class → scope(asset 归并父族) → default → auto。
func TestOperationsConfigTargetForHierarchy(t *testing.T) {
	nodeTarget := egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: 1}
	poolTarget := egress.RoutingTarget{Mode: egress.RoutingTargetPool, PoolID: 2}
	config := egress.OperationsConfig{
		DefaultTarget: egress.RoutingTarget{Mode: egress.RoutingTargetDirect},
		ScopeTargets: map[egress.Scope]egress.RoutingTarget{
			egress.ScopeWeb:   nodeTarget,
			egress.ScopeBuild: {Mode: egress.RoutingTargetAuto},
		},
		ClassTargets: map[egress.TrafficClass]egress.RoutingTarget{
			egress.TrafficClassBilling: poolTarget,
		},
	}
	if got := config.TargetFor(egress.ScopeWeb, egress.TrafficClassBilling); got.Mode != egress.RoutingTargetPool {
		t.Fatalf("class must win over scope: %+v", got)
	}
	if got := config.TargetFor(egress.ScopeWeb, egress.TrafficClassInference); got.Mode != egress.RoutingTargetNode || got.NodeID != 1 {
		t.Fatalf("scope target = %+v", got)
	}
	// Asset scopes inherit their parent family's exit.
	if got := config.TargetFor(egress.ScopeWebAsset, egress.TrafficClassInference); got.NodeID != 1 {
		t.Fatalf("asset scope must follow web family: %+v", got)
	}
	if got := config.TargetFor(egress.ScopeConsoleAsset, egress.TrafficClassInference); got.Mode != egress.RoutingTargetDirect {
		t.Fatalf("console asset must follow console → default: %+v", got)
	}
	// Explicit auto scope target pins that level to the automatic schedule
	// even when a default target exists.
	if got := config.TargetFor(egress.ScopeBuild, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("explicit auto scope must stay on the automatic schedule: %+v", got)
	}
	// Unconfigured level without default ends on the automatic schedule.
	empty := egress.OperationsConfig{}
	if got := empty.TargetFor(egress.ScopeWeb, egress.TrafficClassInference); got.Mode != egress.RoutingTargetAuto {
		t.Fatalf("empty config target = %+v", got)
	}
}

// 校验:作用域白名单、目标形状、层级上限。
func TestValidateRoutingTargets(t *testing.T) {
	valid := egress.RoutingTarget{Mode: egress.RoutingTargetNode, NodeID: 1}
	if err := egress.ValidateRoutingTargets(valid, nil, nil); err != nil {
		t.Fatalf("valid default target rejected: %v", err)
	}
	if err := egress.ValidateRoutingTargets(egress.RoutingTarget{Mode: egress.RoutingTargetNode}, nil, nil); err == nil {
		t.Fatal("node target without node id must be rejected")
	}
	if err := egress.ValidateRoutingTargets(egress.RoutingTarget{Mode: egress.RoutingTargetAuto, NodeID: 1}, nil, nil); err == nil {
		t.Fatal("auto target with node id must be rejected")
	}
	if err := egress.ValidateRoutingTargets(valid, map[egress.Scope]egress.RoutingTarget{egress.ScopeWebAsset: {Mode: egress.RoutingTargetDirect}}, nil); err == nil {
		t.Fatal("asset-scope target must be rejected")
	}
	if err := egress.ValidateRoutingTargets(valid, map[egress.Scope]egress.RoutingTarget{egress.Scope("bogus"): {Mode: egress.RoutingTargetDirect}}, nil); err == nil {
		t.Fatal("unknown scope must be rejected")
	}
	if err := egress.ValidateRoutingTargets(valid, nil, map[egress.TrafficClass]egress.RoutingTarget{egress.TrafficClass("bogus"): {Mode: egress.RoutingTargetDirect}}); err == nil {
		t.Fatal("unknown traffic class must be rejected")
	}
	overflow := make(map[egress.TrafficClass]egress.RoutingTarget, egress.MaxRoutingTargets+1)
	for _, class := range append(egress.TrafficClasses(), egress.TrafficClass("extra")) {
		overflow[class] = egress.RoutingTarget{Mode: egress.RoutingTargetDirect}
	}
	if err := egress.ValidateRoutingTargets(valid, nil, overflow); err == nil {
		t.Fatal("per-level size bound must be enforced")
	}
}

// 迁移路径(dropLegacyEgressResourceColumns 删除旧列+捕获旧路由)在 SQLite 上存在
// 运行时缺陷: dropEgressLegacyColumns 以字符串表名调用 glebarez sqlite 的
// Migrator.DropColumn, 该实现对字符串 dst 解析不出 schema 会空指针 panic。
// 故本文件暂不包含需要实际删除旧列的迁移用例, 待非测试代码修复后补充
// (见 schema_egress_scopeless.go dropEgressLegacyColumns)。

type egressProbeStub struct {
	result egress.ProbeResult
	err    error
}

type mutatingEgressProbeStub struct {
	repository  *EgressRepository
	replacement string
	result      egress.ProbeResult
}

func (stub mutatingEgressProbeStub) ProbeEgressNode(ctx context.Context, node egress.Node) (egress.ProbeResult, error) {
	node.EncryptedProxyURL = stub.replacement
	node.ProbeStatus = egress.ProbeStatusUnknown
	node.LastProbedAt = nil
	node.ProbeLatencyMS = 0
	node.ExitIP = ""
	node.ProbeError = ""
	node.ProbeProvider = ""
	node.IPv4Probe = egress.ProbeFamilyResult{Status: egress.ProbeStatusUnknown}
	node.IPv6Probe = egress.ProbeFamilyResult{Status: egress.ProbeStatusUnknown}
	if _, err := stub.repository.UpdateEgressNode(ctx, node); err != nil {
		return egress.ProbeResult{}, err
	}
	return stub.result, nil
}

func (stub egressProbeStub) ProbeEgressNode(context.Context, egress.Node) (egress.ProbeResult, error) {
	return stub.result, stub.err
}

func egressOperationsCipher(t *testing.T) *security.Cipher {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func createHealthyEgressNode(t *testing.T, ctx context.Context, repository *EgressRepository, cipher *security.Cipher, name string) egress.Node {
	t.Helper()
	proxy, err := cipher.Encrypt("http://" + name + ".example:8080")
	if err != nil {
		t.Fatal(err)
	}
	probedAt := time.Now().UTC()
	created, err := repository.CreateEgressNode(ctx, egress.Node{
		Name: name, Enabled: true, EncryptedProxyURL: proxy,
		Health: 1, ProbeStatus: egress.ProbeStatusHealthy, LastProbedAt: &probedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func setEgressProbeFamilies(t *testing.T, ctx context.Context, repository *EgressRepository, node egress.Node, ipv4, ipv6 egress.ProbeStatus) {
	t.Helper()
	now := time.Now().UTC()
	if err := repository.UpdateEgressNodeProbe(ctx, node.ID, node.EncryptedProxyURL, egress.ProbeResult{
		Status: egress.ProbeStatusUnhealthy, TestedAt: now, Error: "probe failed",
		IPv4: egress.ProbeFamilyResult{Status: ipv4, TestedAt: now},
		IPv6: egress.ProbeFamilyResult{Status: ipv6, TestedAt: now},
	}); err != nil {
		t.Fatal(err)
	}
}
