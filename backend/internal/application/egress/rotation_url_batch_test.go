package egress

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

type rotationURLStubRepo struct {
	qualityStubRepo
	written  map[uint64]string
	enableds map[uint64]bool
}

func (r *rotationURLStubRepo) UpdateEgressNodeRotationURL(_ context.Context, id uint64, encrypted string, enabled bool) error {
	if r.written == nil {
		r.written = map[uint64]string{}
		r.enableds = map[uint64]bool{}
	}
	r.written[id] = encrypted
	r.enableds[id] = enabled
	return nil
}

func newBatchRotationService(t *testing.T) (*Service, *rotationURLStubRepo, security.Cryptor) {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypt := func(value string) string {
		encrypted, encryptErr := cipher.Encrypt(value)
		if encryptErr != nil {
			t.Fatal(encryptErr)
		}
		return encrypted
	}
	base := &qualityStubRepo{nodes: map[uint64]domain.Node{
		1: {ID: 1, Name: "B-1", Enabled: true, EncryptedProxyURL: encrypt("socks5://203.0.113.10:1080")},
		2: {ID: 2, Name: "B-2", Enabled: true, EncryptedProxyURL: encrypt("socks5://203.0.113.10:1081")},
		3: {ID: 3, Name: "C-1", Enabled: true, EncryptedProxyURL: encrypt("socks5://203.0.113.11:1080")},
	}}
	repo := &rotationURLStubRepo{}
	repo.nodes = base.nodes
	service := &Service{repository: repo, cipher: cipher}
	return service, repo, cipher
}

func decryptForTest(t *testing.T, cipher security.Cryptor, encrypted string) string {
	t.Helper()
	value, err := cipher.Decrypt(encrypted)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// {port} 占位符按每个节点的代理端口替换——一机多实例的核心场景。
func TestBatchSetNodeRotationSubstitutesPortPerNode(t *testing.T) {
	service, repo, cipher := newBatchRotationService(t)
	result, err := service.BatchSetNodeRotation(context.Background(), []uint64{1, 2, 3}, "http://203.0.113.10:9000/rotate/{port}?token=x")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 3 || result.Skipped != 0 {
		t.Fatalf("result = %+v", result)
	}
	want := map[uint64]string{
		1: "http://203.0.113.10:9000/rotate/1080?token=x",
		2: "http://203.0.113.10:9000/rotate/1081?token=x",
		3: "http://203.0.113.10:9000/rotate/1080?token=x",
	}
	for id, expected := range want {
		if got := decryptForTest(t, cipher, repo.written[id]); got != expected {
			t.Fatalf("node %d rotation = %q, want %q", id, got, expected)
		}
	}
}

// {name} 需要按节点名替换且 URL 转义；{host} 取代理主机。
func TestBatchSetNodeRotationSubstitutesNameAndHost(t *testing.T) {
	service, repo, cipher := newBatchRotationService(t)
	if _, err := service.BatchSetNodeRotation(context.Background(), []uint64{1}, "http://{host}:9000/rotate/{name}?token=x"); err != nil {
		t.Fatal(err)
	}
	want := "http://203.0.113.10:9000/rotate/B-1?token=x"
	if got := decryptForTest(t, cipher, repo.written[1]); got != want {
		t.Fatalf("rotation = %q, want %q", got, want)
	}
}

// 模板含 {port} 而节点代理无端口 → 跳过该节点而非整体失败。
func TestBatchSetNodeRotationSkipsPortlessNode(t *testing.T) {
	service, repo, _ := newBatchRotationService(t)
	cipher := service.cipher
	encrypted, err := cipher.Encrypt("socks5://203.0.113.10")
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	node := repo.nodes[1]
	node.EncryptedProxyURL = encrypted
	repo.nodes[1] = node
	repo.mu.Unlock()
	result, err := service.BatchSetNodeRotation(context.Background(), []uint64{1, 2}, "http://x:9000/rotate/{port}")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || result.Skipped != 1 {
		t.Fatalf("result = %+v", result)
	}
	if _, written := repo.written[1]; written {
		t.Fatalf("portless node should be skipped")
	}
}

// 非法模板整体拒绝；空模板清除。
func TestBatchSetNodeRotationValidatesAndClears(t *testing.T) {
	service, repo, _ := newBatchRotationService(t)
	if _, err := service.BatchSetNodeRotation(context.Background(), []uint64{1}, "ftp://bad"); err == nil {
		t.Fatal("invalid scheme accepted")
	}
	result, err := service.BatchSetNodeRotation(context.Background(), []uint64{1, 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 2 {
		t.Fatalf("clear result = %+v", result)
	}
	for _, id := range []uint64{1, 2} {
		if repo.written[id] != "" {
			t.Fatalf("node %d not cleared", id)
		}
	}
}
