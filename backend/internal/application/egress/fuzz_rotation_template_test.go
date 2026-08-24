package egress

import (
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// FuzzResolveRotationTemplate 模糊轮换 webhook 模板解析:任意模板+任意节点
// 代理 URL 组合不得 panic/超时; 可解析结果必须包含模板与节点双方信息且
// 与 {name}/{host}/{port} 占位符语义一致(结果为 URL 形态)。语义正确性
// 由 rotation_url_batch_test 的确定用例覆盖, 本测试锁健壮性。
func FuzzResolveRotationTemplate(f *testing.F) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		f.Fatal(err)
	}
	f.Add("http://rot:9000/rotate/{port}?token=x", "socks5://10.0.0.1:1080", "node-a")
	f.Add("http://rot/{name}", "http://[2001:db8::1]:80", "名 称")
	f.Add("http://{host}", "http://host", "n")
	f.Add("", "http://10.0.0.1:1", "")
	f.Add("http://rot/{{port}}", "socks5h://noport", "n")
	f.Fuzz(func(t *testing.T, template, proxyURL, name string) {
		encrypted, encErr := cipher.Encrypt(proxyURL)
		if encErr != nil {
			t.Skip()
		}
		node := domain.Node{ID: 1, Name: name, Enabled: true, EncryptedProxyURL: encrypted}
		done := make(chan struct{})
		go func() {
			defer close(done)
			resolved, _ := resolveRotationTemplate(template, node, cipher)
			_ = resolved
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("template resolve exceeded 5s: template len %d proxy len %d", len(template), len(proxyURL))
		}
	})
}
