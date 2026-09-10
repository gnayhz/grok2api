package settings

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput = errors.New("运行设置参数无效")
	ErrConflict     = errors.New("运行设置已被其他会话更新")
	ErrApplyPending = errors.New("运行设置已保存，本实例仍有目标待应用")
)

// ProviderBuildConfig 是管理接口使用的 Provider 可编辑输入。
type ProviderBuildConfig struct {
	BaseURL               string
	FallbackBaseURL       string
	ClientVersion         string
	ClientIdentifier      string
	TokenAuth             string
	UserAgent             string
	ResponseHeaderTimeout string
	StreamIdleTimeout     string
}

// ProviderBuildRecommendation 表示当前网关已完成兼容回归的 Grok Build 协议基线。
type ProviderBuildRecommendation struct {
	ClientVersion string
	UserAgent     string
}

type ProviderWebConfig struct {
	BaseURL                 string
	StatsigMode             string
	StatsigManualValue      string
	StatsigManualConfigured bool
	StatsigSignerURL        string
	ClearanceMode           string
	FlareSolverrURL         string
	ClearanceTimeout        string
	ClearanceRefresh        string
	QuotaTimeout            string
	ChatTimeout             string
	StreamIdleTimeout       string
	ImageTimeout            string
	VideoTimeout            string
	MediaConcurrency        int
	AllowNSFW               bool
	RecoveryBackoffBase     string
	RecoveryBackoffMax      string
	// ClearanceProvided distinguishes older admin clients that predate the
	// managed-clearance fields from an explicit update to those fields.
	ClearanceProvided bool
}

type ProviderConsoleConfig struct {
	BaseURL           string
	ChatTimeout       string
	StreamIdleTimeout string
}

// ServerConfig 是管理接口使用的推理入口容量输入。
type ServerConfig struct {
	MaxConcurrentRequests int
}

// BatchConfig 是管理接口使用的批量任务并发输入。
type BatchConfig struct {
	ImportConcurrency     int
	ConversionConcurrency int
	SyncConcurrency       int
	RefreshConcurrency    int
	RandomDelay           string
}

type MediaConfig struct {
	MaxImageBytes           int64
	MaxTotalBytes           int64
	CleanupThresholdPercent int
	CleanupInterval         string
}

// FrontendConfig 是管理接口使用的公开 API 地址输入。
type FrontendConfig struct {
	PublicAPIBaseURL string
}

// RoutingConfig 是管理接口使用的路由可编辑输入。
type RoutingConfig struct {
	StickyTTL                           string
	CooldownBase                        string
	CooldownMax                         string
	CapacityWait                        string
	MaxAttempts                         int
	VideoMaxAttempts                    int
	PreferFreeBuild                     bool
	MarkBuildChatDeniedAsReauth         bool
	MarkBuildChatDeniedAsReauthProvided bool
	AccountIsolatedConnections          bool
	// AccountIsolatedConnectionsProvided preserves the current value when an
	// older management client omits the newly added field.
	AccountIsolatedConnectionsProvided bool
	SegmentedSelector                  SegmentedSelectorConfig
	SegmentedSelectorProvided          bool
}

type SegmentedSelectorConfig struct {
	Enabled       bool
	MinCandidates int
	WindowSize    int
}

// AuditConfig 是管理接口使用的审计可编辑输入。
type AuditConfig struct {
	BufferSize              int
	BatchSize               int
	FlushInterval           string
	CommitDelayMS           int
	RetentionPeriod         string
	RetentionPeriodProvided bool
	RetentionSource         string
	FileRetentionPeriod     string
	FileRetentionSource     string
	RetentionDays           int
	RetentionDaysProvided   bool
}

// ClientKeyDefaultsConfig 是管理接口使用的密钥默认限制输入。
type ClientKeyDefaultsConfig struct {
	RPMLimit      int
	MaxConcurrent int
}

// AccountsConfig 是管理接口使用的账号池维护策略输入。
type AccountsConfig struct {
	MarkBuildForbiddenReauth  bool
	BuildForbiddenReauthCodes []string
	// ExcludeBuildBotFlaggedFromScheduling drops bot-risk Build accounts from scheduling only.
	ExcludeBuildBotFlaggedFromScheduling bool
	AutoCleanReauthEnabled               bool
	AutoCleanReauthInterval              string
	AutoCleanReauthMinAge                string
	AutoCleanIncludeDisabled             bool
	// MarkBuildForbiddenReauthProvided preserves the value when an older management client omits the field.
	MarkBuildForbiddenReauthProvided bool
	// BuildForbiddenReauthCodesProvided preserves the configured codes when an older management client omits the field.
	BuildForbiddenReauthCodesProvided bool
	// ExcludeBuildBotFlaggedFromSchedulingProvided preserves the value when an older management client omits the field.
	ExcludeBuildBotFlaggedFromSchedulingProvided bool
}

// RequestRetryEditable 是管理接口使用的实时路由守卫输入（时长为字符串）。
type RequestRetryEditable struct {
	Enabled             bool
	MaxAttempts         int
	OnExhausted         string
	AccountCooldown     string
	EvidenceTimeout     string
	CreatedTimeout      string
	IdleAccountCooldown string
}

// EgressRotationEditable 是管理接口使用的出口轮换输入（时长为字符串）。
type EgressRotationEditable struct {
	Enabled                  bool
	MaxAttemptsPerQuarantine int
	MinNodeInterval          string
	MaxGlobalPerHour         int
	WebhookTimeout           string
	WebhookRetries           int
	SettleDelay              string
	ProbeTimeout             string
	ProbeInterval            string
}

// EditableConfig 聚合管理端允许修改的运行参数。
type EditableConfig struct {
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
	// AccountsProvided 区分旧管理端未发送 accounts 与显式提交默认值。
	AccountsProvided bool
	RequestRetry     RequestRetryEditable
	// RequestRetryProvided 区分旧管理端未发送该节与显式提交零值。
	RequestRetryProvided bool
	EgressRotation       EgressRotationEditable
	// EgressRotationProvided 同上。
	EgressRotationProvided bool
}

// Snapshot 表示当前运行设置和需要重启才能生效的字段。
type Snapshot struct {
	Config                   EditableConfig
	RecommendedProviderBuild ProviderBuildRecommendation
	UpdatedAt                time.Time
	Revision                 uint64
	AppliedRevision          uint64
	ApplyPending             bool
	ApplyTargets             []ApplyStatus
	Notification             NotificationStatus
	RestartRequired          []string
	// FileRequestRetry 是文件配置基线的 requestRetry 节:设置页"运行时覆盖
	// 标记+回同步文件值"的数据源。与 Config.RequestRetry 不同即处于覆盖态
	// ——历史事故中该覆盖静默发生(守卫开机离场且无任何提示)。
	FileRequestRetry RequestRetryEditable
}

// Service 管理允许在线修改的配置，并向后台任务广播配置变更。
type Service struct {
	mu                     sync.RWMutex
	updateMu               sync.Mutex
	cfg                    config.Config
	updatedAt              time.Time
	revision               uint64
	lastAppliedRevision    uint64
	targets                []ApplyTarget
	applyStates            []ApplyStatus
	notification           NotificationStatus
	fileCfg                config.Config
	fileCfgSet             bool
	activeBufferSize       int
	activeMediaConcurrency int
	repository             repository.RuntimeSettingsRepository
	notify                 func(context.Context) error
	requestRetryProjection func() config.RequestRetryConfig
}

