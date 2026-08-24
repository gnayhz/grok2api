package config

import (
	"os"
	"path/filepath"
	"testing"
)

// yaml 缺省字段的默认保留语义:egress.qualityGuard 段只写一个字段时,
// 其余字段必须保留 DefaultEgressConfig 的预填值——特别是 crossAccountThreshold=2
// (运行时 <2 会静默关闭跨账号兜底,与三处文档"默认 2"矛盾)。
func TestEgressYamlPartialSectionKeepsThresholdDefault(t *testing.T) {
	writeAndLoad := func(content string) Config {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(path)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		return cfg
	}

	// 部分字段:显式的生效,兄弟字段保留默认。
	partial := writeAndLoad(`server:
  listen: "0.0.0.0:8000"
secrets:
  jwtSecret: "0123456789abcdef0123456789abcdef"
  credentialEncryptionKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
egress:
  qualityGuard:
    quarantineCooldown: 2h
`)
	if got := partial.Egress.QualityGuard.QuarantineCooldown.Value(); got.Hours() != 2 {
		t.Fatalf("explicit field lost: %v", partial.Egress.QualityGuard.QuarantineCooldown)
	}
	if partial.Egress.QualityGuard.CrossAccountThreshold != 2 {
		t.Fatalf("partial section clobbered threshold default: %d (want 2 — 运行时 <2 静默关闭跨账号兜底)", partial.Egress.QualityGuard.CrossAccountThreshold)
	}

	// 完全缺省 egress 段:全部默认保留。
	bare := writeAndLoad(`server:
  listen: "0.0.0.0:8000"
secrets:
  jwtSecret: "0123456789abcdef0123456789abcdef"
  credentialEncryptionKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
`)
	if bare.Egress.QualityGuard.CrossAccountThreshold != 2 {
		t.Fatalf("absent section lost threshold default: %d", bare.Egress.QualityGuard.CrossAccountThreshold)
	}
	if !bare.Egress.Rotation.Enabled {
		t.Fatal("absent section lost rotation default (enabled)")
	}
}
