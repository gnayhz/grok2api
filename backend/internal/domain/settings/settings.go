package settings

import "time"

const (
	DefaultBuildResponseHeaderTimeout = 5 * time.Minute
	MinBuildResponseHeaderTimeout     = 30 * time.Second
	MaxBuildResponseHeaderTimeout     = 30 * time.Minute

	DefaultBuildStreamIdleTimeout = 2 * time.Minute
	MinBuildStreamIdleTimeout     = 30 * time.Second
	MaxBuildStreamIdleTimeout     = 10 * time.Minute

	DefaultWebStreamIdleTimeout     = 90 * time.Second
	DefaultConsoleStreamIdleTimeout = 2 * time.Minute
	MinProviderStreamIdleTimeout    = 30 * time.Second
	MaxProviderStreamIdleTimeout    = 10 * time.Minute
)

// Config 表示可跨重启持久化并支持热加载的网关运行参数。
type Config struct {
	Server            ServerConfig
	ProviderBuild     ProviderBuildConfig
	ProviderWeb       ProviderWebConfig
	ProviderConsole   ProviderConsoleConfig
	Batch             BatchConfig
	Media             MediaConfig
	Frontend          FrontendConfig
	Routing           RoutingConfig
	Audit             AuditConfig
	ClientKeyDefaults ClientKeyDefaultsConfig
	Accounts          AccountsConfig
	// RequestRetry/EgressRotation/AccountRisk 为指针节：旧持久化载荷整段缺失
	// 时保持 nil,applyDomainConfig 沿用文件基线,而不是把零值当作"全部关闭"。
	RequestRetry   *RequestRetryConfig
	EgressRotation *EgressRotationConfig
	AccountRisk    *AccountRiskConfig
}

// AccountRiskConfig 定义账号 RSC 风险归因的可热更新参数。Method 取值
// 恒为 ssoProbe（homepage 解析器已删除，字段仅为兼容保留）；OnDenied 取值 flag/disable/markOnly；
// 语义与 config.AccountRiskRSCConfig 一致(0 值表示代码默认)。
type AccountRiskConfig struct {
	Enabled          bool
	Method           string
	Concurrency      int
	Timeout          time.Duration
	OnDenied         string
	PatrolEnabled    bool
	PatrolBucketDays int
	PatrolInterval   time.Duration
	PatrolBatchSize  int
	// ProbeProxyURL 让 SSO 探针经代理出站(socks5/http(s);空=直连)。
	// 历史事故:服务器直连出口不干净时,首批巡检整批被降级服务误判。
	ProbeProxyURL string
	// DeniedConfirmations: denied 定罪所需连续确认次数(0=默认 2)。
	DeniedConfirmations int
	// DeniedTTL: 已确认 denied verdict 的新鲜期(0=默认 24h),过期可重探。
	DeniedTTL time.Duration
	// BuildProbeEnabled gates the Build-native differential probe for
	// Build-channel degrade events. Pointer semantics: nil (legacy payloads)
	// inherits the file baseline.
	BuildProbeEnabled *bool
}

// ServerConfig 定义可热更新的推理入口容量参数。
type ServerConfig struct {
	MaxConcurrentRequests int
}

// RequestRetryConfig 定义实时路由守卫（质量扣留/重试/截止预算）的可热更新
// 参数。时长语义与 config.RequestRetryConfig 一致：0 表示使用代码默认。
type RequestRetryConfig struct {
	Enabled             bool
	MaxAttempts         int
	OnExhausted         string
	AccountCooldown     time.Duration
	SameAccountRetry    bool
	EvidenceTimeout     time.Duration
	CreatedTimeout      time.Duration
	IdleAccountCooldown time.Duration
	// GuardedModels 是守卫介入的模型白名单（空=全部推理模型）。yaml 级
	// 配置，不在管理端设置面内；apply 忽略持久化 overlay，以文件基线为准。
	GuardedModels []string
}

// EgressRotationConfig 定义出口换 IP 轮换调度的可热更新参数。
type EgressRotationConfig struct {
	Enabled                  bool
	MaxAttemptsPerQuarantine int
	MinNodeInterval          time.Duration
	MaxGlobalPerHour         int
	WebhookTimeout           time.Duration
	WebhookRetries           int
	SettleDelay              time.Duration
	ProbeTimeout             time.Duration
	ProbeInterval            time.Duration
	CanaryModelPublicID      string
	CanaryCreatedTimeout     time.Duration
}