// NewService receives consumers already constructed from cfg. Each target must
// support repeated installation of the complete configuration and must not mutate
// cfg. Registration is fixed for the instance lifetime; callbacks run without mu.
func NewService(cfg config.Config, updatedAt time.Time, revision uint64, repository repository.RuntimeSettingsRepository, notify func(context.Context) error, targets []ApplyTarget) *Service {
	if updatedAt.IsZero() {
		updatedAt = time.Now().UTC()
	}
	states := make([]ApplyStatus, len(targets))
	names := make(map[string]bool, len(targets))
	for i, target := range targets {
		if target.Name == "" || target.Apply == nil || names[target.Name] {
			panic("invalid settings apply target")
		}
		names[target.Name] = true
		states[i] = ApplyStatus{Name: target.Name, AppliedRevision: revision}
	}
	notification := NotificationStatus{Revision: revision, State: "observed"}
	if notify == nil {
		notification.State = "disabled"
	}
	return &Service{cfg: cfg, updatedAt: updatedAt, revision: revision, lastAppliedRevision: revision,
		activeBufferSize: cfg.Audit.BufferSize, activeMediaConcurrency: cfg.Provider.Web.MediaConcurrency,
		repository: repository, notify: notify, targets: append([]ApplyTarget(nil), targets...), applyStates: states, notification: notification}
}

// LoadPersisted 将数据库运行设置覆盖到代码默认配置，并执行完整边界校验。
func LoadPersisted(ctx context.Context, base config.Config, repository repository.RuntimeSettingsRepository) (config.Config, time.Time, uint64, error) {
	value, updatedAt, revision, found, err := repository.Get(ctx)
	if err != nil {
		return config.Config{}, time.Time{}, 0, err
	}
	if !found {
		return base, updatedAt, revision, nil
	}
	// 持久化层使用强类型时长，避免数据库格式受 HTTP DTO 字符串影响。
	loaded, err := applyDomainConfig(base, value)
	if err != nil {
		return config.Config{}, time.Time{}, 0, err
	}
	if err := loaded.Validate(); err != nil {
		return config.Config{}, time.Time{}, 0, fmt.Errorf("校验运行设置: %w", err)
	}
	return loaded, updatedAt, revision, nil
}

// Get returns the local saved intent and application status, without a storage read.
func (s *Service) Get() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snapshotLocked()
}

// PublicAPIBaseURL 返回运行设置、配置文件或内置默认值解析后的公开 API 根地址。
func (s *Service) PublicAPIBaseURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg.Frontend.EffectivePublicAPIBaseURL()
}

// Update 校验并持久化运行设置，再原子替换进程内配置。
func (s *Service) Update(ctx context.Context, expectedRevision uint64, input EditableConfig) (Snapshot, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	s.mu.RLock()
	current := s.cfg
	currentRevision := s.revision
	s.mu.RUnlock()
	if expectedRevision != currentRevision {
		return Snapshot{}, ErrConflict
	}
	if s.requestRetryProjection != nil {
		input.RequestRetryProvided = false
	}
	next, err := mergeEditable(current, input)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return s.persistAndApply(ctx, next, currentRevision)
}

// ResetEgressRotation restores only the displayed network rotation policy.
// Other gateway settings and the quality_guard document retain their owners.
func (s *Service) ResetEgressRotation(ctx context.Context, expected uint64) (Snapshot, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	next, base, revision := s.cfg, s.cfg, s.revision
	if s.fileCfgSet {
		base = s.fileCfg
	}
	s.mu.RUnlock()
	if expected != revision {
		return Snapshot{}, ErrConflict
	}
	next.Egress.Rotation = base.Egress.Rotation
	return s.persistAndApply(ctx, next, expected)
}

// Caller holds updateMu after validating its complete configuration intent.
func (s *Service) persistAndApply(ctx context.Context, next config.Config, expected uint64) (Snapshot, error) {
	updatedAt, revision, err := s.repository.Save(ctx, s.persistedConfig(next), expected)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return Snapshot{}, ErrConflict
		}
		return Snapshot{}, err
	}

	// Every save persists the canonical retention override, including legacy clients.
	next.Audit.RetentionSource = "runtime"
	s.mu.Lock()
	s.cfg = next
	s.updatedAt = updatedAt
	s.revision = revision
	s.markNotificationLocked(revision, true)
	s.mu.Unlock()

	s.runApply(ctx, next, revision)
	s.publish(ctx)
	return s.Get(), nil
}

// SetFileConfig 记录「文件默认」基线，供 ResetToDefaults 恢复。
// 在装配层加载持久化覆盖前调用一次。
func (s *Service) SetFileConfig(base config.Config) {
	s.mu.Lock()
	s.fileCfg = base
	s.fileCfgSet = true
	s.mu.Unlock()
}

// ResetToDefaults advances the durable revision and restores this instance's
// file baseline. A stale caller cannot remove another instance's newer override.
func (s *Service) ResetToDefaults(ctx context.Context, expectedRevision uint64) (Snapshot, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	s.mu.RLock()
	currentRevision := s.revision
	base := s.cfg
	if s.fileCfgSet {
		base = s.fileCfg
	}
	s.mu.RUnlock()
	if expectedRevision != currentRevision {
		return Snapshot{}, ErrConflict
	}
	updatedAt, nextRevision, err := s.repository.Reset(ctx, expectedRevision)
	if err != nil {
		if errors.Is(err, repository.ErrConflict) {
			return Snapshot{}, ErrConflict
		}
		return Snapshot{}, err
	}
	s.mu.Lock()
	s.cfg = base
	s.updatedAt = updatedAt
	s.revision = nextRevision
	s.markNotificationLocked(nextRevision, true)
	s.mu.Unlock()
	s.runApply(ctx, base, nextRevision)
	s.publish(ctx)
	return s.Get(), nil
}

// ReloadPersisted reconciles against the durable clock. Notifications only
// trigger this read; they never provide an alternative version authority.
func (s *Service) ReloadPersisted(ctx context.Context) error {
	snapshot, err := s.Read(ctx)
	if err != nil {
		return err
	}
	if snapshot.ApplyPending {
		return ErrApplyPending
	}
	return nil
}

// Read checks durable authority and retries unfinished application/publication.
// Storage failure is an error; a saved intent with pending application is data.
func (s *Service) Read(ctx context.Context) (Snapshot, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if err := s.reload(ctx); err != nil {
		return Snapshot{}, err
	}
	s.publish(ctx)
	return s.Get(), nil
}

func (s *Service) reload(ctx context.Context) error {
	value, updatedAt, revision, hasOverride, err := s.repository.Get(ctx)
	if err != nil {
		return err
	}
	s.mu.RLock()
	current := s.cfg
	base := s.cfg
	if s.fileCfgSet {
		base = s.fileCfg
	}
	currentRevision := s.revision
	appliedRevision := s.lastAppliedRevision
	s.mu.RUnlock()
	if revision < currentRevision {
		return fmt.Errorf("runtime settings revision regressed: persisted=%d observed=%d", revision, currentRevision)
	}
	if revision == currentRevision {
		if appliedRevision < currentRevision {
			s.runApply(ctx, current, currentRevision)
		}
		return nil
	}
	next := base
	if hasOverride {
		next, err = applyDomainConfig(base, value)
		if err != nil {
			return err
		}
	}
	if err := next.Validate(); err != nil {
		return fmt.Errorf("校验重载运行设置: %w", err)
	}
	s.mu.Lock()
	s.cfg = next
	s.updatedAt = updatedAt
	s.revision = revision
	s.markNotificationLocked(revision, false)
	s.mu.Unlock()
	s.runApply(ctx, next, revision)
	return nil
}

