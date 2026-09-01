package egress

import (
	"context"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// fakeQualityRepo embeds the shared test stub and records targeted writes.
type fakeQualityRepo struct {
	egressRepositoryTestStub
	mu      sync.Mutex
	node    domain.Node
	quality []uint64
	updates []domain.Node
}

func (r *fakeQualityRepo) GetEgressNode(context.Context, uint64) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.node, nil
}

func (r *fakeQualityRepo) UpdateEgressNodeQualityState(_ context.Context, id uint64, health float64, failures int, cooldown *time.Time, lastErr string, degradeCount int, lastDegradedAt *time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.quality = append(r.quality, id)
	r.node.Health, r.node.FailureCount, r.node.CooldownUntil, r.node.LastError = health, failures, cooldown, lastErr
	r.node.DegradeCount, r.node.LastDegradedAt = degradeCount, lastDegradedAt
	return nil
}

func (r *fakeQualityRepo) UpdateEgressNode(_ context.Context, value domain.Node) (domain.Node, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.updates = append(r.updates, value)
	r.node = value
	return value, nil
}

func TestQuarantineNodeForQualityCoolsFixedNode(t *testing.T) {
	until := time.Now().Add(time.Hour)
	repo := &fakeQualityRepo{node: domain.Node{ID: 7, Name: "B-microwarp", Enabled: true, Health: 1, EncryptedProxyURL: "enc"}}
	manager := NewManager(repo, nil)
	previous, err := manager.QuarantineNodeForQuality(context.Background(), 7, until)
	if err != nil {
		t.Fatal(err)
	}
	if previous.ID != 7 {
		t.Fatalf("previous snapshot = %d", previous.ID)
	}
	node := repo.node
	if node.CooldownUntil == nil || !node.CooldownUntil.After(time.Now()) {
		t.Fatalf("cooldown not applied: %+v", node.CooldownUntil)
	}
	if node.LastError != domain.LastErrorExitIPQuality {
		t.Fatalf("last error = %q", node.LastError)
	}
	if node.DegradeCount != 1 || node.FailureCount != 1 {
		t.Fatalf("counters = degrade %d failure %d", node.DegradeCount, node.FailureCount)
	}
}

func TestQuarantineNodeForQualitySkipsPoolNodes(t *testing.T) {
	repo := &fakeQualityRepo{node: domain.Node{ID: 9, ProxyPool: true, RotationEnabled: true, EncryptedProxyURL: "enc"}}
	manager := NewManager(repo, nil)
	if _, err := manager.QuarantineNodeForQuality(context.Background(), 9, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if len(repo.quality) != 0 || len(repo.updates) != 0 {
		t.Fatalf("pool node was quarantined: quality=%v updates=%d", repo.quality, len(repo.updates))
	}
}

func newQuarantineTestCipher(t *testing.T) security.Cryptor {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}

func encryptedForTest(t *testing.T, cipher security.Cryptor, value string) string {
	t.Helper()
	encrypted, err := cipher.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}

func TestAcquireHonorsRequestNodeExclusions(t *testing.T) {
	cipher := newQuarantineTestCipher(t)
	bad := domain.Node{ID: 1, Name: "bad", Enabled: true, Health: 1, EncryptedProxyURL: encryptedForTest(t, cipher, "http://127.0.0.1:1")}
	good := domain.Node{ID: 2, Name: "good", Enabled: true, Health: 1, EncryptedProxyURL: encryptedForTest(t, cipher, "http://127.0.0.1:2")}
	repo := &egressRepositoryTestStub{nodes: []domain.Node{bad, good}}
	manager := NewManager(repo, cipher)
	excluded := map[uint64]struct{}{1: {}}
	ctx := WithNodeExclusions(context.Background(), excluded)
	lease, _, err := manager.AcquireIfConfigured(ctx, domain.ScopeBuild, "affinity")
	if err != nil {
		t.Fatal(err)
	}
	if lease == nil || lease.NodeID == 1 {
		t.Fatalf("excluded node still selected: %+v", lease)
	}
	lease.Release()
}
