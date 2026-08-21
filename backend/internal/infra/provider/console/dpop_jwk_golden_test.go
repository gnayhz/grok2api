package console

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"math/big"
	"testing"
)

// TestPublicDPoPJWKGoldenVector 用固定标量锁定 JWK 坐标编码的字节形态
// （2026-08-21 golden 基准，值由旧实现 key.X/key.Y FillBytes(32) 生成）：
// 迁移到 PublicKey.Bytes()（未压缩 SEC1 点 0x04||X||Y）后输出必须逐字节一致，
// 否则 JWK thumbprint（jkt）会变，已分发的 DPoP 会话校验将全部失效。
func TestPublicDPoPJWKGoldenVector(t *testing.T) {
	t.Parallel()
	d, ok := new(big.Int).SetString("8f2a55c37c4e4bfc3f1c4a7e260b1b0b5b29e6d3b7f5e2c4a9d08c7b3e5f1a6d", 16)
	if !ok {
		t.Fatal("固定标量解析失败")
	}
	x, y := elliptic.P256().ScalarBaseMult(d.Bytes())
	key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: x, Y: y}
	jwk, err := publicDPoPJWK(key)
	if err != nil {
		t.Fatalf("P-256 公钥编码不应失败: %v", err)
	}
	const wantX = "5vwDjp1I3Cwz9H2doiAOlvcuJp5aqY_aTLs9qSDxgZk"
	const wantY = "TesDH_lMvlUQm8pz0hPy0IcRipVqot-CdXq6vz5oLJk"
	if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.X != wantX || jwk.Y != wantY {
		t.Fatalf("JWK golden 不匹配: kty=%s crv=%s x=%s y=%s", jwk.Kty, jwk.Crv, jwk.X, jwk.Y)
	}
}
