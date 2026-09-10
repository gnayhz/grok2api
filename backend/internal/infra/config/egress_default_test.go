package config

import (
	"os"
	"path/filepath"
	"testing"
)

// yaml 缺省字段的默认保留语义:egress.rotation 段只写一个字段时,
// 其余字段必须保留 DefaultEgressConfig 的预填值。
func TestEgressYamlPartialSectionKeepsDefaults(t *testing.T) {
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
	partial := writeAndLoad("server:\n  listen: \"0.0.0.0:8000\"\nsecrets:\n  jwtSecret: \"0123456789abcdef0123456789abcdef\"\n  credentialEncryptionKey: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\negress:\n  rotation:\n    maxAttemptsPerQuarantine: 5\n")
	if partial.Egress.Rotation.MaxAttemptsPerQuarantine != 5 {
		t.Fatalf("explicit field lost: %d", partial.Egress.Rotation.MaxAttemptsPerQuarantine)
	}
	if !partial.Egress.Rotation.Enabled {
		t.Fatal("partial section clobbered enabled default")
	}

	// 完全缺省 egress 段:全部默认保留。
	bare := writeAndLoad("server:\n  listen: \"0.0.0.0:8000\"\nsecrets:\n  jwtSecret: \"0123456789abcdef0123456789abcdef\"\n  credentialEncryptionKey: \"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=\"\n")
	if !bare.Egress.Rotation.Enabled {
		t.Fatal("absent section lost rotation default (enabled)")
	}
}
