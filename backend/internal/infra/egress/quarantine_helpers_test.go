package egress

import (
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

// testCipher 构建测试用对称密钥(旧质量隔离测试删除后仍被客户端缓存
// 与粘性池生命周期测试共用)。
func testCipher(t *testing.T) security.Cryptor {
	t.Helper()
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	return cipher
}
