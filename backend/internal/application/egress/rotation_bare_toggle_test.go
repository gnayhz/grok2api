package egress

import (
	"errors"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// TestPoolNodeAndRotationWebhookAreMutuallyExclusive 锁定节点类型的互斥语义:
// 外部代理池(ProxyPool 标志,出口由服务商自动更换)与换 IP Webhook(自建
// WARP 出口的强制切换)是两种节点,不得同时配置。历史缺陷:模型上把二者
// 当可叠加属性,运行时用 ProxyPool && RotationEnabled 判定池模式,导致无
// Webhook 的真代理池被误按固定出口惩罚(单次降智冷却整节点,两家隧道全
// 脏时整池 502);而固定 IP 误勾代理池又靠运行时判定兜底。互斥在保存时
// 强制,运行时 ProxyPool 即池。
func TestPoolNodeAndRotationWebhookAreMutuallyExclusive(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher)
	poolURL := "socks5h://pool.example:1080"
	poolEncrypted, err := cipher.Encrypt(poolURL)
	if err != nil {
		t.Fatal(err)
	}
	webhook := "https://warp.example/rotate?token=x"
	webhookEncrypted, err := cipher.Encrypt(webhook)
	if err != nil {
		t.Fatal(err)
	}

	// 代理池节点勾上 Webhook:保存被拒。
	both := egress.Node{ID: 1, Name: "both", Enabled: true, ProxyPool: true, EncryptedProxyURL: poolEncrypted, EncryptedRotationURL: webhookEncrypted}
	if _, err := service.applyInput(both, Input{Name: "both", Enabled: true}, false); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("pool node with rotation webhook must be rejected, got %v", err)
	}

	// 纯代理池节点(无任何轮换配置):池模式成立。
	poolOnly := egress.Node{ID: 2, Name: "pool", Enabled: true, ProxyPool: true, EncryptedProxyURL: poolEncrypted}
	if !poolOnly.IsPoolModeNode(poolURL) {
		t.Fatal("external pool node (flag only) must classify as pool mode")
	}

	// 自建 WARP 节点(Webhook+开关,无池标志):轮换成立,但不是池模式。
	warp := egress.Node{ID: 3, Name: "warp", Enabled: true, EncryptedProxyURL: poolEncrypted, EncryptedRotationURL: webhookEncrypted, RotationEnabled: true}
	if warp.IsPoolModeNode(poolURL) {
		t.Fatal("self-hosted warp node must never classify as pool mode")
	}
	enabled := true
	updatedWarp, err := service.applyInput(warp, Input{Name: "warp", Enabled: true, RotationEnabled: &enabled}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !updatedWarp.RotationEnabled {
		t.Fatal("warp node with a webhook must keep rotation enabled")
	}
}