// FrontendConfig 定义公开 API 地址的运行时覆盖值；留空时使用配置文件值。
type FrontendConfig struct {
	PublicAPIBaseURL string
}

type ProviderConsoleConfig struct {
	BaseURL           string
	ChatTimeout       time.Duration
	StreamIdleTimeout time.Duration
}

type MediaConfig struct {
	MaxImageBytes           int64
	MaxTotalBytes           int64
	CleanupThresholdPercent int
	CleanupInterval         time.Duration
}

type ProviderWebConfig struct {
	BaseURL             string
	StatsigMode         string
	StatsigManualValue  string
	StatsigSignerURL    string
	ClearanceMode       string
	FlareSolverrURL     string
	ClearanceTimeout    time.Duration
	ClearanceRefresh    time.Duration
	QuotaTimeout        time.Duration
	ChatTimeout         time.Duration
	StreamIdleTimeout   time.Duration
	ImageTimeout        time.Duration
	VideoTimeout        time.Duration
	MediaConcurrency    int
	AllowNSFW           bool
	RecoveryBackoffBase time.Duration
	RecoveryBackoffMax  time.Duration
}

// BatchConfig 定义账号导入、转换、同步和凭据刷新的并发上限。
type BatchConfig struct {
	ImportConcurrency     int
	ConversionConcurrency int
	SyncConcurrency       int
	RefreshConcurrency    int
	RandomDelay           *time.Duration
}

// ProviderBuildConfig 定义 Grok Build CLI 上游协议标识。
type ProviderBuildConfig struct {
	BaseURL               string
	FallbackBaseURL       string
	ClientVersion         string
	ClientIdentifier      string
	TokenAuth             string
	UserAgent             string
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
}

// RoutingConfig 定义会话粘性、冷却和故障切换边界。
type RoutingConfig struct {
	StickyTTL        time.Duration
	CooldownBase     time.Duration
	CooldownMax      time.Duration
	CapacityWait     time.Duration
	MaxAttempts      int
	VideoMaxAttempts int
	PreferFreeBuild  bool
	// MarkBuildChatDeniedAsReauth 为 true 时，Build chat 权限拒绝标 reauthRequired，默认 false 保留模型级冷却。
	MarkBuildChatDeniedAsReauth bool
	// AccountIsolatedConnections is optional so persisted payloads written by
	// older releases do not silently override a value supplied by config.yaml.
	AccountIsolatedConnections *bool
	SegmentedSelector          *SegmentedSelectorConfig
}

type SegmentedSelectorConfig struct {
	ActiveEnabled bool
	MinCandidates int
	WindowSize    int
}

type AuditConfig struct {
	BufferSize    int
	BatchSize     int
	FlushInterval time.Duration
	CommitDelay   time.Duration
	RetentionDays *int
}

// ClientKeyDefaultsConfig 定义新建客户端密钥的默认限制。
type ClientKeyDefaultsConfig struct {
	RPMLimit      int
	MaxConcurrent int
}

// AccountsConfig 定义账号池后台维护策略；默认全部关闭。
type AccountsConfig struct {
	// MarkBuildForbiddenReauth marks high-confidence Grok Build permission denials as requiring reauthorization.
	MarkBuildForbiddenReauth bool
	// BuildForbiddenReauthCodes contains exact upstream error codes that opt into account invalidation.
	BuildForbiddenReauthCodes []string
	// ExcludeBuildBotFlaggedFromScheduling 为 true 时，bot_flag_source/bfs∈{1,2} 的 Build 账号不参与调度。
	// 仅影响 ProviderBuild 选号；关联 Web/Console 账号调度不受影响。
	ExcludeBuildBotFlaggedFromScheduling bool
	// AutoCleanReauthEnabled 为 true 时，周期性删除已标记 reauthRequired 且超过 minAge 的账号。
	AutoCleanReauthEnabled bool
	// AutoCleanReauthInterval 自动清理扫描间隔。
	AutoCleanReauthInterval time.Duration
	// AutoCleanReauthMinAge 仅删除 reauth_marked_at 早于该时长的 reauthRequired 账号。
	AutoCleanReauthMinAge time.Duration
	// AutoCleanIncludeDisabled 为 true 时，reauth 清理时包含 enabled=false 的账号。
	AutoCleanIncludeDisabled bool
}
