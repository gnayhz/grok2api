package egress

import (
	"context"
	"errors"
	netfetch "github.com/chenyme/grok2api/backend/internal/testsupport/netfetch"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type hygieneReadFailureRepository struct {
	*relational.EgressRepository
	failure error
	reads   int
	failID  uint64
}

func TestSyncHygieneMissingTargetRemainsRemovable(t *testing.T) {
	stub := &routingHygieneStub{
		config:  domain.OperationsConfig{DefaultTarget: domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: 7}},
		nodeErr: repository.ErrNotFound,
	}
	service := &Service{repository: stub}
	service.SetSubscriptionFetcher(netfetch.NewEgressSubscriptionFetcher(nil, NormalizeSubscriptionURL))
	if err := service.enforceRoutingHygieneAfterSync(context.Background(), stub); err != nil {
		t.Fatal(err)
	}
	if stub.saveCalls != 1 || stub.saved[0].DefaultTarget.Configured() {
		t.Fatalf("proven missing target was retained: %+v", stub.saved)
	}
}

func (r *hygieneReadFailureRepository) GetEgressNode(ctx context.Context, id uint64) (domain.Node, error) {
	r.reads++
	if r.failure != nil && (r.failID == 0 || r.failID == id) {
		return domain.Node{}, r.failure
	}
	return r.EgressRepository.GetEgressNode(ctx, id)
}

func TestSyncHygieneReadFailurePreservesRouting(t *testing.T) {
	for _, level := range []string{"default", "scope", "class"} {
		for _, failure := range []error{errors.New("injected node lookup failure"), context.Canceled} {
			t.Run(level+"/"+failure.Error(), func(t *testing.T) { assertSyncHygieneReadFailure(t, level, failure) })
		}
	}
}

func assertSyncHygieneReadFailure(t *testing.T, level string, failure error) {
	ctx, original, repo := newFinalReviewHealthFixture(t)
	defer original.Close(ctx)
	proxy := "http://fixed.example:8080"
	node, err := original.Create(ctx, Input{Name: "fixed-target-" + level + failure.Error(), Enabled: true, ProxyURL: &proxy})
	if err != nil {
		t.Fatal(err)
	}
	target := domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: node.ID}
	templateProxy := "http://user-{account}:pass@template.example:8080"
	invalid, err := original.Create(ctx, Input{Name: "template-target-" + level + failure.Error(), Enabled: true, ProxyURL: &templateProxy})
	if err != nil {
		t.Fatal(err)
	}
	invalidTarget := domain.RoutingTarget{Mode: domain.RoutingTargetNode, NodeID: invalid.ID}
	config := domain.DefaultOperationsConfig()
	config.DefaultTarget = target
	config.ScopeTargets = map[domain.Scope]domain.RoutingTarget{domain.ScopeBuild: target}
	config.ClassTargets = map[domain.TrafficClass]domain.RoutingTarget{domain.TrafficClassInference: target}
	if level != "default" {
		config.DefaultTarget = invalidTarget
	}
	if level == "class" {
		config.ScopeTargets[domain.ScopeBuild] = invalidTarget
	}
	if _, err := repo.SaveEgressOperationsConfig(ctx, config, func(domain.Node) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != "1.1.1.1" {
			http.Error(w, "unexpected target", 502)
			return
		}
		_, _ = io.WriteString(w, "http://feed.example:8080\n")
	}))
	defer feed.Close()
	encryptedURL, err := original.cipher.Encrypt("http://1.1.1.1/subscription")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := original.cipher.Encrypt(feed.URL)
	if err != nil {
		t.Fatal(err)
	}
	source, err := repo.CreateEgressSource(ctx, domain.SubscriptionSource{Name: "read-failure-feed-" + level + failure.Error(), Enabled: true, EncryptedURL: encryptedURL, EncryptedProxyURL: encryptedProxy, RefreshIntervalSeconds: 900})
	if err != nil {
		t.Fatal(err)
	}
	fault := &hygieneReadFailureRepository{EgressRepository: repo, failure: failure, failID: node.ID}
	service := NewService(fault, original.cipher)
	service.SetSubscriptionFetcher(netfetch.NewEgressSubscriptionFetcher(nil, NormalizeSubscriptionURL))
	service.SetSubscriptionFetcher(netfetch.NewEgressSubscriptionFetcher(nil, NormalizeSubscriptionURL))
	defer service.Close(ctx)
	result, err := service.SyncSource(ctx, source.ID)
	if err != nil || result.Imported != 1 || fault.reads == 0 {
		t.Fatalf("sync did not reach hygiene: %+v %v reads=%d", result, err, fault.reads)
	}
	after, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Errorf("node lookup failure rewrote administrator routing: before=%+v after=%+v", before, after)
	}
	stored, err := repo.GetEgressSource(ctx, source.ID)
	if err != nil || stored.LastSyncError != "" {
		t.Fatalf("completed subscription lost: %+v %v", stored, err)
	}
	fault.failure = nil
	if _, err := service.SyncSource(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	recovered, err := repo.GetEgressOperationsConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	switch level {
	case "default":
		if !reflect.DeepEqual(before, recovered) {
			t.Errorf("recovered sync lost valid routing: %+v", recovered)
		}
	case "scope":
		if recovered.DefaultTarget.Configured() || recovered.ScopeTargets[domain.ScopeBuild] != target || recovered.ClassTargets[domain.TrafficClassInference] != target {
			t.Errorf("recovery did not apply only proven invalid targets: %+v", recovered)
		}
	case "class":
		if recovered.DefaultTarget.Configured() || len(recovered.ScopeTargets) != 0 || recovered.ClassTargets[domain.TrafficClassInference] != target {
			t.Errorf("recovery did not apply only proven invalid targets: %+v", recovered)
		}
	}
}