func applyDomainConfig(base config.Config, value settingsdomain.Config) (config.Config, error) {
	resolvedAudit, err := persistedAuditRetention(base.Audit, value.Audit)
	if err != nil {
		return config.Config{}, err
	}
	// 旧版运行设置没有 Server 字段，反序列化后为零；升级时沿用当前配置默认值。
	if value.Server.MaxConcurrentRequests > 0 {
		base.Server.MaxConcurrentRequests = value.Server.MaxConcurrentRequests
	}
	capacityWait := value.Routing.CapacityWait
	if capacityWait <= 0 {
		capacityWait = base.Routing.CapacityWait.Value()
	}
	base.Provider.Build = config.BuildProviderConfig{
		BaseURL: value.ProviderBuild.BaseURL, FallbackBaseURL: config.NormalizeBuildFallbackBaseURL(value.ProviderBuild.FallbackBaseURL),
		ClientVersion: value.ProviderBuild.ClientVersion, ClientIdentifier: value.ProviderBuild.ClientIdentifier,
		TokenAuth: value.ProviderBuild.TokenAuth, UserAgent: value.ProviderBuild.UserAgent,
		ResponseHeaderTimeout: config.Duration(value.ProviderBuild.ResponseHeaderTimeout),
		StreamIdleTimeout:     config.Duration(value.ProviderBuild.StreamIdleTimeout),
	}
	if value.ProviderBuild.ResponseHeaderTimeout <= 0 {
		base.Provider.Build.ResponseHeaderTimeout = config.Duration(settingsdomain.DefaultBuildResponseHeaderTimeout)
	}
	if value.ProviderBuild.StreamIdleTimeout <= 0 {
		base.Provider.Build.StreamIdleTimeout = config.Duration(settingsdomain.DefaultBuildStreamIdleTimeout)
	}
	clearanceMode := strings.TrimSpace(value.ProviderWeb.ClearanceMode)
	if clearanceMode == "" {
		clearanceMode = base.Provider.Web.ClearanceMode
	}
	flareSolverrURL := strings.TrimSpace(value.ProviderWeb.FlareSolverrURL)
	if flareSolverrURL == "" {
		flareSolverrURL = base.Provider.Web.FlareSolverrURL
	}
	clearanceTimeout := value.ProviderWeb.ClearanceTimeout
	if clearanceTimeout <= 0 {
		clearanceTimeout = base.Provider.Web.ClearanceTimeout.Value()
	}
	clearanceRefresh := value.ProviderWeb.ClearanceRefresh
	if clearanceRefresh <= 0 {
		clearanceRefresh = base.Provider.Web.ClearanceRefresh.Value()
	}
	base.Provider.Web = config.WebProviderConfig{
		BaseURL: value.ProviderWeb.BaseURL, QuotaTimeout: config.Duration(value.ProviderWeb.QuotaTimeout),
		StatsigMode: value.ProviderWeb.StatsigMode, StatsigManualValue: value.ProviderWeb.StatsigManualValue, StatsigSignerURL: value.ProviderWeb.StatsigSignerURL,
		ClearanceMode: clearanceMode, FlareSolverrURL: flareSolverrURL,
		ClearanceTimeout: config.Duration(clearanceTimeout), ClearanceRefresh: config.Duration(clearanceRefresh),
		ChatTimeout: config.Duration(value.ProviderWeb.ChatTimeout), StreamIdleTimeout: config.Duration(value.ProviderWeb.StreamIdleTimeout),
		ImageTimeout:     config.Duration(value.ProviderWeb.ImageTimeout),
		VideoTimeout:     config.Duration(value.ProviderWeb.VideoTimeout),
		MediaConcurrency: value.ProviderWeb.MediaConcurrency, AllowNSFW: value.ProviderWeb.AllowNSFW,
		RecoveryBackoffBase: config.Duration(value.ProviderWeb.RecoveryBackoffBase), RecoveryBackoffMax: config.Duration(value.ProviderWeb.RecoveryBackoffMax),
	}
	if value.ProviderWeb.StreamIdleTimeout <= 0 {
		base.Provider.Web.StreamIdleTimeout = config.Duration(settingsdomain.DefaultWebStreamIdleTimeout)
	}
	// Console 是后续版本新增的完整配置段；旧 JSON 整段缺失时沿用代码默认值。
	if value.ProviderConsole != (settingsdomain.ProviderConsoleConfig{}) {
		base.Provider.Console = config.ConsoleProviderConfig{
			BaseURL: value.ProviderConsole.BaseURL, ChatTimeout: config.Duration(value.ProviderConsole.ChatTimeout),
			StreamIdleTimeout: config.Duration(value.ProviderConsole.StreamIdleTimeout),
		}
		if value.ProviderConsole.StreamIdleTimeout <= 0 {
			base.Provider.Console.StreamIdleTimeout = config.Duration(settingsdomain.DefaultConsoleStreamIdleTimeout)
		}
	}
	randomDelay := time.Duration(-1)
	if value.Batch.RandomDelay != nil {
		randomDelay = *value.Batch.RandomDelay
	}
	base.Batch = config.BatchConfig{
		ImportConcurrency: value.Batch.ImportConcurrency, ConversionConcurrency: value.Batch.ConversionConcurrency,
		SyncConcurrency: value.Batch.SyncConcurrency, RefreshConcurrency: value.Batch.RefreshConcurrency,
		RandomDelay: config.Duration(randomDelay),
	}
	base.Media.MaxImageBytes = value.Media.MaxImageBytes
	base.Media.MaxTotalBytes = value.Media.MaxTotalBytes
	base.Media.CleanupThresholdPercent = value.Media.CleanupThresholdPercent
	base.Media.CleanupInterval = config.Duration(value.Media.CleanupInterval)
	base.Frontend.PublicAPIBaseURLOverride = strings.TrimSpace(value.Frontend.PublicAPIBaseURL)
	segmentedEnabled := base.Routing.SegmentedSelectorEnabled
	segmentedMinCandidates := base.Routing.SegmentedMinCandidates
	segmentedWindowSize := base.Routing.SegmentedWindowSize
	accountIsolatedConnections := base.Routing.AccountIsolatedConnections
	if value.Routing.AccountIsolatedConnections != nil {
		accountIsolatedConnections = *value.Routing.AccountIsolatedConnections
	}
	if value.Routing.SegmentedSelector != nil {
		segmentedEnabled = value.Routing.SegmentedSelector.ActiveEnabled
		segmentedMinCandidates = value.Routing.SegmentedSelector.MinCandidates
		segmentedWindowSize = value.Routing.SegmentedSelector.WindowSize
	}
	base.Routing = config.RoutingConfig{
		StickyTTL: config.Duration(value.Routing.StickyTTL), CooldownBase: config.Duration(value.Routing.CooldownBase),
		CooldownMax: config.Duration(value.Routing.CooldownMax), CapacityWait: config.Duration(capacityWait), MaxAttempts: value.Routing.MaxAttempts, VideoMaxAttempts: value.Routing.VideoMaxAttempts,
		MarkBuildChatDeniedAsReauth: value.Routing.MarkBuildChatDeniedAsReauth,
		PreferFreeBuild:             value.Routing.PreferFreeBuild,
		AccountIsolatedConnections:  accountIsolatedConnections,
		SegmentedSelectorEnabled:    segmentedEnabled,
		SegmentedMinCandidates:      segmentedMinCandidates,
		SegmentedWindowSize:         segmentedWindowSize,
		ReasoningReplayEnabled:      base.Routing.ReasoningReplayEnabled, ReasoningReplayTTL: base.Routing.ReasoningReplayTTL,
		ConversationHistoryRetention: base.Routing.ConversationHistoryRetention, ConversationHistoryMaxBytes: base.Routing.ConversationHistoryMaxBytes,
		ReasoningReplayMaxEntries: base.Routing.ReasoningReplayMaxEntries,
	}
	commitDelay := base.Audit.CommitDelay.Value()
	if value.Audit.CommitDelay > 0 {
		commitDelay = value.Audit.CommitDelay
	}

	base.Audit = config.AuditConfig{
		JournalDirectory: base.Audit.JournalDirectory, JournalMaxBytes: base.Audit.JournalMaxBytes,
		BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: config.Duration(value.Audit.FlushInterval),
		CommitDelay: config.Duration(commitDelay), RetentionPeriod: resolvedAudit.RetentionPeriod, RetentionSource: resolvedAudit.RetentionSource,
		LedgerMode: base.Audit.LedgerMode, LedgerFailureThreshold: base.Audit.LedgerFailureThreshold,
		LedgerUnhealthyGrace: base.Audit.LedgerUnhealthyGrace, LedgerQueueHighWatermarkPct: base.Audit.LedgerQueueHighWatermarkPct,
	}
	base.ClientKeyDefaults = config.ClientKeyDefaultsConfig{
		RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
	}
	// Accounts 为后续新增段；旧持久化缺字段时沿用代码默认（全部关闭）。
	if value.Accounts.AutoCleanReauthInterval > 0 {
		base.Accounts.AutoCleanReauthInterval = config.Duration(value.Accounts.AutoCleanReauthInterval)
	}
	if value.Accounts.AutoCleanReauthMinAge > 0 {
		base.Accounts.AutoCleanReauthMinAge = config.Duration(value.Accounts.AutoCleanReauthMinAge)
	}
	base.Accounts.AutoCleanReauthEnabled = value.Accounts.AutoCleanReauthEnabled
	base.Accounts.AutoCleanIncludeDisabled = value.Accounts.AutoCleanIncludeDisabled
	base.Accounts.MarkBuildForbiddenReauth = value.Accounts.MarkBuildForbiddenReauth
	if value.Accounts.BuildForbiddenReauthCodes != nil {
		base.Accounts.BuildForbiddenReauthCodes = append([]string(nil), value.Accounts.BuildForbiddenReauthCodes...)
	}
	base.Accounts.ExcludeBuildBotFlaggedFromScheduling = value.Accounts.ExcludeBuildBotFlaggedFromScheduling
	// RequestRetry/EgressRotation:指针节,nil(旧载荷/未保存过)沿用文件基线。
	// AccountRisk 节已随旧风险归因链删除(切换手册第2步)。
	if value.RequestRetry != nil {
		// guardedModels 是 yaml 级白名单，不在管理端 DTO。整节赋值不得用
		// 缺省/空切片/陈旧持久化名单覆盖文件基线（空使用领域默认清单）。
		fileGuarded := append([]string(nil), base.RequestRetry.GuardedModels...)
		fileAdmission, fileToolAdmission := base.RequestRetry.AdmissionTimeout, base.RequestRetry.ToolAdmissionTimeout
		base.RequestRetry = config.RequestRetryConfig{
			Enabled: value.RequestRetry.Enabled, MaxAttempts: value.RequestRetry.MaxAttempts,
			AdmissionTimeout: fileAdmission, ToolAdmissionTimeout: fileToolAdmission,
			OnExhausted: value.RequestRetry.OnExhausted, AccountCooldown: config.Duration(value.RequestRetry.AccountCooldown),
			EvidenceTimeout: config.Duration(value.RequestRetry.EvidenceTimeout), CreatedTimeout: config.Duration(value.RequestRetry.CreatedTimeout),
			IdleAccountCooldown: config.Duration(value.RequestRetry.IdleAccountCooldown),
			GuardedModels:       fileGuarded,
		}
	}
	if value.EgressRotation != nil {
		base.Egress.Rotation = config.EgressRotationConfig{
			Enabled: value.EgressRotation.Enabled, MaxAttemptsPerQuarantine: value.EgressRotation.MaxAttemptsPerQuarantine,
			MinNodeInterval: config.Duration(value.EgressRotation.MinNodeInterval), MaxGlobalPerHour: value.EgressRotation.MaxGlobalPerHour,
			WebhookTimeout: config.Duration(value.EgressRotation.WebhookTimeout), WebhookRetries: value.EgressRotation.WebhookRetries,
			SettleDelay: config.Duration(value.EgressRotation.SettleDelay), ProbeTimeout: config.Duration(value.EgressRotation.ProbeTimeout),
			ProbeInterval: config.Duration(value.EgressRotation.ProbeInterval),
		}
	}
	return base, nil
}

