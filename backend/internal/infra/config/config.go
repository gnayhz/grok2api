package config

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/pkg/signerurl"
	"gopkg.in/yaml.v3"
)

const (
	DatabaseURLEnv                = "GROK2API_DATABASE_URL"
	StatsigModeManual             = "manual"
	StatsigModeURL                = "url"
	ClearanceModeManual           = "manual"
	ClearanceModeFlareSolverr     = "flaresolverr"
	ClearanceModeOnDemand         = "on_demand"
	DefaultStatsigSignerURL       = "https://grok.wodf.de/sign"
	DefaultFlareSolverrURL        = "http://flaresolverr:8191"
	RecommendedBuildClientVersion = "1.0.4"
	RecommendedBuildUserAgent     = "grok-shell/" + RecommendedBuildClientVersion + " (linux; x86_64)"

	maxServerBodyBytes     = 256 << 20
	maxRequestTimeout      = 24 * time.Hour
	maxReadTimeout         = time.Hour
	maxRoutingTTL          = 30 * 24 * time.Hour
	maxRoutingCooldown     = 24 * time.Hour
	maxRoutingCapacityWait = 30 * time.Second
	maxRoutingAttempts     = 65535
	minAuditFlushInterval  = 10 * time.Millisecond
	maxAuditFlushInterval  = time.Minute
	maxAuditBufferSize     = 262144
	maxAuditBatchSize      = 4096
	minAuditCommitDelay    = time.Millisecond
	maxAuditCommitDelay    = 50 * time.Millisecond
	maxDeploymentReplicas  = 1024
)

const unlimitedRoutingAttempts = -1

var buildForbiddenCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// Config 表示后端运行配置。
type Config struct {
	Server            ServerConfig            `yaml:"server"`
	Frontend          FrontendConfig          `yaml:"frontend"`
	Database          DatabaseConfig          `yaml:"database"`
	RuntimeStore      RuntimeStoreConfig      `yaml:"runtimeStore"`
	Deployment        DeploymentConfig        `yaml:"deployment"`
	Auth              AuthConfig              `yaml:"auth"`
	Secrets           Secrets                 `yaml:"secrets"`
	BootstrapAdmin    BootstrapAdminConfig    `yaml:"bootstrapAdmin"`
	Provider          ProviderConfig          `yaml:"provider"`
	Batch             BatchConfig             `yaml:"-"`
	Media             MediaConfig             `yaml:"media"`
	Routing           RoutingConfig           `yaml:"routing"`
	Audit             AuditConfig             `yaml:"audit"`
	RequestRetry      RequestRetryConfig      `yaml:"requestRetry"`
	AccountRisk       AccountRiskConfig       `yaml:"accountRisk"`
	ClientKeyDefaults ClientKeyDefaultsConfig `yaml:"clientKeyDefaults"`
	Egress            EgressConfig            `yaml:"egress"`
	Accounts          AccountsConfig          `yaml:"-"`
}

type ServerConfig struct {
	Listen                string   `yaml:"listen"`
	MaxBodyBytes          int64    `yaml:"maxBodyBytes"`
	MaxConcurrentRequests int      `yaml:"maxConcurrentRequests"`
	TrustedProxies        []string `yaml:"trustedProxies"`
	ReadTimeout           Duration `yaml:"readTimeout"`
	RequestTimeout        Duration `yaml:"requestTimeout"`
	SwaggerEnabled        bool     `yaml:"swaggerEnabled"`
	// UpdateCheckEnabled 控制出网检查 GitHub Release(nil=默认开启)。内网/离线
	// 或合规敏感部署可显式关闭——此前无任何开关, 启动即联网。
	UpdateCheckEnabled *bool `yaml:"updateCheckEnabled"`
}

type FrontendConfig struct {
	PublicAPIBaseURL         string `yaml:"publicApiBaseURL"`
	PublicAPIBaseURLOverride string `yaml:"-"`
	StaticPath               string `yaml:"staticPath"`
}

const DefaultPublicAPIBaseURL = "http://127.0.0.1:8000"

// EffectivePublicAPIBaseURL 按运行设置、配置文件、内置默认值的顺序解析公开地址。
func (c FrontendConfig) EffectivePublicAPIBaseURL() string {
	for _, value := range []string{c.PublicAPIBaseURLOverride, c.PublicAPIBaseURL} {
		if value = strings.TrimRight(strings.TrimSpace(value), "/"); value != "" {
			return value
		}
	}
	return DefaultPublicAPIBaseURL
}

type DatabaseConfig struct {
	Driver   string                 `yaml:"driver"`
	SQLite   SQLiteDatabaseConfig   `yaml:"sqlite"`
	Postgres PostgresDatabaseConfig `yaml:"postgres"`
}

type SQLiteDatabaseConfig struct {
	Path string `yaml:"path"`
}

