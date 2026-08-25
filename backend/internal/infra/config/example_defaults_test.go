package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// TestExampleConfigMatchesCodeDefaults：config.example.yaml 是运维的配置
// 起点。加载器使用 KnownFields 严格解码——示例一旦出现代码不认识的键，
// 用户复制示例即启动失败；示例值与代码默认值漂移则会误导运维。本测试
// 用真实密钥替换占位后完整加载示例文件，并校验文档化的 provider /
// clientKeyDefaults 值与 defaultConfig() 完全一致。
func TestExampleConfigMatchesCodeDefaults(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatalf("read config.example.yaml: %v", err)
	}
	// 替换三类占位符：密钥与管理员密码（Load 会拒绝示例占位值）。
	content := strings.Replace(string(raw), "replace-with-a-strong-password", "ExampleStrongPassw0rd!", 1)
	content = strings.Replace(content, "replace-with-at-least-32-characters", "12345678901234567890123456789012", 1)
	content = strings.Replace(content, "replace-with-base64-key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", 1)

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("config.example.yaml 必须可被加载器完整接受（未知键或占位值会导致用户复制即失败）: %v", err)
	}

	defaults := defaultConfig()
	if cfg.Provider.Build.BaseURL != defaults.Provider.Build.BaseURL ||
		cfg.Provider.Build.FallbackBaseURL != defaults.Provider.Build.FallbackBaseURL ||
		cfg.Provider.Build.ClientVersion != defaults.Provider.Build.ClientVersion ||
		cfg.Provider.Build.ClientIdentifier != defaults.Provider.Build.ClientIdentifier ||
		cfg.Provider.Build.TokenAuth != defaults.Provider.Build.TokenAuth ||
		cfg.Provider.Build.UserAgent != defaults.Provider.Build.UserAgent {
		t.Fatalf("example provider.build 与代码默认漂移: example=%#v code=%#v", cfg.Provider.Build, defaults.Provider.Build)
	}
	if cfg.Provider.Web.BaseURL != defaults.Provider.Web.BaseURL ||
		cfg.Provider.Web.QuotaTimeout != defaults.Provider.Web.QuotaTimeout ||
		cfg.Provider.Web.ChatTimeout != defaults.Provider.Web.ChatTimeout ||
		cfg.Provider.Web.ImageTimeout != defaults.Provider.Web.ImageTimeout ||
		cfg.Provider.Web.VideoTimeout != defaults.Provider.Web.VideoTimeout ||
		cfg.Provider.Web.MediaConcurrency != defaults.Provider.Web.MediaConcurrency ||
		cfg.Provider.Web.AllowNSFW != defaults.Provider.Web.AllowNSFW ||
		cfg.Provider.Web.RecoveryBackoffBase != defaults.Provider.Web.RecoveryBackoffBase ||
		cfg.Provider.Web.RecoveryBackoffMax != defaults.Provider.Web.RecoveryBackoffMax {
		t.Fatalf("example provider.web 与代码默认漂移: example=%#v code=%#v", cfg.Provider.Web, defaults.Provider.Web)
	}
	if cfg.Provider.Console.BaseURL != defaults.Provider.Console.BaseURL ||
		cfg.Provider.Console.ChatTimeout != defaults.Provider.Console.ChatTimeout {
		t.Fatalf("example provider.console 与代码默认漂移: example=%#v code=%#v", cfg.Provider.Console, defaults.Provider.Console)
	}
	if cfg.ClientKeyDefaults.RPMLimit != clientkeydomain.DefaultRPMLimit || cfg.ClientKeyDefaults.MaxConcurrent != clientkeydomain.DefaultMaxConcurrent {
		t.Fatalf("example clientKeyDefaults 与代码默认漂移: example=%#v", cfg.ClientKeyDefaults)
	}
	if cfg.Provider.Web.ChatTimeout.Value() != 2*time.Minute {
		t.Fatalf("provider.web.chatTimeout 语义应为 2m, got %s", cfg.Provider.Web.ChatTimeout.Value())
	}
}