func toDomainConfig(value config.Config) settingsdomain.Config {
	randomDelay := value.Batch.RandomDelay.Value()
	accountIsolatedConnections := value.Routing.AccountIsolatedConnections
	return settingsdomain.Config{
		Server: settingsdomain.ServerConfig{MaxConcurrentRequests: value.Server.MaxConcurrentRequests},
		ProviderBuild: settingsdomain.ProviderBuildConfig{
			BaseURL: value.Provider.Build.BaseURL, FallbackBaseURL: config.NormalizeBuildFallbackBaseURL(value.Provider.Build.FallbackBaseURL),
			ClientVersion: value.Provider.Build.ClientVersion, ClientIdentifier: value.Provider.Build.ClientIdentifier,
			TokenAuth: value.Provider.Build.TokenAuth, UserAgent: value.Provider.Build.UserAgent,
			ResponseHeaderTimeout: value.Provider.Build.ResponseHeaderTimeout.Value(),
			StreamIdleTimeout:     value.Provider.Build.StreamIdleTimeout.Value(),
		},
		ProviderWeb: settingsdomain.ProviderWebConfig{
			BaseURL: value.Provider.Web.BaseURL, QuotaTimeout: value.Provider.Web.QuotaTimeout.Value(),
			StatsigMode: value.Provider.Web.StatsigMode, StatsigManualValue: value.Provider.Web.StatsigManualValue,
			StatsigSignerURL: value.Provider.Web.StatsigSignerURL,
			ClearanceMode:    value.Provider.Web.ClearanceMode, FlareSolverrURL: value.Provider.Web.FlareSolverrURL,
			ClearanceTimeout: value.Provider.Web.ClearanceTimeout.Value(), ClearanceRefresh: value.Provider.Web.ClearanceRefresh.Value(),
			ChatTimeout: value.Provider.Web.ChatTimeout.Value(), StreamIdleTimeout: value.Provider.Web.StreamIdleTimeout.Value(),
			ImageTimeout:     value.Provider.Web.ImageTimeout.Value(),
			VideoTimeout:     value.Provider.Web.VideoTimeout.Value(),
			MediaConcurrency: value.Provider.Web.MediaConcurrency, AllowNSFW: value.Provider.Web.AllowNSFW,
			RecoveryBackoffBase: value.Provider.Web.RecoveryBackoffBase.Value(), RecoveryBackoffMax: value.Provider.Web.RecoveryBackoffMax.Value(),
		},
		ProviderConsole: settingsdomain.ProviderConsoleConfig{
			BaseURL: value.Provider.Console.BaseURL, ChatTimeout: value.Provider.Console.ChatTimeout.Value(),
			StreamIdleTimeout: value.Provider.Console.StreamIdleTimeout.Value(),
		},
		Batch: settingsdomain.BatchConfig{
			ImportConcurrency: value.Batch.ImportConcurrency, ConversionConcurrency: value.Batch.ConversionConcurrency,
			SyncConcurrency: value.Batch.SyncConcurrency, RefreshConcurrency: value.Batch.RefreshConcurrency,
			RandomDelay: &randomDelay,
		},
		Media: settingsdomain.MediaConfig{
			MaxImageBytes: value.Media.MaxImageBytes, MaxTotalBytes: value.Media.MaxTotalBytes,
			CleanupThresholdPercent: value.Media.CleanupThresholdPercent, CleanupInterval: value.Media.CleanupInterval.Value(),
		},
		Frontend: settingsdomain.FrontendConfig{
			PublicAPIBaseURL: value.Frontend.PublicAPIBaseURLOverride,
		},
		Routing: settingsdomain.RoutingConfig{
			StickyTTL: value.Routing.StickyTTL.Value(), CooldownBase: value.Routing.CooldownBase.Value(),
			CooldownMax: value.Routing.CooldownMax.Value(), CapacityWait: value.Routing.CapacityWait.Value(), MaxAttempts: value.Routing.MaxAttempts, VideoMaxAttempts: value.Routing.VideoMaxAttempts,
			MarkBuildChatDeniedAsReauth: value.Routing.MarkBuildChatDeniedAsReauth,
			PreferFreeBuild:             value.Routing.PreferFreeBuild,
			AccountIsolatedConnections:  &accountIsolatedConnections,
			SegmentedSelector: &settingsdomain.SegmentedSelectorConfig{
				ActiveEnabled: value.Routing.SegmentedSelectorEnabled,
				MinCandidates: value.Routing.SegmentedMinCandidates, WindowSize: value.Routing.SegmentedWindowSize,
			},
		},
		Audit: settingsdomain.AuditConfig{
			BufferSize: value.Audit.BufferSize, BatchSize: value.Audit.BatchSize, FlushInterval: value.Audit.FlushInterval.Value(), CommitDelay: value.Audit.CommitDelay.Value(),
			RetentionPeriod: durationPointer(value.Audit.RetentionPeriod.Value()),
		},
		ClientKeyDefaults: settingsdomain.ClientKeyDefaultsConfig{
			RPMLimit: value.ClientKeyDefaults.RPMLimit, MaxConcurrent: value.ClientKeyDefaults.MaxConcurrent,
		},
		Accounts: settingsdomain.AccountsConfig{
			MarkBuildForbiddenReauth:             value.Accounts.MarkBuildForbiddenReauth,
			BuildForbiddenReauthCodes:            append([]string(nil), value.Accounts.BuildForbiddenReauthCodes...),
			ExcludeBuildBotFlaggedFromScheduling: value.Accounts.ExcludeBuildBotFlaggedFromScheduling,
			AutoCleanReauthEnabled:               value.Accounts.AutoCleanReauthEnabled,
			AutoCleanReauthInterval:              value.Accounts.AutoCleanReauthInterval.Value(),
			AutoCleanReauthMinAge:                value.Accounts.AutoCleanReauthMinAge.Value(),
			AutoCleanIncludeDisabled:             value.Accounts.AutoCleanIncludeDisabled,
		},
		RequestRetry: &settingsdomain.RequestRetryConfig{
			Enabled:             value.RequestRetry.Enabled,
			MaxAttempts:         value.RequestRetry.MaxAttempts,
			OnExhausted:         value.RequestRetry.OnExhausted,
			AccountCooldown:     value.RequestRetry.AccountCooldown.Value(),
			EvidenceTimeout:     value.RequestRetry.EvidenceTimeout.Value(),
			CreatedTimeout:      value.RequestRetry.CreatedTimeout.Value(),
			IdleAccountCooldown: value.RequestRetry.IdleAccountCooldown.Value(),
			// guardedModels 是 yaml 级；apply 忽略 overlay，不回写以免陈旧名单进库。
		},
		EgressRotation: &settingsdomain.EgressRotationConfig{
			Enabled:                  value.Egress.Rotation.Enabled,
			MaxAttemptsPerQuarantine: value.Egress.Rotation.MaxAttemptsPerQuarantine,
			MinNodeInterval:          value.Egress.Rotation.MinNodeInterval.Value(),
			MaxGlobalPerHour:         value.Egress.Rotation.MaxGlobalPerHour,
			WebhookTimeout:           value.Egress.Rotation.WebhookTimeout.Value(),
			WebhookRetries:           value.Egress.Rotation.WebhookRetries,
			SettleDelay:              value.Egress.Rotation.SettleDelay.Value(),
			ProbeTimeout:             value.Egress.Rotation.ProbeTimeout.Value(),
			ProbeInterval:            value.Egress.Rotation.ProbeInterval.Value(),
		},
	}
}

