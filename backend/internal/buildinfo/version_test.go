package buildinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CurrentVersion 的回退链与清洗语义锁定：ldflags 注入 > 环境变量 >
// VERSION 文件 > build info > "dev"。cleanVersion 拒绝超长与控制字符，
// 防止 VERSION 文件被塞入转义序列后经 /docs 或版本接口回显。
func TestCleanVersionRejectsOversizeAndControlChars(t *testing.T) {
	if got := cleanVersion(strings.Repeat("v", 129)); got != "" {
		t.Fatalf("oversize version accepted: %d chars", len(got))
	}
	if got := cleanVersion("1.2.3\nrm -rf"); got != "" {
		t.Fatalf("control-chars version accepted: %q", got)
	}
	if got := cleanVersion("  1.2.3  "); got != "1.2.3" {
		t.Fatalf("trim broken: %q", got)
	}
	if got := cleanVersion("1.2.3"); got != "1.2.3" {
		t.Fatalf("plain version broken: %q", got)
	}
}

func TestCurrentVersionFallsBackThroughChain(t *testing.T) {
	dir := t.TempDir()
	origWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(origWD) })

	t.Run("env wins over VERSION file", func(t *testing.T) {
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte("9.9.9-file\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GROK2API_VERSION", "8.8.8-env")
		if got := CurrentVersion(); got != "8.8.8-env" {
			t.Fatalf("got %q, want env value", got)
		}
	})

	t.Run("VERSION file used when env empty", func(t *testing.T) {
		t.Setenv("GROK2API_VERSION", "")
		if got := CurrentVersion(); got != "9.9.9-file" {
			t.Fatalf("got %q, want file value", got)
		}
	})

	t.Run("invalid VERSION file falls through to dev", func(t *testing.T) {
		t.Setenv("GROK2API_VERSION", "")
		if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(strings.Repeat("x", 200)), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := CurrentVersion(); got != "dev" {
			t.Fatalf("got %q, want dev fallback", got)
		}
	})
}
