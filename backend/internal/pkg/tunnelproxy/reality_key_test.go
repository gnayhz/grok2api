package tunnelproxy

import (
	"crypto/ecdh"
	"crypto/rand"
	"testing"

	utls "github.com/refraction-networking/utls"
)

// TestRealityX25519KeySelection 锁定 round 32 修复：握手处与第二处取键
// 口径一致——KeyShareKeys 优先、EcdheKey 回退、双缺时 nil 并由调用方
// 显式报错（此前 handshake 直接读 EcdheKey，KeyShareKeys 路径 nil panic）。
func TestRealityX25519KeySelection(t *testing.T) {
	key, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// 形态1：只有 KeyShareKeys（utls >=1.8 标准构建）。
	state := utls.TLS13OnlyState{KeyShareKeys: &utls.KeySharePrivateKeys{Ecdhe: key}}
	if got := realityX25519Key(state); got == nil || !got.Equal(key) {
		t.Fatal("KeyShareKeys 形态应取到键")
	}
	// 形态2：只有 EcdheKey（fingerprint 构建，utls <1.8 语义）。
	//nolint:staticcheck // 测试夹具刻意构造弃用形态验证回退分支
	state = utls.TLS13OnlyState{EcdheKey: key}
	if got := realityX25519Key(state); got == nil || !got.Equal(key) {
		t.Fatal("EcdheKey 回退形态应取到键")
	}
	// 形态3：双缺 → nil（调用方报错路径）。
	if got := realityX25519Key(utls.TLS13OnlyState{}); got != nil {
		t.Fatal("双缺应返回 nil")
	}
	// 优先级：两者都在时 KeyShareKeys 胜出（与 utls 文档语义一致）。
	other, _ := ecdh.X25519().GenerateKey(rand.Reader)
	state = utls.TLS13OnlyState{KeyShareKeys: &utls.KeySharePrivateKeys{Ecdhe: key}, EcdheKey: other} //nolint:staticcheck // 优先级夹具
	if got := realityX25519Key(state); !got.Equal(key) {
		t.Fatal("KeyShareKeys 应优先于 EcdheKey")
	}
}