func boolPointer(value bool) *bool { return &value }

func (s *Service) snapshotLocked() Snapshot {
	restartRequired := []string{}
	if s.cfg.Audit.BufferSize != s.activeBufferSize {
		restartRequired = append(restartRequired, "audit.bufferSize")
	}
	if s.cfg.Provider.Web.MediaConcurrency != s.activeMediaConcurrency {
		restartRequired = append(restartRequired, "providerWeb.mediaConcurrency")
	}
	fileBase := s.cfg
	if s.fileCfgSet {
		fileBase = s.fileCfg
	}
	effective := s.cfg
	if s.requestRetryProjection != nil {
		effective.RequestRetry = s.requestRetryProjection()
	}
	editable := toEditable(effective)
	editable.Audit.FileRetentionPeriod = fileBase.Audit.RetentionPeriod.String()
	editable.Audit.FileRetentionSource = fileBase.Audit.RetentionSource
	return Snapshot{
		Config: editable,
		RecommendedProviderBuild: ProviderBuildRecommendation{
			ClientVersion: config.RecommendedBuildClientVersion,
			UserAgent:     config.RecommendedBuildUserAgent,
		},
		UpdatedAt: s.updatedAt, Revision: s.revision, RestartRequired: restartRequired,
		AppliedRevision: s.lastAppliedRevision, ApplyPending: s.lastAppliedRevision < s.revision,
		ApplyTargets: s.applyStatusesLocked(), Notification: s.notification,
		FileRequestRetry: toEditable(fileBase).RequestRetry,
	}
}

