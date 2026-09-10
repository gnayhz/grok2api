package egress

import (
	"context"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// TestBatchSetNodeRotationSkipsPoolNodes 锁定批量 webhook 模板与代理池的互斥:
// 套用非空模板时代理池节点必须跳过(外部代理池出口由服务商自动更换,webhook
// 无意义且会产生单节点编辑被互斥校验拒绝的非法状态);清空模板照常执行,
// 用于修复误配数据。
func TestBatchSetNodeRotationSkipsPoolNodes(t *testing.T) {
	service, repo, cipher := newBatchRotationService(t)
	repo.nodes[4] = domain.Node{ID: 4, Name: "pool", Enabled: true, ProxyPool: true, EncryptedProxyURL: mustEncrypt(t, cipher, "socks5://203.0.113.12:1080")}

	result, err := service.BatchSetNodeRotation(context.Background(), []uint64{1, 4}, "http://203.0.113.10:9000/rotate/{port}?token=x")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || result.Skipped != 1 {
		t.Fatalf("result = %+v, want updated=1 skipped=1 (pool node must be skipped)", result)
	}
	if _, written := repo.written[4]; written {
		t.Fatal("pool node must not receive a rotation webhook")
	}

	// 清空模板:误配的池节点照常清理,回到合法状态。
	repo.written[4] = mustEncrypt(t, cipher, "http://legacy.example/hook")
	repo.enableds[4] = true
	result, err = service.BatchSetNodeRotation(context.Background(), []uint64{4}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated != 1 || repo.written[4] != "" || repo.enableds[4] {
		t.Fatalf("clear result = %+v written=%q enabled=%v, want cleared", result, repo.written[4], repo.enableds[4])
	}
}

func mustEncrypt(t *testing.T, cipher interface {
	Encrypt(string) (string, error)
}, value string) string {
	t.Helper()
	encrypted, err := cipher.Encrypt(value)
	if err != nil {
		t.Fatal(err)
	}
	return encrypted
}
