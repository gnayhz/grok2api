package settings

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	randomDelay := 200 * time.Millisecond
	accountIsolated := true
	return Config{
		Server:            ServerConfig{MaxConcurrentRequests: 1024},
		ProviderBuild:     ProviderBuildConfig{BaseURL: "https://api.grok.com", FallbackBaseURL: DefaultBuildFallbackBaseURL, ClientVersion: "1.0.4", ClientIdentifier: "id", TokenAuth: "token", UserAgent: "ua", SessionIdleConnTimeout: DefaultBuildSessionIdleConnTimeout, ResponseHeaderTimeout: DefaultBuildResponseHeaderTimeout, StreamIdleTimeout: DefaultBuildStreamIdleTimeout},
		ProviderWeb:       ProviderWebConfig{BaseURL: "https://chat.grok.com", StatsigMode: "manual", StatsigManualValue: strings.Repeat("A", 94), ClearanceMode: "manual", ClearanceTimeout: time.Minute, ClearanceRefresh: 10 * time.Minute, QuotaTimeout: 30 * time.Second, ChatTimeout: time.Minute, StreamIdleTimeout: DefaultWebStreamIdleTimeout, ImageTimeout: time.Minute, VideoTimeout: 10 * time.Minute, MediaConcurrency: 4, RecoveryBackoffBase: 30 * time.Second, RecoveryBackoffMax: time.Hour},
		ProviderConsole:   ProviderConsoleConfig{BaseURL: "https://console.grok.com", ChatTimeout: time.Minute, StreamIdleTimeout: DefaultConsoleStreamIdleTimeout},
		Batch:             BatchConfig{ImportConcurrency: 4, ConversionConcurrency: 4, SyncConcurrency: 4, RefreshConcurrency: 4, RandomDelay: &randomDelay},
		Media:             MediaConfig{MaxImageBytes: 8 << 20, MaxTotalBytes: 1 << 30, CleanupThresholdPercent: 80, CleanupInterval: time.Hour},
		Frontend:          FrontendConfig{FilePublicAPIBaseURL: "https://ops.example.test"},
		Routing:           RoutingConfig{StickyTTL: time.Minute, CooldownBase: time.Second, CooldownMax: 30 * time.Second, CapacityWait: 5 * time.Second, MaxAttempts: 3, VideoMaxAttempts: 3, AccountIsolatedConnections: &accountIsolated, SegmentedSelector: &SegmentedSelectorConfig{ActiveEnabled: false}},
		Audit:             AuditConfig{BufferSize: 256, BatchSize: 64, FlushInterval: time.Second, CommitDelay: 5 * time.Millisecond},
		ClientKeyDefaults: ClientKeyDefaultsConfig{RPMLimit: 120, MaxConcurrent: 8},
		Accounts:          AccountsConfig{AutoCleanReauthInterval: 10 * time.Minute, AutoCleanReauthMinAge: 24 * time.Hour, BuildForbiddenReauthCodes: []string{"invalid_token"}},
		RequestRetry:      &RequestRetryConfig{OnExhausted: "fail_closed"},
		EgressRotation:    &EgressRotationConfig{Enabled: true, MaxAttemptsPerQuarantine: 3, MinNodeInterval: 3 * time.Minute, MaxGlobalPerHour: 6, WebhookTimeout: 15 * time.Second, WebhookRetries: 2, SettleDelay: 20 * time.Second, ProbeTimeout: 2 * time.Minute, ProbeInterval: 5 * time.Second},
	}
}

// 配置领域测试无需数据库和文件：纯输入即可覆盖热更新政策范围。
func TestValidateAcceptsRepresentativeConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("representative config rejected: %v", err)
	}
}

func TestValidateRejectsOutOfRangeHotFields(t *testing.T) {
	cases := []struct {
		name  string
		mutat func(*Config)
		want  string
	}{
		{"server concurrency", func(c *Config) { c.Server.MaxConcurrentRequests = 0 }, "server.maxConcurrentRequests"},
		{"build header timeout", func(c *Config) { c.ProviderBuild.ResponseHeaderTimeout = time.Second }, "响应头超时"},
		{"web chat timeout", func(c *Config) { c.ProviderWeb.ChatTimeout = time.Second }, "provider.web.chatTimeout"},
		{"statsig mode", func(c *Config) { c.ProviderWeb.StatsigMode = "auto" }, "Statsig 模式"},
		{"statsig manual value", func(c *Config) { c.ProviderWeb.StatsigManualValue = "not-base64!" }, "x-statsig-id"},
		{"clearance mode", func(c *Config) { c.ProviderWeb.ClearanceMode = "magic" }, "Clearance 模式"},
		{"console url scheme", func(c *Config) { c.ProviderConsole.BaseURL = "http://console.grok.com" }, "provider.console.baseURL"},
		{"batch concurrency", func(c *Config) { c.Batch.SyncConcurrency = 51 }, "批量任务并发"},
		{"media image bytes", func(c *Config) { c.Media.MaxImageBytes = 1 }, "media.maxImageBytes"},
		{"frontend url", func(c *Config) { c.Frontend.FilePublicAPIBaseURL = "ftp://x" }, "frontend.publicApiBaseURL"},
		{"routing sticky", func(c *Config) { c.Routing.StickyTTL = 0 }, "routing.stickyTTL"},
		{"routing attempts", func(c *Config) { c.Routing.MaxAttempts = 0 }, "routing.maxAttempts"},
		{"segmented window", func(c *Config) {
			c.Routing.SegmentedSelector.ActiveEnabled = true
			c.Routing.SegmentedSelector.MinCandidates = 100
			c.Routing.SegmentedSelector.WindowSize = 300
		}, "segmentedWindowSize"},
		{"audit batch", func(c *Config) { c.Audit.BatchSize = 2000 }, "audit.batchSize"},
		{"client key defaults", func(c *Config) { c.ClientKeyDefaults.RPMLimit = 0 }, "clientKeyDefaults"},
		{"accounts autoclean", func(c *Config) { c.Accounts.AutoCleanReauthInterval = time.Second }, "autoCleanReauthInterval"},
		{"reauth codes empty", func(c *Config) { c.Accounts.BuildForbiddenReauthCodes = nil }, "至少需要一个错误码"},
		{"reauth code pattern", func(c *Config) { c.Accounts.BuildForbiddenReauthCodes = []string{"bad code!"} }, "无效错误码"},
		{"request retry legacy", func(c *Config) { c.RequestRetry.OnExhausted = "retry" }, "legacy value"},
		{"rotation interval", func(c *Config) { c.EgressRotation.MinNodeInterval = time.Second }, "minNodeInterval"},
		{"fallback https", func(c *Config) { c.ProviderBuild.FallbackBaseURL = "http://api.x.ai/v1" }, "HTTPS"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			value := validConfig()
			tc.mutat(&value)
			err := value.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate() = %v; want containing %q", err, tc.want)
			}
		})
	}
}

// 领域层见到的是映射后的值：文件边界已把空回退规范化为默认地址，
// 领域对未规范化的空值保持拒绝（纵深防御，防止第二处映射漏掉规范化）。
func TestValidateRejectsUnnormalizedBlankFallback(t *testing.T) {
	value := validConfig()
	value.ProviderBuild.FallbackBaseURL = "   "
	if err := value.Validate(); err == nil {
		t.Fatal("unnormalized blank fallback accepted at domain level")
	}
}