func mergeEditable(current config.Config, input EditableConfig) (config.Config, error) {
	if input.Audit.CommitDelayMS < 0 {
		return config.Config{}, errors.New("audit.commitDelayMS 不能为负数")
	}
	// Validate before converting units so an oversized input cannot wrap into
	// the legal delay range checked by Config.Validate below.
	if int64(input.Audit.CommitDelayMS) > math.MaxInt64/int64(time.Millisecond) {
		return config.Config{}, errors.New("audit.commitDelayMS 超出有效时长范围")
	}
	next := current
	next.Server.MaxConcurrentRequests = input.Server.MaxConcurrentRequests
	next.Provider.Build.BaseURL = strings.TrimSpace(input.ProviderBuild.BaseURL)
	next.Provider.Build.FallbackBaseURL = config.NormalizeBuildFallbackBaseURL(input.ProviderBuild.FallbackBaseURL)
	next.Provider.Build.ClientVersion = strings.TrimSpace(input.ProviderBuild.ClientVersion)
	next.Provider.Build.ClientIdentifier = strings.TrimSpace(input.ProviderBuild.ClientIdentifier)
	if tokenAuth := strings.TrimSpace(input.ProviderBuild.TokenAuth); tokenAuth != "" {
		next.Provider.Build.TokenAuth = tokenAuth
	}
	next.Provider.Build.UserAgent = strings.TrimSpace(input.ProviderBuild.UserAgent)
	next.Provider.Web.BaseURL = strings.TrimSpace(input.ProviderWeb.BaseURL)
	next.Provider.Web.StatsigMode = strings.TrimSpace(input.ProviderWeb.StatsigMode)
	next.Provider.Web.StatsigSignerURL = strings.TrimSpace(input.ProviderWeb.StatsigSignerURL)
	if input.ProviderWeb.ClearanceProvided {
		next.Provider.Web.ClearanceMode = strings.TrimSpace(input.ProviderWeb.ClearanceMode)
		next.Provider.Web.FlareSolverrURL = strings.TrimSpace(input.ProviderWeb.FlareSolverrURL)
	}
	if next.Provider.Web.StatsigMode == config.StatsigModeManual {
		if value := strings.TrimSpace(input.ProviderWeb.StatsigManualValue); value != "" {
			next.Provider.Web.StatsigManualValue = value
		}
	} else {
		next.Provider.Web.StatsigManualValue = ""
	}
	next.Provider.Web.MediaConcurrency = input.ProviderWeb.MediaConcurrency
	next.Provider.Web.AllowNSFW = input.ProviderWeb.AllowNSFW
	next.Provider.Console.BaseURL = strings.TrimSpace(input.ProviderConsole.BaseURL)
	next.Batch = config.BatchConfig{
		ImportConcurrency: input.Batch.ImportConcurrency, ConversionConcurrency: input.Batch.ConversionConcurrency,
		SyncConcurrency: input.Batch.SyncConcurrency, RefreshConcurrency: input.Batch.RefreshConcurrency,
	}
	next.Media.MaxImageBytes = input.Media.MaxImageBytes
	next.Media.MaxTotalBytes = input.Media.MaxTotalBytes
	next.Media.CleanupThresholdPercent = input.Media.CleanupThresholdPercent
	next.Frontend.PublicAPIBaseURLOverride = strings.TrimSpace(input.Frontend.PublicAPIBaseURL)
	next.Routing.MaxAttempts = input.Routing.MaxAttempts
	next.Routing.VideoMaxAttempts = input.Routing.VideoMaxAttempts
	next.Routing.PreferFreeBuild = input.Routing.PreferFreeBuild
	if input.Routing.AccountIsolatedConnectionsProvided {
		next.Routing.AccountIsolatedConnections = input.Routing.AccountIsolatedConnections
	}
	if input.Routing.SegmentedSelectorProvided {
		next.Routing.SegmentedSelectorEnabled = input.Routing.SegmentedSelector.Enabled
		next.Routing.SegmentedMinCandidates = input.Routing.SegmentedSelector.MinCandidates
		next.Routing.SegmentedWindowSize = input.Routing.SegmentedSelector.WindowSize
	}
	if input.Routing.MarkBuildChatDeniedAsReauthProvided {
		next.Routing.MarkBuildChatDeniedAsReauth = input.Routing.MarkBuildChatDeniedAsReauth
	}
	next.Audit.BufferSize = input.Audit.BufferSize
	next.Audit.BatchSize = input.Audit.BatchSize
	if input.Audit.CommitDelayMS > 0 {
		next.Audit.CommitDelay = config.Duration(time.Duration(input.Audit.CommitDelayMS) * time.Millisecond)
	}
	var retentionErr error
	next.Audit, retentionErr = mergeAuditRetention(next.Audit, input.Audit)
	if retentionErr != nil {
		return config.Config{}, retentionErr
	}

	next.ClientKeyDefaults.RPMLimit = input.ClientKeyDefaults.RPMLimit
	next.ClientKeyDefaults.MaxConcurrent = input.ClientKeyDefaults.MaxConcurrent
	if input.AccountsProvided {
		if input.Accounts.MarkBuildForbiddenReauthProvided {
			next.Accounts.MarkBuildForbiddenReauth = input.Accounts.MarkBuildForbiddenReauth
		}
		if input.Accounts.BuildForbiddenReauthCodesProvided {
			next.Accounts.BuildForbiddenReauthCodes = normalizeForbiddenCodes(input.Accounts.BuildForbiddenReauthCodes)
		}
		if input.Accounts.ExcludeBuildBotFlaggedFromSchedulingProvided {
			next.Accounts.ExcludeBuildBotFlaggedFromScheduling = input.Accounts.ExcludeBuildBotFlaggedFromScheduling
		}
		next.Accounts.AutoCleanReauthEnabled = input.Accounts.AutoCleanReauthEnabled
		next.Accounts.AutoCleanIncludeDisabled = input.Accounts.AutoCleanIncludeDisabled
	}
	if input.RequestRetryProvided {
		next.RequestRetry.Enabled = input.RequestRetry.Enabled
		next.RequestRetry.MaxAttempts = input.RequestRetry.MaxAttempts
		next.RequestRetry.OnExhausted = strings.TrimSpace(input.RequestRetry.OnExhausted)
	}
	if input.EgressRotationProvided {
		next.Egress.Rotation.Enabled = input.EgressRotation.Enabled
		next.Egress.Rotation.MaxAttemptsPerQuarantine = input.EgressRotation.MaxAttemptsPerQuarantine
		next.Egress.Rotation.MaxGlobalPerHour = input.EgressRotation.MaxGlobalPerHour
		next.Egress.Rotation.WebhookRetries = input.EgressRotation.WebhookRetries
	}

	type durationInput struct {
		path  string
		value string
		set   func(config.Duration)
	}
	durations := []durationInput{
		{"routing.stickyTTL", input.Routing.StickyTTL, func(value config.Duration) { next.Routing.StickyTTL = value }},
		{"routing.cooldownBase", input.Routing.CooldownBase, func(value config.Duration) { next.Routing.CooldownBase = value }},
		{"routing.cooldownMax", input.Routing.CooldownMax, func(value config.Duration) { next.Routing.CooldownMax = value }},
		{"routing.capacityWait", input.Routing.CapacityWait, func(value config.Duration) { next.Routing.CapacityWait = value }},
		{"audit.flushInterval", input.Audit.FlushInterval, func(value config.Duration) { next.Audit.FlushInterval = value }},
		{"providerWeb.quotaTimeout", input.ProviderWeb.QuotaTimeout, func(value config.Duration) { next.Provider.Web.QuotaTimeout = value }},
		{"providerWeb.chatTimeout", input.ProviderWeb.ChatTimeout, func(value config.Duration) { next.Provider.Web.ChatTimeout = value }},
		{"providerWeb.imageTimeout", input.ProviderWeb.ImageTimeout, func(value config.Duration) { next.Provider.Web.ImageTimeout = value }},
		{"providerWeb.videoTimeout", input.ProviderWeb.VideoTimeout, func(value config.Duration) { next.Provider.Web.VideoTimeout = value }},
		{"providerWeb.recoveryBackoffBase", input.ProviderWeb.RecoveryBackoffBase, func(value config.Duration) { next.Provider.Web.RecoveryBackoffBase = value }},
		{"providerWeb.recoveryBackoffMax", input.ProviderWeb.RecoveryBackoffMax, func(value config.Duration) { next.Provider.Web.RecoveryBackoffMax = value }},
		{"providerConsole.chatTimeout", input.ProviderConsole.ChatTimeout, func(value config.Duration) { next.Provider.Console.ChatTimeout = value }},
		{"media.cleanupInterval", input.Media.CleanupInterval, func(value config.Duration) { next.Media.CleanupInterval = value }},
		{"batch.randomDelay", input.Batch.RandomDelay, func(value config.Duration) { next.Batch.RandomDelay = value }},
	}
	if strings.TrimSpace(input.ProviderBuild.ResponseHeaderTimeout) != "" {
		durations = append(durations, durationInput{"providerBuild.responseHeaderTimeout", input.ProviderBuild.ResponseHeaderTimeout, func(value config.Duration) { next.Provider.Build.ResponseHeaderTimeout = value }})
	}
	if strings.TrimSpace(input.ProviderBuild.StreamIdleTimeout) != "" {
		durations = append(durations, durationInput{"providerBuild.streamIdleTimeout", input.ProviderBuild.StreamIdleTimeout, func(value config.Duration) { next.Provider.Build.StreamIdleTimeout = value }})
	}
	if strings.TrimSpace(input.ProviderWeb.StreamIdleTimeout) != "" {
		durations = append(durations, durationInput{"providerWeb.streamIdleTimeout", input.ProviderWeb.StreamIdleTimeout, func(value config.Duration) { next.Provider.Web.StreamIdleTimeout = value }})
	}
	if strings.TrimSpace(input.ProviderConsole.StreamIdleTimeout) != "" {
		durations = append(durations, durationInput{"providerConsole.streamIdleTimeout", input.ProviderConsole.StreamIdleTimeout, func(value config.Duration) { next.Provider.Console.StreamIdleTimeout = value }})
	}
	if input.ProviderWeb.ClearanceProvided {
		durations = append(durations,
			durationInput{"providerWeb.clearanceTimeout", input.ProviderWeb.ClearanceTimeout, func(value config.Duration) { next.Provider.Web.ClearanceTimeout = value }},
			durationInput{"providerWeb.clearanceRefresh", input.ProviderWeb.ClearanceRefresh, func(value config.Duration) { next.Provider.Web.ClearanceRefresh = value }},
		)
	}
	if input.AccountsProvided {
		durations = append(durations,
			durationInput{"accounts.autoCleanReauthInterval", input.Accounts.AutoCleanReauthInterval, func(value config.Duration) { next.Accounts.AutoCleanReauthInterval = value }},
			durationInput{"accounts.autoCleanReauthMinAge", input.Accounts.AutoCleanReauthMinAge, func(value config.Duration) { next.Accounts.AutoCleanReauthMinAge = value }},
		)
	}
	if input.RequestRetryProvided {
		durations = append(durations,
			durationInput{"requestRetry.accountCooldown", input.RequestRetry.AccountCooldown, func(value config.Duration) { next.RequestRetry.AccountCooldown = value }},
			durationInput{"requestRetry.evidenceTimeout", input.RequestRetry.EvidenceTimeout, func(value config.Duration) { next.RequestRetry.EvidenceTimeout = value }},
			durationInput{"requestRetry.createdTimeout", input.RequestRetry.CreatedTimeout, func(value config.Duration) { next.RequestRetry.CreatedTimeout = value }},
			durationInput{"requestRetry.idleAccountCooldown", input.RequestRetry.IdleAccountCooldown, func(value config.Duration) { next.RequestRetry.IdleAccountCooldown = value }},
		)
	}
	if input.EgressRotationProvided {
		durations = append(durations,
			durationInput{"egressRotation.minNodeInterval", input.EgressRotation.MinNodeInterval, func(value config.Duration) { next.Egress.Rotation.MinNodeInterval = value }},
			durationInput{"egressRotation.webhookTimeout", input.EgressRotation.WebhookTimeout, func(value config.Duration) { next.Egress.Rotation.WebhookTimeout = value }},
			durationInput{"egressRotation.settleDelay", input.EgressRotation.SettleDelay, func(value config.Duration) { next.Egress.Rotation.SettleDelay = value }},
			durationInput{"egressRotation.probeTimeout", input.EgressRotation.ProbeTimeout, func(value config.Duration) { next.Egress.Rotation.ProbeTimeout = value }},
			durationInput{"egressRotation.probeInterval", input.EgressRotation.ProbeInterval, func(value config.Duration) { next.Egress.Rotation.ProbeInterval = value }},
		)
	}
	for _, item := range durations {
		value, err := time.ParseDuration(strings.TrimSpace(item.value))
		if err != nil {
			return config.Config{}, fmt.Errorf("%s 必须是有效时长", item.path)
		}
		item.set(config.Duration(value))
	}
	// Enforce the relationship only for new writes. Persisted settings from an
	// older version remain loadable during rolling upgrades, while an admin can
	// no longer save an idle deadline shadowed by a shorter absolute timeout.
	if next.Provider.Web.StreamIdleTimeout.Value() > next.Provider.Web.ChatTimeout.Value() {
		return config.Config{}, errors.New("providerWeb.streamIdleTimeout 不能超过 providerWeb.chatTimeout")
	}
	if next.Provider.Console.StreamIdleTimeout.Value() > next.Provider.Console.ChatTimeout.Value() {
		return config.Config{}, errors.New("providerConsole.streamIdleTimeout 不能超过 providerConsole.chatTimeout")
	}
	if err := next.Validate(); err != nil {
		return config.Config{}, err
	}
	return next, nil
}