type PostgresDatabaseConfig struct {
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"maxOpenConns"`
	MaxIdleConns int    `yaml:"maxIdleConns"`
}

type RuntimeStoreConfig struct {
	Driver string             `yaml:"driver"`
	Redis  RedisRuntimeConfig `yaml:"redis"`
}

type DeploymentConfig struct {
	Replicas    int    `yaml:"replicas"`
	InstanceID  string `yaml:"instanceID"`
	ClusterID   string `yaml:"clusterID"`
	SharedMedia bool   `yaml:"sharedMedia"`
}

type RedisRuntimeConfig struct {
	Address   string `yaml:"address"`
	Username  string `yaml:"username"`
	Password  string `yaml:"password"`
	Database  int    `yaml:"database"`
	KeyPrefix string `yaml:"keyPrefix"`
	TLS       bool   `yaml:"tls"`
}

type AuthConfig struct {
	AccessTokenTTL  Duration `yaml:"accessTokenTTL"`
	RefreshTokenTTL Duration `yaml:"refreshTokenTTL"`
	SecureCookies   bool     `yaml:"secureCookies"`
}

type ProviderConfig struct {
	Build   BuildProviderConfig   `yaml:"build"`
	Web     WebProviderConfig     `yaml:"web"`
	Console ConsoleProviderConfig `yaml:"console"`
}

type BuildProviderConfig struct {
	BaseURL               string   `yaml:"baseURL"`
	FallbackBaseURL       string   `yaml:"fallbackBaseURL"`
	ClientVersion         string   `yaml:"clientVersion"`
	ClientIdentifier      string   `yaml:"clientIdentifier"`
	TokenAuth             string   `yaml:"tokenAuth"`
	UserAgent             string   `yaml:"userAgent"`
	ResponseHeaderTimeout Duration `yaml:"-"`
	StreamIdleTimeout     Duration `yaml:"-"`
}

// DefaultBuildFallbackBaseURL 是主 Build API 对可回退推理操作 403 时探测的 XAI API 根地址。
const DefaultBuildFallbackBaseURL = "https://api.x.ai/v1"

type WebProviderConfig struct {
	BaseURL             string   `yaml:"baseURL"`
	StatsigMode         string   `yaml:"-"`
	StatsigManualValue  string   `yaml:"-"`
	StatsigSignerURL    string   `yaml:"-"`
	ClearanceMode       string   `yaml:"-"`
	FlareSolverrURL     string   `yaml:"-"`
	ClearanceTimeout    Duration `yaml:"-"`
	ClearanceRefresh    Duration `yaml:"-"`
	QuotaTimeout        Duration `yaml:"quotaTimeout"`
	ChatTimeout         Duration `yaml:"chatTimeout"`
	StreamIdleTimeout   Duration `yaml:"-"`
	ImageTimeout        Duration `yaml:"imageTimeout"`
	VideoTimeout        Duration `yaml:"videoTimeout"`
	MediaConcurrency    int      `yaml:"mediaConcurrency"`
	AllowNSFW           bool     `yaml:"allowNSFW"`
	RecoveryBackoffBase Duration `yaml:"recoveryBackoffBase"`
	RecoveryBackoffMax  Duration `yaml:"recoveryBackoffMax"`
}

type ConsoleProviderConfig struct {
	BaseURL           string   `yaml:"baseURL"`
	LegacyUserAgent   string   `yaml:"userAgent"` // Deprecated: 仅用于兼容旧配置文件，不参与请求。
	ChatTimeout       Duration `yaml:"chatTimeout"`
	StreamIdleTimeout Duration `yaml:"-"`
}

// BatchConfig 定义可热加载的账号批量任务并发上限。
type BatchConfig struct {
	ImportConcurrency     int
	ConversionConcurrency int
	SyncConcurrency       int
	RefreshConcurrency    int
	RandomDelay           Duration
}

type MediaConfig struct {
	Driver                  string           `yaml:"driver"`
	MaxImageBytes           int64            `yaml:"-"`
	MaxTotalBytes           int64            `yaml:"-"`
	CleanupThresholdPercent int              `yaml:"-"`
	CleanupInterval         Duration         `yaml:"-"`
	Local                   LocalMediaConfig `yaml:"local"`
}

type LocalMediaConfig struct {
	Path string `yaml:"path"`
}

type RoutingConfig struct {
	StickyTTL        Duration `yaml:"stickyTTL"`
	CooldownBase     Duration `yaml:"cooldownBase"`
	CooldownMax      Duration `yaml:"cooldownMax"`
	CapacityWait     Duration `yaml:"capacityWait"`
	MaxAttempts      int      `yaml:"maxAttempts"`
	VideoMaxAttempts int      `yaml:"videoMaxAttempts"`
	PreferFreeBuild  bool     `yaml:"preferFreeBuild"`
	// MarkBuildChatDeniedAsReauth 为 true 时，Build chat 权限拒绝标 reauthRequired，默认 false。
	MarkBuildChatDeniedAsReauth bool     `yaml:"markBuildChatDeniedAsReauth"`
	AccountIsolatedConnections  bool     `yaml:"accountIsolatedConnections"`
	SegmentedSelectorEnabled    bool     `yaml:"segmentedSelectorEnabled"`
	SegmentedMinCandidates      int      `yaml:"segmentedSelectorMinCandidates"`
	SegmentedWindowSize         int      `yaml:"segmentedSelectorWindowSize"`
	ReasoningReplayEnabled      bool     `yaml:"reasoningReplayEnabled"`
	ReasoningReplayTTL          Duration `yaml:"reasoningReplayTTL"`
	ReasoningReplayMaxEntries   int      `yaml:"reasoningReplayMaxEntries"`
}

type AuditConfig struct {
	BufferSize                  int      `yaml:"bufferSize"`
	BatchSize                   int      `yaml:"batchSize"`
	FlushInterval               Duration `yaml:"flushInterval"`
	CommitDelay                 Duration `yaml:"commitDelay"`
	RetentionDays               int      `yaml:"retentionDays"`
	LedgerMode                  string   `yaml:"ledgerMode"`
	LedgerFailureThreshold      int      `yaml:"ledgerFailureThreshold"`
	LedgerUnhealthyGrace        Duration `yaml:"ledgerUnhealthyGrace"`
	LedgerQueueHighWatermarkPct int      `yaml:"ledgerQueueHighWatermarkPercent"`
	// Retention 是审计记录保留时长：超过该时长的 request_audits 及其
	// attempts 明细会被后台任务分批删除（0 = 永久保留，默认）。修改需
	// 重启进程（审计不在运行时设置面内）。非零须在 24h-8760h 之间。
	Retention Duration `yaml:"retention"`
}

// RequestRetryConfig holds the real-time routing guard policy（实时路由守卫，
// 零延迟状态机）：缺少思考证据的流在毫秒内扣留并换号重试，预算耗尽
// Fail-Closed 503。判定与长度阈值无关（正文抢跑即扣留），与 hold 窗口
// 无关（判决性信号即时生效）。
type RequestRetryConfig struct {
	Enabled         bool     `yaml:"enabled"`
	MaxAttempts     int      `yaml:"maxAttempts"`
	OnExhausted     string   `yaml:"onExhausted"`
	AccountCooldown Duration `yaml:"accountCooldown"`

	// GuardedModels 限定守卫介入的模型白名单（空=全部推理模型，向后兼容）。
	// 守卫价值集中在主力推理模型（grok-4.5/4.6）；边缘/退役模型介入只会
	// 产出噪声拦截（grok-4.3 四连 503 实证）。条目匹配 public/
	// upstream 模型名，"grok-4.6" 前缀覆盖 "grok-4.6-xhigh" 档位别名。
	// yaml 级配置，重启生效；管理端保存不会清空该字段。
	GuardedModels []string `yaml:"guardedModels"`

	// SameAccountRetry retries the withholding account once before switching.
	// 仅在旋转代理池出口（每请求换新 IP）下生效：直连/固定出口时网关会
	// 强制忽略该开关——同号重试再次进入同一脏 IP 的恢复概率≈0。
	// 默认开启；显式置 false 关闭。
	SameAccountRetry bool `yaml:"sameAccountRetry"`
	// EvidenceTimeout 是流式请求的零证据截止：静默期超过该时长仍无思考
	// 证据且无任何可见输出时，中止该次上游尝试并按空闲路径重试
	// （0=默认 3.5s）。降智流已被 item.done 零延迟拦截截胡，该截止仅作为
	// 网络假死/静默丢包的防死锁兜底（旧默认 15s 是"死等密文证据"时代的
	// 产物）；干净流首个思考增量 2.1s 即达，3.5s 仍有安全边际。
	EvidenceTimeout Duration `yaml:"evidenceTimeout"`
	// CreatedTimeout 是流式请求的首事件截止：连任何 SSE data 事件（实践中
	// 即 response.created / 首 chunk / message_start）都未到达时，中止该次
	// 上游尝试并重试（0=默认 5s）。仅当 CreatedTimeout 短于 EvidenceTimeout
	// 时比证据截止更早；默认 5s>3.5s 时空流先走证据臂。
	CreatedTimeout Duration `yaml:"createdTimeout"`
	// IdleAccountCooldown 是空流/静默超时的账号冷却（0=默认 15m），独立于
	// missing-thinking 的 AccountCooldown；上下限与 AccountCooldown 相同。
	IdleAccountCooldown Duration `yaml:"idleAccountCooldown"`
}

type ClientKeyDefaultsConfig struct {
	RPMLimit      int `yaml:"rpmLimit"`
	MaxConcurrent int `yaml:"maxConcurrent"`
}

// AccountsConfig 定义可热加载的账号池维护策略；默认全部关闭。
type AccountsConfig struct {
	MarkBuildForbiddenReauth  bool
	BuildForbiddenReauthCodes []string
	// ExcludeBuildBotFlaggedFromScheduling removes Build accounts with bot_flag_source/bfs in {1,2}
	// from scheduling only. Linked Web/Console accounts are unaffected.
	ExcludeBuildBotFlaggedFromScheduling bool
	AutoCleanReauthEnabled               bool
	AutoCleanReauthInterval              Duration
	AutoCleanReauthMinAge                Duration
	AutoCleanIncludeDisabled             bool
}

type Secrets struct {
	JWTSecret string `yaml:"jwtSecret"`
	// CredentialEncryptionKey 是当前主密钥；轮换密钥时把旧密钥列入
	// LegacyEncryptionKeys，存量凭据经历史密钥回退继续可解（新密文一律
	// 用主密钥写入）。
	CredentialEncryptionKey string   `yaml:"credentialEncryptionKey"`
	LegacyEncryptionKeys    []string `yaml:"legacyCredentialEncryptionKeys"`
}

type BootstrapAdminConfig struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

// Duration 支持在 YAML 中使用 10m、1h 等可读时间格式。
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

func (d Duration) Value() time.Duration { return time.Duration(d) }

func (d Duration) String() string {
	value := d.Value().String()
	if strings.HasSuffix(value, "m0s") {
		value = strings.TrimSuffix(value, "0s")
	}
	if strings.HasSuffix(value, "h0m") {
		value = strings.TrimSuffix(value, "0m")
	}
	return value
}

// Load 从 YAML 加载启动配置，并为非敏感运行参数补充代码默认值。
func Load(path string) (Config, error) {
	cfg := defaultConfig()
	loadedFrom := ""
	if strings.TrimSpace(path) != "" {
		data, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Config{}, fmt.Errorf("读取配置文件: %w", err)
		}
		if err == nil {
			loadedFrom = path
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			if err := decoder.Decode(&cfg); err != nil && !errors.Is(err, io.EOF) {
				// 旧配置定向迁移提示:本分支删除了 routing.autoAssign* 键,
				// 从旧版本升级且未清理配置的部署会得到不可读的 unknown field 报错。
				if strings.Contains(err.Error(), "autoAssignMax") {
					return Config{}, fmt.Errorf("解析配置文件: %w（routing.autoAssignMaxNodeShare/autoAssignMaxMigrationShare 已随账号-代理绑定功能删除，请从 config.yaml 中移除这两行）", err)
				}
				return Config{}, fmt.Errorf("解析配置文件: %w", err)
			}
			var extra any
			if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
				if err != nil {
					return Config{}, fmt.Errorf("解析配置文件: %w", err)
				}
				return Config{}, errors.New("配置文件只能包含一个 YAML 文档")
			}
		}
	}
	if loadedFrom != "" {
		if err := resolveRelativePaths(&cfg, loadedFrom); err != nil {
			return Config{}, err
		}
	}
	if err := applyEnvironmentOverrides(&cfg); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// applyEnvironmentOverrides applies typed, application-owned environment
// overrides after YAML and before CLI overrides. Empty values are ignored so
// Compose can pass an optional variable without changing existing deployments.
func applyEnvironmentOverrides(cfg *Config) error {
	value := strings.TrimSpace(os.Getenv(DatabaseURLEnv))
	if value == "" {
		return nil
	}
	dsn, err := validatePostgresEnvironmentURL(value)
	if err != nil {
		return err
	}
	cfg.Database.Driver = "postgres"
	cfg.Database.Postgres.DSN = dsn
	return nil
}

func validatePostgresEnvironmentURL(value string) (string, error) {
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "postgresql+asyncpg://") {
		return "", fmt.Errorf("%s 不支持 SQLAlchemy asyncpg URL；请将 postgresql+asyncpg:// 改为 postgresql://", DatabaseURLEnv)
	}
	if !strings.HasPrefix(lower, "postgres://") && !strings.HasPrefix(lower, "postgresql://") {
		return "", fmt.Errorf("%s 必须使用 postgres:// 或 postgresql:// URL（连接信息已隐藏）", DatabaseURLEnv)
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme == "" || parsed.Fragment != "" {
		return "", fmt.Errorf("%s 不是有效的 PostgreSQL URL（连接信息已隐藏）", DatabaseURLEnv)
	}
	return value, nil
}

func resolveRelativePaths(cfg *Config, configPath string) error {
	absoluteConfigPath, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("解析配置文件路径: %w", err)
	}
	baseDir := filepath.Dir(absoluteConfigPath)
	if cfg.Database.Driver == "sqlite" {
		path := strings.TrimSpace(cfg.Database.SQLite.Path)
		if path != "" && !filepath.IsAbs(path) {
			cfg.Database.SQLite.Path = filepath.Clean(filepath.Join(baseDir, path))
		}
	}
	mediaPath := strings.TrimSpace(cfg.Media.Local.Path)
	if mediaPath != "" && !filepath.IsAbs(mediaPath) {
		cfg.Media.Local.Path = filepath.Clean(filepath.Join(baseDir, mediaPath))
	}
	staticPath := strings.TrimSpace(cfg.Frontend.StaticPath)
	if staticPath != "" && !filepath.IsAbs(staticPath) {
		cfg.Frontend.StaticPath = filepath.Clean(filepath.Join(baseDir, staticPath))
	}
	return nil
}

// Validate 校验启动所需的安全配置和运行边界。
func (c Config) Validate() error {
	if strings.TrimSpace(c.Server.Listen) == "" {
		return errors.New("server.listen 不能为空")
	}
	// 提前到配置校验阶段拒绝畸形地址：此前空格以外的任何值都要等到
	// ListenAndServe 才失败，报错没有字段名（round 82 活体复现：
	// "not-an-addr"/"127.0.0.1" 缺端口到 listen tcp 才报）。
	if host, portText, err := net.SplitHostPort(c.Server.Listen); err != nil {
		return fmt.Errorf("server.listen %q 必须是 host:port 形式", c.Server.Listen)
	} else if port, portErr := strconv.Atoi(portText); portErr != nil || port < 1 || port > 65535 {
		return fmt.Errorf("server.listen 端口无效: %q（host=%s）", portText, host)
	}
	if c.Server.MaxBodyBytes <= 0 || c.Server.MaxBodyBytes > maxServerBodyBytes {
		return fmt.Errorf("server.maxBodyBytes 必须在 1 到 %d 字节之间", maxServerBodyBytes)
	}
	if c.Server.ReadTimeout.Value() <= 0 || c.Server.ReadTimeout.Value() > maxReadTimeout {
		return errors.New("server.readTimeout 必须大于零且不超过 1 小时")
	}
	if c.Server.RequestTimeout.Value() <= 0 || c.Server.RequestTimeout.Value() > maxRequestTimeout {
		return errors.New("server.requestTimeout 必须大于零且不超过 24 小时")
	}
	if c.Server.MaxConcurrentRequests < 1 || c.Server.MaxConcurrentRequests > 100000 {
		return errors.New("server.maxConcurrentRequests 必须在 1 到 100000 之间")
	}
	for _, value := range c.Server.TrustedProxies {
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			return errors.New("server.trustedProxies 不能包含空值")
		}
		if trimmed != value {
			return fmt.Errorf("server.trustedProxies %q 不能包含首尾空白", value)
		}
		if net.ParseIP(trimmed) != nil {
			continue
		}
		_, network, err := net.ParseCIDR(trimmed)
		if err != nil {
			return fmt.Errorf("server.trustedProxies %q 必须是 IP 或 CIDR", value)
		}
		if ones, _ := network.Mask.Size(); ones == 0 {
			return fmt.Errorf("server.trustedProxies %q 不能信任整个互联网", value)
		}
	}
	for _, item := range []struct {
		name  string
		value string
	}{
		{name: "frontend.publicApiBaseURL", value: c.Frontend.PublicAPIBaseURL},
		{name: "frontend.publicApiBaseURL 运行设置", value: c.Frontend.PublicAPIBaseURLOverride},
	} {
		if publicBase := strings.TrimSpace(item.value); publicBase != "" {
			publicAPIURL, err := url.ParseRequestURI(publicBase)
			if err != nil || (publicAPIURL.Scheme != "http" && publicAPIURL.Scheme != "https") || publicAPIURL.Host == "" || publicAPIURL.User != nil || publicAPIURL.RawQuery != "" || publicAPIURL.Fragment != "" {
				return fmt.Errorf("%s 必须是不含凭据、查询参数和片段的 HTTP(S) URL", item.name)
			}
		}
	}
	switch c.Database.Driver {
	case "sqlite":
		if strings.TrimSpace(c.Database.SQLite.Path) == "" {
			return errors.New("database.sqlite.path 不能为空")
		}
	case "postgres":
		if strings.TrimSpace(c.Database.Postgres.DSN) == "" {
			return errors.New("database.postgres.dsn 不能为空")
		}
		if c.Database.Postgres.MaxOpenConns < 1 || c.Database.Postgres.MaxOpenConns > 1000 {
			return errors.New("database.postgres.maxOpenConns 必须在 1 到 1000 之间")
		}
		if c.Database.Postgres.MaxIdleConns < 0 || c.Database.Postgres.MaxIdleConns > c.Database.Postgres.MaxOpenConns {
			return errors.New("database.postgres.maxIdleConns 必须在 0 到 maxOpenConns 之间")
		}
	default:
		return errors.New("database.driver 必须是 sqlite 或 postgres")
	}
	switch c.RuntimeStore.Driver {
	case "memory":
	case "redis":
		if strings.TrimSpace(c.RuntimeStore.Redis.Address) == "" {
			return errors.New("runtimeStore.redis.address 不能为空")
		}
		if c.RuntimeStore.Redis.Database < 0 || c.RuntimeStore.Redis.Database > 1024 {
			return errors.New("runtimeStore.redis.database 必须在 0 到 1024 之间")
		}
		if prefix := strings.TrimSpace(c.RuntimeStore.Redis.KeyPrefix); prefix == "" || len(prefix) > 128 {
			return errors.New("runtimeStore.redis.keyPrefix 必须在 1 到 128 个字符之间")
		}
	default:
		return errors.New("runtimeStore.driver 必须是 memory 或 redis")
	}
	if c.Deployment.Replicas < 1 || c.Deployment.Replicas > maxDeploymentReplicas {
		return fmt.Errorf("deployment.replicas 必须在 1 到 %d 之间", maxDeploymentReplicas)
	}
	if c.Deployment.Replicas > 1 {
		if c.Database.Driver != "postgres" {
			return errors.New("多实例部署必须使用 PostgreSQL")
		}
		if c.RuntimeStore.Driver != "redis" {
			return errors.New("多实例部署必须使用 Redis 运行态存储")
		}
		if strings.TrimSpace(c.Deployment.InstanceID) == "" {
			return errors.New("多实例部署必须配置 deployment.instanceID")
		}
		if strings.TrimSpace(c.Deployment.ClusterID) == "" {
			return errors.New("多实例部署必须配置 deployment.clusterID")
		}
		if !c.Deployment.SharedMedia {
			return errors.New("多实例部署必须确认 deployment.sharedMedia=true 并挂载共享媒体目录")
		}
	}
	if c.Media.Driver != "local" {
		return errors.New("media.driver 当前仅支持 local")
	}
	if strings.TrimSpace(c.Media.Local.Path) == "" {
		return errors.New("media.local.path 不能为空")
	}
	if c.Media.MaxImageBytes < 1<<20 || c.Media.MaxImageBytes > 32<<20 {
		return errors.New("media.maxImageBytes 必须在 1 MiB 到 32 MiB 之间")
	}
	if c.Media.MaxTotalBytes < c.Media.MaxImageBytes || c.Media.MaxTotalBytes > 1<<40 {
		return errors.New("media.maxTotalBytes 必须不小于单图上限且不超过 1 TiB")
	}
	if c.Media.CleanupThresholdPercent < 50 || c.Media.CleanupThresholdPercent > 95 {
		return errors.New("media.cleanupThresholdPercent 必须在 50 到 95 之间")
	}
	if c.Media.CleanupInterval.Value() < time.Minute || c.Media.CleanupInterval.Value() > 24*time.Hour {
		return errors.New("media.cleanupInterval 必须在 1 分钟到 24 小时之间")
	}
	if len(c.Secrets.JWTSecret) < 32 {
		return errors.New("secrets.jwtSecret 至少需要 32 个字符")
	}
	if isExampleSecret(c.Secrets.JWTSecret) {
		return errors.New("secrets.jwtSecret 不能使用示例占位值")
	}
	if !validCredentialEncryptionKey(c.Secrets.CredentialEncryptionKey) {
		return errors.New("secrets.credentialEncryptionKey 必须是 Base64 编码的 32 字节密钥")
	}
	for index, key := range c.Secrets.LegacyEncryptionKeys {
		if strings.TrimSpace(key) == "" {
			continue
		}
		if !validCredentialEncryptionKey(key) {
			return fmt.Errorf("secrets.legacyCredentialEncryptionKeys[%d] 必须是 Base64 编码的 32 字节密钥", index)
		}
		if key == c.Secrets.CredentialEncryptionKey {
			return fmt.Errorf("secrets.legacyCredentialEncryptionKeys[%d] 与当前主密钥相同（无意义且掩盖配置错误）", index)
		}
	}
	if isExampleSecret(c.BootstrapAdmin.Password) {
		return errors.New("bootstrapAdmin.password 不能使用示例占位值")
	}
	if c.Auth.AccessTokenTTL.Value() <= 0 || c.Auth.RefreshTokenTTL.Value() <= 0 {
		return errors.New("JWT 有效期必须大于零")
	}
	if err := validateAPIBaseURL("provider.build.baseURL", c.Provider.Build.BaseURL, false); err != nil {
		return err
	}
	fallbackBase := strings.TrimSpace(c.Provider.Build.FallbackBaseURL)
	if fallbackBase == "" {
		fallbackBase = DefaultBuildFallbackBaseURL
	}
	if err := validateAPIBaseURL("provider.build.fallbackBaseURL", fallbackBase, true); err != nil {
		return err
	}
	if strings.TrimSpace(c.Provider.Build.ClientVersion) == "" || strings.TrimSpace(c.Provider.Build.ClientIdentifier) == "" || strings.TrimSpace(c.Provider.Build.TokenAuth) == "" || strings.TrimSpace(c.Provider.Build.UserAgent) == "" {
		return errors.New("provider.build 客户端标识不能为空")
	}
	if timeout := c.Provider.Build.ResponseHeaderTimeout.Value(); timeout < settingsdomain.MinBuildResponseHeaderTimeout || timeout > settingsdomain.MaxBuildResponseHeaderTimeout {
		return errors.New("Grok Build 响应头超时必须在 30 秒到 30 分钟之间")
	}
	if idle := c.Provider.Build.StreamIdleTimeout.Value(); idle < settingsdomain.MinBuildStreamIdleTimeout || idle > settingsdomain.MaxBuildStreamIdleTimeout {
		return errors.New("Grok Build 流式空闲超时必须在 30 秒到 10 分钟之间")
	}
	webURL, err := url.ParseRequestURI(strings.TrimSpace(c.Provider.Web.BaseURL))
	if err != nil || webURL.Scheme != "https" || webURL.Host == "" || webURL.User != nil {
		return errors.New("provider.web.baseURL 必须是无凭据的 HTTPS URL")
	}
	switch c.Provider.Web.StatsigMode {
	case StatsigModeManual:
		if !validStatsigID(c.Provider.Web.StatsigManualValue) {
			return errors.New("provider.web 手动 x-statsig-id 格式无效")
		}
	case StatsigModeURL:
		if err := signerurl.Validate(c.Provider.Web.StatsigSignerURL); err != nil {
			return fmt.Errorf("provider.web Statsig 签名 URL 无效: %w", err)
		}
	default:
		return errors.New("provider.web Statsig 模式必须是 manual 或 url")
	}
	switch c.Provider.Web.ClearanceMode {
	case ClearanceModeManual:
	case ClearanceModeFlareSolverr, ClearanceModeOnDemand:
		if err := validateFlareSolverrURL(c.Provider.Web.FlareSolverrURL); err != nil {
			return fmt.Errorf("provider.web FlareSolverr URL 无效: %w", err)
		}
	default:
		return errors.New("provider.web Clearance 模式必须是 manual、flaresolverr 或 on_demand")
	}
	if c.Provider.Web.ClearanceTimeout.Value() < 10*time.Second || c.Provider.Web.ClearanceTimeout.Value() > 5*time.Minute {
		return errors.New("provider.web Clearance 超时必须在 10 秒到 5 分钟之间")
	}
	if c.Provider.Web.ClearanceRefresh.Value() < time.Minute || c.Provider.Web.ClearanceRefresh.Value() > 24*time.Hour {
		return errors.New("provider.web Clearance 刷新间隔必须在 1 分钟到 24 小时之间")
	}
	if c.Provider.Web.QuotaTimeout.Value() < time.Second || c.Provider.Web.QuotaTimeout.Value() > 2*time.Minute {
		return errors.New("provider.web.quotaTimeout 必须在 1 秒到 2 分钟之间")
	}
	if c.Provider.Web.ChatTimeout.Value() < 5*time.Second || c.Provider.Web.ChatTimeout.Value() > 30*time.Minute {
		return errors.New("provider.web.chatTimeout 必须在 5 秒到 30 分钟之间")
	}
	if c.Provider.Web.ImageTimeout.Value() < 5*time.Second || c.Provider.Web.ImageTimeout.Value() > 30*time.Minute {
		return errors.New("provider.web.imageTimeout 必须在 5 秒到 30 分钟之间")
	}
	if c.Provider.Web.VideoTimeout.Value() < time.Minute || c.Provider.Web.VideoTimeout.Value() > 2*time.Hour {
		return errors.New("provider.web.videoTimeout 必须在 1 分钟到 2 小时之间")
	}
	if idle := c.Provider.Web.StreamIdleTimeout.Value(); idle < settingsdomain.MinProviderStreamIdleTimeout || idle > settingsdomain.MaxProviderStreamIdleTimeout {
		return errors.New("Grok Web 流式空闲超时必须在 30 秒到 10 分钟之间")
	}
	if c.Provider.Web.MediaConcurrency < 1 || c.Provider.Web.MediaConcurrency > 64 {
		return errors.New("provider.web 媒体并发必须在 1 到 64 之间")
	}
	consoleURL, err := url.ParseRequestURI(strings.TrimSpace(c.Provider.Console.BaseURL))
	if err != nil || consoleURL.Scheme != "https" || consoleURL.Host == "" || consoleURL.User != nil {
		return errors.New("provider.console.baseURL 必须是无凭据的 HTTPS URL")
	}
	if c.Provider.Console.ChatTimeout.Value() < 5*time.Second || c.Provider.Console.ChatTimeout.Value() > 30*time.Minute {
		return errors.New("provider.console.chatTimeout 必须在 5 秒到 30 分钟之间")
	}
	if idle := c.Provider.Console.StreamIdleTimeout.Value(); idle < settingsdomain.MinProviderStreamIdleTimeout || idle > settingsdomain.MaxProviderStreamIdleTimeout {
		return errors.New("Grok Console 流式空闲超时必须在 30 秒到 10 分钟之间")
	}
	if c.Batch.ImportConcurrency < 1 || c.Batch.ImportConcurrency > 50 ||
		c.Batch.ConversionConcurrency < 1 || c.Batch.ConversionConcurrency > 50 ||
		c.Batch.SyncConcurrency < 1 || c.Batch.SyncConcurrency > 50 ||
		c.Batch.RefreshConcurrency < 1 || c.Batch.RefreshConcurrency > 50 {
		return errors.New("批量任务并发必须在 1 到 50 之间")
	}
	if c.Batch.RandomDelay.Value() < 0 || c.Batch.RandomDelay.Value() > 5*time.Second {
		return errors.New("批量任务随机延迟必须在 0 到 5 秒之间")
	}
	if c.Provider.Web.RecoveryBackoffBase.Value() < 5*time.Second || c.Provider.Web.RecoveryBackoffMax.Value() < c.Provider.Web.RecoveryBackoffBase.Value() || c.Provider.Web.RecoveryBackoffMax.Value() > 6*time.Hour {
		return errors.New("provider.web 恢复退避配置无效")
	}
	// routing 各约束拆分校验并指明具体字段：此前 9 个条件合并为一条
	// 「routing 配置无效」，运维只能通读整块代码定位是哪个字段越界。
	if c.Routing.StickyTTL.Value() <= 0 || c.Routing.StickyTTL.Value() > maxRoutingTTL {
		return fmt.Errorf("routing.stickyTTL 必须在 1 纳秒到 %s 之间", maxRoutingTTL)
	}
	if c.Routing.CooldownBase.Value() <= 0 || c.Routing.CooldownMax.Value() < c.Routing.CooldownBase.Value() || c.Routing.CooldownMax.Value() > maxRoutingCooldown {
		return errors.New("routing.cooldownBase/cooldownMax 配置无效: 需要 0 < cooldownBase <= cooldownMax <= 30m")
	}
	if c.Routing.CapacityWait.Value() <= 0 || c.Routing.CapacityWait.Value() > maxRoutingCapacityWait {
		return fmt.Errorf("routing.capacityWait 必须在 1 纳秒到 %s 之间", maxRoutingCapacityWait)
	}
	if c.Routing.MaxAttempts < unlimitedRoutingAttempts || c.Routing.MaxAttempts == 0 || c.Routing.MaxAttempts > maxRoutingAttempts {
		return errors.New("routing.maxAttempts 必须是 -1(不限)、1 到 65535；0 不被接受")
	}
	if c.Routing.VideoMaxAttempts < unlimitedRoutingAttempts || c.Routing.VideoMaxAttempts > maxRoutingAttempts {
		return errors.New("routing.videoMaxAttempts 必须是 -1(不限)、0(默认 3)或 1 到 65535")
	}
	if c.Routing.SegmentedMinCandidates < 100 || c.Routing.SegmentedMinCandidates > 1000000 {
		return errors.New("routing.segmentedMinCandidates 必须在 100 到 1000000 之间")
	}
	if c.Routing.SegmentedWindowSize < 8 || c.Routing.SegmentedWindowSize > 256 || c.Routing.SegmentedWindowSize > c.Routing.SegmentedMinCandidates {
		return errors.New("routing.segmentedWindowSize 必须在 8 到 256 之间且不超过 segmentedMinCandidates")
	}
	if c.Routing.ReasoningReplayTTL.Value() <= 0 || c.Routing.ReasoningReplayTTL.Value() > 24*time.Hour {
		return errors.New("routing.reasoningReplayTTL 必须在 1 纳秒到 24 小时之间")
	}
	if c.Routing.ReasoningReplayMaxEntries < 100 || c.Routing.ReasoningReplayMaxEntries > 1000000 {
		return errors.New("routing.reasoningReplayMaxEntries 必须在 100 到 1000000 之间")
	}
	if c.Audit.BufferSize < 1 || c.Audit.BufferSize > maxAuditBufferSize {
		return errors.New("audit.bufferSize 必须在 1 到 100000 之间")
	}
	if c.Audit.BatchSize < 1 || c.Audit.BatchSize > maxAuditBatchSize || c.Audit.BatchSize > c.Audit.BufferSize {
		return errors.New("audit.batchSize 必须在 1 到 1000 之间且不超过 bufferSize")
	}
	if c.Audit.FlushInterval.Value() < minAuditFlushInterval || c.Audit.FlushInterval.Value() > maxAuditFlushInterval {
		return errors.New("audit.flushInterval 必须在 50ms 到 60s 之间")
	}
	if c.Audit.CommitDelay.Value() < minAuditCommitDelay || c.Audit.CommitDelay.Value() > maxAuditCommitDelay {
		return errors.New("audit.commitDelay 必须在 1ms 到 50ms 之间")
	}

	if d := c.Audit.Retention.Value(); d != 0 && (d < 24*time.Hour || d > 8760*time.Hour) {
		return errors.New("audit.retention 必须在 24h 到 8760h 之间（0 表示永久保留）")
	}
	if c.Audit.RetentionDays < 0 || c.Audit.RetentionDays > 365 {
		return errors.New("audit.retentionDays 必须在 0 到 365 之间")
	}
	if c.Audit.LedgerMode != "observe" && c.Audit.LedgerMode != "enforce" {
		return errors.New("audit.ledgerMode 必须是 observe 或 enforce")
	}
	if c.Audit.LedgerFailureThreshold < 1 || c.Audit.LedgerFailureThreshold > 100 {
		return errors.New("audit.ledgerFailureThreshold 必须在 1 到 100 之间")
	}
	if c.Audit.LedgerUnhealthyGrace.Value() < time.Second || c.Audit.LedgerUnhealthyGrace.Value() > 10*time.Minute {
		return errors.New("audit.ledgerUnhealthyGrace 必须在 1 秒到 10 分钟之间")
	}
	if c.Audit.LedgerQueueHighWatermarkPct < 50 || c.Audit.LedgerQueueHighWatermarkPct > 100 {
		return errors.New("audit.ledgerQueueHighWatermarkPercent 必须在 50 到 100 之间")
	}
	if err := validateRequestRetry(c.RequestRetry); err != nil {
		return err
	}
	if err := c.AccountRisk.Validate(); err != nil {
		return err
	}
	if err := c.Egress.Validate(); err != nil {
		return err
	}
	if c.ClientKeyDefaults.RPMLimit < 1 || c.ClientKeyDefaults.RPMLimit > clientkeydomain.MaxRPMLimit || c.ClientKeyDefaults.MaxConcurrent < 1 || c.ClientKeyDefaults.MaxConcurrent > clientkeydomain.MaxConcurrent {
		return errors.New("clientKeyDefaults 超出允许范围")
	}
	if c.Accounts.AutoCleanReauthInterval.Value() < time.Minute || c.Accounts.AutoCleanReauthInterval.Value() > time.Hour {
		return errors.New("accounts.autoCleanReauthInterval 必须在 1 分钟到 1 小时之间")
	}
	if c.Accounts.AutoCleanReauthMinAge.Value() < time.Minute || c.Accounts.AutoCleanReauthMinAge.Value() > 30*24*time.Hour {
		return errors.New("accounts.autoCleanReauthMinAge 必须在 1 分钟到 30 天之间")
	}
	if len(c.Accounts.BuildForbiddenReauthCodes) > 32 {
		return errors.New("accounts.buildForbiddenReauthCodes 最多支持 32 个错误码")
	}
	for _, code := range c.Accounts.BuildForbiddenReauthCodes {
		if !buildForbiddenCodePattern.MatchString(strings.TrimSpace(code)) {
			return errors.New("accounts.buildForbiddenReauthCodes 包含无效错误码")
		}
	}
	if len(c.Accounts.BuildForbiddenReauthCodes) == 0 {
		return errors.New("accounts.buildForbiddenReauthCodes 至少需要一个错误码")
	}
	return nil
}

func validateRequestRetry(value RequestRetryConfig) error {
	if !value.Enabled {
		return nil
	}
	// 预算是安全属性而非调优旋钮（蓝图 §3.2）：上限 3 = 默认 2（初始+1 次
	// 重试）留一档旋转池同号重试余量；历史上限 6 会重建零延迟拦截前的
	// 90-120s 串行黑洞性时延。
	if value.MaxAttempts != 0 && (value.MaxAttempts < 1 || value.MaxAttempts > 3) {
		return errors.New("requestRetry.maxAttempts 必须在 1 到 3 之间（全局请求预算上限，默认 2）")
	}
	switch strings.TrimSpace(value.OnExhausted) {
	case "", "fail_open", "fail_closed":
	default:
		return errors.New("requestRetry.onExhausted 必须是 fail_open 或 fail_closed")
	}
	if len(value.GuardedModels) > 32 {
		return errors.New("requestRetry.guardedModels 最多 32 个条目")
	}
	for _, name := range value.GuardedModels {
		if strings.TrimSpace(name) == "" {
			return errors.New("requestRetry.guardedModels 含空条目")
		}
	}
	if d := value.AccountCooldown.Value(); d != 0 && (d < time.Minute || d > 168*time.Hour) {
		return errors.New("requestRetry.accountCooldown 必须在 1m 到 168h 之间")
	}
	if d := value.IdleAccountCooldown.Value(); d != 0 && (d < time.Minute || d > 168*time.Hour) {
		return errors.New("requestRetry.idleAccountCooldown 必须在 1m 到 168h 之间（0 表示默认 15m）")
	}
	if d := value.EvidenceTimeout.Value(); d != 0 && (d < time.Second || d > 5*time.Minute) {
		return errors.New("requestRetry.evidenceTimeout 必须在 1s 到 5m 之间（0 表示默认 3.5s）")
	}
	if d := value.CreatedTimeout.Value(); d != 0 && (d < time.Second || d > 2*time.Minute) {
		return errors.New("requestRetry.createdTimeout 必须在 1s 到 2m 之间（0 表示默认 5s）")
	}
	if d := value.IdleAccountCooldown.Value(); d != 0 && (d < time.Minute || d > 168*time.Hour) {
		return errors.New("qualityGuard.requestRetry.idleAccountCooldown 必须在 1m 到 168h 之间")
	}
	return nil
}

// validateAPIBaseURL 仅允许无凭据、query、fragment 的 HTTP(S) API 根地址。
// requireHTTPS 为 true 时强制 HTTPS（用于生产默认 XAI 备用地址）。
func validateAPIBaseURL(name, raw string, requireHTTPS bool) error {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s 必须是不含凭据、查询参数和片段的 HTTP(S) URL", name)
	}
	switch parsed.Scheme {
	case "https":
		return nil
	case "http":
		if requireHTTPS {
			return fmt.Errorf("%s 必须是 HTTPS URL", name)
		}
		return nil
	default:
		return fmt.Errorf("%s 必须是不含凭据、查询参数和片段的 HTTP(S) URL", name)
	}
}

// NormalizeBuildFallbackBaseURL 在旧配置缺字段时填入默认 XAI 备用地址。
func NormalizeBuildFallbackBaseURL(value string) string {
	if strings.TrimSpace(value) == "" {
		return DefaultBuildFallbackBaseURL
	}
	return strings.TrimSpace(value)
}

func defaultConfig() Config {
	return Config{
		Server: ServerConfig{
			Listen:                "127.0.0.1:8000",
			MaxBodyBytes:          32 << 20,
			MaxConcurrentRequests: 1024,
			ReadTimeout:           Duration(15 * time.Minute),
			RequestTimeout:        Duration(2 * time.Hour),
		},
		Frontend: FrontendConfig{PublicAPIBaseURL: DefaultPublicAPIBaseURL, StaticPath: "./frontend/dist"},
		Database: DatabaseConfig{
			Driver:   "sqlite",
			SQLite:   SQLiteDatabaseConfig{Path: "./data/backend.db"},
			Postgres: PostgresDatabaseConfig{MaxOpenConns: 50, MaxIdleConns: 10},
		},
		RuntimeStore: RuntimeStoreConfig{
			Driver: "memory",
			Redis:  RedisRuntimeConfig{Address: "127.0.0.1:6379", KeyPrefix: "grok2api:"},
		},
		Deployment: DeploymentConfig{Replicas: 1, ClusterID: "grok2api"},
		Auth: AuthConfig{
			AccessTokenTTL:  Duration(15 * time.Minute),
			RefreshTokenTTL: Duration(30 * 24 * time.Hour),
		},
		Provider: ProviderConfig{
			Build: BuildProviderConfig{
				BaseURL: "https://cli-chat-proxy.grok.com/v1", FallbackBaseURL: DefaultBuildFallbackBaseURL,
				ClientVersion: RecommendedBuildClientVersion, ClientIdentifier: "grok-shell", TokenAuth: "xai-grok-cli",
				UserAgent: RecommendedBuildUserAgent, ResponseHeaderTimeout: Duration(settingsdomain.DefaultBuildResponseHeaderTimeout),
				StreamIdleTimeout: Duration(settingsdomain.DefaultBuildStreamIdleTimeout),
			},
			Web: WebProviderConfig{
				BaseURL: "https://grok.com", StatsigMode: StatsigModeURL, StatsigSignerURL: DefaultStatsigSignerURL,
				ClearanceMode: ClearanceModeManual, FlareSolverrURL: DefaultFlareSolverrURL,
				ClearanceTimeout: Duration(time.Minute), ClearanceRefresh: Duration(10 * time.Minute),
				QuotaTimeout: Duration(25 * time.Second),
				ChatTimeout:  Duration(2 * time.Minute), StreamIdleTimeout: Duration(settingsdomain.DefaultWebStreamIdleTimeout),
				ImageTimeout:     Duration(3 * time.Minute),
				VideoTimeout:     Duration(15 * time.Minute),
				MediaConcurrency: 4, RecoveryBackoffBase: Duration(30 * time.Second),
				RecoveryBackoffMax: Duration(30 * time.Minute),
			},
			Console: ConsoleProviderConfig{BaseURL: "https://console.x.ai", ChatTimeout: Duration(5 * time.Minute), StreamIdleTimeout: Duration(settingsdomain.DefaultConsoleStreamIdleTimeout)},
		},
		Batch: BatchConfig{
			ImportConcurrency: 25, ConversionConcurrency: 25, SyncConcurrency: 25,
			RefreshConcurrency: 25, RandomDelay: Duration(500 * time.Millisecond),
		},
		Egress: DefaultEgressConfig(),
		Media: MediaConfig{
			Driver: "local", MaxImageBytes: 32 << 20, MaxTotalBytes: 1 << 30,
			CleanupThresholdPercent: 80, CleanupInterval: Duration(10 * time.Minute),
			Local: LocalMediaConfig{Path: "./data/media"},
		},
		Routing: RoutingConfig{
			StickyTTL:                   Duration(time.Hour),
			CooldownBase:                Duration(30 * time.Second),
			CooldownMax:                 Duration(30 * time.Minute),
			CapacityWait:                Duration(500 * time.Millisecond),
			MaxAttempts:                 999,
			VideoMaxAttempts:            999,
			MarkBuildChatDeniedAsReauth: false,
			PreferFreeBuild:             false,
			AccountIsolatedConnections:  false,
			SegmentedSelectorEnabled:    true,
			SegmentedMinCandidates:      3000,
			SegmentedWindowSize:         64,
			ReasoningReplayEnabled:      true,
			ReasoningReplayTTL:          Duration(time.Hour),
			ReasoningReplayMaxEntries:   10240,
		},
		Audit: AuditConfig{
			BufferSize: 16384, BatchSize: 256, FlushInterval: Duration(250 * time.Millisecond), CommitDelay: Duration(5 * time.Millisecond),
			RetentionDays: 7,
			LedgerMode:    "enforce", LedgerFailureThreshold: 1,
			LedgerUnhealthyGrace: Duration(10 * time.Second), LedgerQueueHighWatermarkPct: 90,
		},

		AccountRisk: DefaultAccountRiskConfig(),
		RequestRetry: RequestRetryConfig{
			MaxAttempts: 2, OnExhausted: "fail_closed", SameAccountRetry: true,
			AccountCooldown: Duration(24 * time.Hour), EvidenceTimeout: Duration(3500 * time.Millisecond), CreatedTimeout: Duration(5 * time.Second)},
		ClientKeyDefaults: ClientKeyDefaultsConfig{RPMLimit: clientkeydomain.DefaultRPMLimit, MaxConcurrent: clientkeydomain.DefaultMaxConcurrent},
		Accounts: AccountsConfig{
			MarkBuildForbiddenReauth:             false,
			BuildForbiddenReauthCodes:            []string{"permission-denied"},
			ExcludeBuildBotFlaggedFromScheduling: false,
			AutoCleanReauthEnabled:               false,
			AutoCleanReauthInterval:              Duration(10 * time.Minute),
			AutoCleanReauthMinAge:                Duration(time.Hour),
			AutoCleanIncludeDisabled:             false,
		},
	}
}

func validateFlareSolverrURL(value string) error {
	if err := signerurl.Validate(value); err != nil {
		return errors.New(strings.ReplaceAll(err.Error(), "签名 URL", "URL"))
	}
	return nil
}

func validStatsigID(value string) bool {
	value = strings.TrimSpace(value)
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 70
}

func validCredentialEncryptionKey(value string) bool {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	return err == nil && len(decoded) == 32
}

func isExampleSecret(value string) bool {
	switch strings.TrimSpace(value) {
	case "replace-with-at-least-32-characters", "replace-with-base64-key", "replace-with-a-strong-password":
		return true
	default:
		return false
	}
}