func toEditable(cfg config.Config) EditableConfig {
	return EditableConfig{
		Server: ServerConfig{MaxConcurrentRequests: cfg.Server.MaxConcurrentRequests},
		ProviderBuild: ProviderBuildConfig{
			BaseURL: cfg.Provider.Build.BaseURL, FallbackBaseURL: config.NormalizeBuildFallbackBaseURL(cfg.Provider.Build.FallbackBaseURL),
			ClientVersion: cfg.Provider.Build.ClientVersion, ClientIdentifier: cfg.Provider.Build.ClientIdentifier,
			TokenAuth: cfg.Provider.Build.TokenAuth, UserAgent: cfg.Provider.Build.UserAgent,
			ResponseHeaderTimeout: cfg.Provider.Build.ResponseHeaderTimeout.String(),
			StreamIdleTimeout:     cfg.Provider.Build.StreamIdleTimeout.String(),
		},
		ProviderWeb: ProviderWebConfig{
			BaseURL: cfg.Provider.Web.BaseURL, QuotaTimeout: cfg.Provider.Web.QuotaTimeout.String(),
			StatsigMode: cfg.Provider.Web.StatsigMode, StatsigManualConfigured: strings.TrimSpace(cfg.Provider.Web.StatsigManualValue) != "",
			StatsigSignerURL: cfg.Provider.Web.StatsigSignerURL,
			ClearanceMode:    cfg.Provider.Web.ClearanceMode, FlareSolverrURL: cfg.Provider.Web.FlareSolverrURL,
			ClearanceTimeout: cfg.Provider.Web.ClearanceTimeout.String(), ClearanceRefresh: cfg.Provider.Web.ClearanceRefresh.String(),
			ChatTimeout: cfg.Provider.Web.ChatTimeout.String(), StreamIdleTimeout: cfg.Provider.Web.StreamIdleTimeout.String(),
			ImageTimeout:     cfg.Provider.Web.ImageTimeout.String(),
			VideoTimeout:     cfg.Provider.Web.VideoTimeout.String(),
			MediaConcurrency: cfg.Provider.Web.MediaConcurrency, AllowNSFW: cfg.Provider.Web.AllowNSFW,
			RecoveryBackoffBase: cfg.Provider.Web.RecoveryBackoffBase.String(), RecoveryBackoffMax: cfg.Provider.Web.RecoveryBackoffMax.String(),
		},
		ProviderConsole: ProviderConsoleConfig{
			BaseURL: cfg.Provider.Console.BaseURL, ChatTimeout: cfg.Provider.Console.ChatTimeout.String(),
			StreamIdleTimeout: cfg.Provider.Console.StreamIdleTimeout.String(),
		},
		Batch: BatchConfig{
			ImportConcurrency: cfg.Batch.ImportConcurrency, ConversionConcurrency: cfg.Batch.ConversionConcurrency,
			SyncConcurrency: cfg.Batch.SyncConcurrency, RefreshConcurrency: cfg.Batch.RefreshConcurrency,
			RandomDelay: cfg.Batch.RandomDelay.String(),
		},
		Media: MediaConfig{
			MaxImageBytes: cfg.Media.MaxImageBytes, MaxTotalBytes: cfg.Media.MaxTotalBytes,
			CleanupThresholdPercent: cfg.Media.CleanupThresholdPercent, CleanupInterval: cfg.Media.CleanupInterval.String(),
		},
		Frontend: FrontendConfig{
			PublicAPIBaseURL: cfg.Frontend.PublicAPIBaseURLOverride,
		},
		Routing: RoutingConfig{
			StickyTTL: cfg.Routing.StickyTTL.String(), CooldownBase: cfg.Routing.CooldownBase.String(),
			CooldownMax: cfg.Routing.CooldownMax.String(), CapacityWait: cfg.Routing.CapacityWait.String(), MaxAttempts: cfg.Routing.MaxAttempts, VideoMaxAttempts: cfg.Routing.VideoMaxAttempts,
			MarkBuildChatDeniedAsReauth:         cfg.Routing.MarkBuildChatDeniedAsReauth,
			MarkBuildChatDeniedAsReauthProvided: true,
			PreferFreeBuild:                     cfg.Routing.PreferFreeBuild,
			AccountIsolatedConnections:          cfg.Routing.AccountIsolatedConnections,
			AccountIsolatedConnectionsProvided:  true,
			SegmentedSelector: SegmentedSelectorConfig{
				Enabled: cfg.Routing.SegmentedSelectorEnabled, MinCandidates: cfg.Routing.SegmentedMinCandidates,
				WindowSize: cfg.Routing.SegmentedWindowSize,
			},
			SegmentedSelectorProvided: true,
		},
		Audit: AuditConfig{
			BufferSize: cfg.Audit.BufferSize, BatchSize: cfg.Audit.BatchSize, FlushInterval: cfg.Audit.FlushInterval.String(), CommitDelayMS: int(cfg.Audit.CommitDelay.Value() / time.Millisecond),
			RetentionPeriod: cfg.Audit.RetentionPeriod.String(), RetentionPeriodProvided: true, RetentionSource: cfg.Audit.RetentionSource,
			RetentionDays: int(cfg.Audit.RetentionPeriod.Value() / (24 * time.Hour)), RetentionDaysProvided: cfg.Audit.RetentionPeriod.Value()%(24*time.Hour) == 0,
		},
		ClientKeyDefaults: ClientKeyDefaultsConfig{RPMLimit: cfg.ClientKeyDefaults.RPMLimit, MaxConcurrent: cfg.ClientKeyDefaults.MaxConcurrent},
		Accounts: AccountsConfig{
			MarkBuildForbiddenReauth:                     cfg.Accounts.MarkBuildForbiddenReauth,
			BuildForbiddenReauthCodes:                    append([]string(nil), cfg.Accounts.BuildForbiddenReauthCodes...),
			ExcludeBuildBotFlaggedFromScheduling:         cfg.Accounts.ExcludeBuildBotFlaggedFromScheduling,
			MarkBuildForbiddenReauthProvided:             true,
			BuildForbiddenReauthCodesProvided:            true,
			ExcludeBuildBotFlaggedFromSchedulingProvided: true,
			AutoCleanReauthEnabled:                       cfg.Accounts.AutoCleanReauthEnabled,
			AutoCleanReauthInterval:                      cfg.Accounts.AutoCleanReauthInterval.String(),
			AutoCleanReauthMinAge:                        cfg.Accounts.AutoCleanReauthMinAge.String(),
			AutoCleanIncludeDisabled:                     cfg.Accounts.AutoCleanIncludeDisabled,
		},
		RequestRetry: RequestRetryEditable{
			Enabled: cfg.RequestRetry.Enabled, MaxAttempts: cfg.RequestRetry.MaxAttempts,
			OnExhausted: cfg.RequestRetry.OnExhausted, AccountCooldown: cfg.RequestRetry.AccountCooldown.String(),
			EvidenceTimeout: cfg.RequestRetry.EvidenceTimeout.String(), CreatedTimeout: cfg.RequestRetry.CreatedTimeout.String(),
			IdleAccountCooldown: cfg.RequestRetry.IdleAccountCooldown.String(),
		},
		RequestRetryProvided: true,
		EgressRotation: EgressRotationEditable{
			Enabled: cfg.Egress.Rotation.Enabled, MaxAttemptsPerQuarantine: cfg.Egress.Rotation.MaxAttemptsPerQuarantine,
			MinNodeInterval: cfg.Egress.Rotation.MinNodeInterval.String(), MaxGlobalPerHour: cfg.Egress.Rotation.MaxGlobalPerHour,
			WebhookTimeout: cfg.Egress.Rotation.WebhookTimeout.String(), WebhookRetries: cfg.Egress.Rotation.WebhookRetries,
			SettleDelay: cfg.Egress.Rotation.SettleDelay.String(), ProbeTimeout: cfg.Egress.Rotation.ProbeTimeout.String(),
			ProbeInterval: cfg.Egress.Rotation.ProbeInterval.String(),
		},
		EgressRotationProvided: true,
		AccountsProvided:       true,
	}
}

func normalizeForbiddenCodes(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		code := strings.ToLower(strings.TrimSpace(value))
		if code == "" {
			continue
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		result = append(result, code)
	}
	return result
}

// SetRequestRetryProjection makes the legacy settings surface read-only.
// Install once during composition, before exposing this service to requests.
func (s *Service) SetRequestRetryProjection(project func() config.RequestRetryConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestRetryProjection = project
}
func (s *Service) persistedConfig(cfg config.Config) settingsdomain.Config {
	value := toDomainConfig(cfg)
	if s.requestRetryProjection != nil {
		value.RequestRetry = nil
	}
	return value
}
