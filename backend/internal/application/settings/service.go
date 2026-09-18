package settings

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrInvalidInput = errors.New("运行设置参数无效")
	ErrConflict     = errors.New("运行设置已被其他会话更新")
	ErrApplyPending = errors.New("运行设置已保存，本实例仍有目标待应用")
)

// ProviderBuildConfig 是管理接口使用的 Provider 可编辑输入。
type ProviderBuildConfig struct {
	BaseURL                string
	FallbackBaseURL        string
	ClientVersion          string
	ClientIdentifier       string
	TokenAuth              string
	UserAgent              string
	SessionIdleConnTimeout string
	ResponseHeaderTimeout  string
	StreamIdleTimeout      string
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
	cfg                    settingsdomain.Config
	updatedAt              time.Time
	revision               uint64
	lastAppliedRevision    uint64
	targets                []ApplyTarget
	applyStates            []ApplyStatus
	notification           NotificationStatus
	fileCfg                settingsdomain.Config
	fileCfgSet             bool
	fileRetentionSource    string
	validate               func(settingsdomain.Config) error
	resolvePersisted       func(settingsdomain.Config) (settingsdomain.Config, error)
	activeBufferSize       int
	activeMediaConcurrency int
	repository             repository.RuntimeSettingsRepository
	notify                 func(context.Context) error
	requestRetryProjection func() settingsdomain.RequestRetryConfig
}

// NewService receives consumers already constructed from cfg. Each target must
// support repeated installation of the complete configuration and must not mutate
// cfg. Registration is fixed for the instance lifetime; callbacks run without mu.
func NewService(cfg settingsdomain.Config, updatedAt time.Time, revision uint64, repository repository.RuntimeSettingsRepository, notify func(context.Context) error, targets []ApplyTarget) *Service {
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
		activeBufferSize: cfg.Audit.BufferSize, activeMediaConcurrency: cfg.ProviderWeb.MediaConcurrency,
		repository: repository, notify: notify, targets: append([]ApplyTarget(nil), targets...), applyStates: states, notification: notification}
}

// LoadPersisted reads the durable overlay and revision; composition resolves
// legacy omissions against the file baseline before constructing consumers.
func LoadPersisted(ctx context.Context, repository repository.RuntimeSettingsRepository) (settingsdomain.Config, time.Time, uint64, bool, error) {
	value, updatedAt, revision, found, err := repository.Get(ctx)
	if err != nil {
		return settingsdomain.Config{}, time.Time{}, 0, false, err
	}
	return value, updatedAt, revision, found, nil
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
	next, err := s.mergeEditable(current, input)
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
	if base.EgressRotation != nil {
		cloned := *base.EgressRotation
		next.EgressRotation = &cloned
	} else {
		next.EgressRotation = nil
	}
	return s.persistAndApply(ctx, next, expected)
}

// Caller holds updateMu after validating its complete configuration intent.
func (s *Service) persistAndApply(ctx context.Context, next settingsdomain.Config, expected uint64) (Snapshot, error) {
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
func (s *Service) SetFileConfig(base settingsdomain.Config) {
	s.mu.Lock()
	s.fileCfg = base
	s.fileCfgSet = true
	s.fileRetentionSource = base.Audit.RetentionSource
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
		next = value
		if s.resolvePersisted != nil {
			next, err = s.resolvePersisted(value)
			if err != nil {
				return fmt.Errorf("解析重载运行设置: %w", err)
			}
		}
	}
	if s.validate != nil {
		if err := s.validate(next); err != nil {
			return fmt.Errorf("校验重载运行设置: %w", err)
		}
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

func boolPointer(value bool) *bool { return &value }

func (s *Service) snapshotLocked() Snapshot {
	restartRequired := []string{}
	if s.cfg.Audit.BufferSize != s.activeBufferSize {
		restartRequired = append(restartRequired, "audit.bufferSize")
	}
	if s.cfg.ProviderWeb.MediaConcurrency != s.activeMediaConcurrency {
		restartRequired = append(restartRequired, "providerWeb.mediaConcurrency")
	}
	fileBase := s.cfg
	if s.fileCfgSet {
		fileBase = s.fileCfg
	}
	effective := s.cfg
	if s.requestRetryProjection != nil {
		projected := s.requestRetryProjection()
		effective.RequestRetry = &projected
	}
	editable := toEditable(effective)
	editable.Audit.FileRetentionPeriod = formatDuration(optionalDuration(fileBase.Audit.RetentionPeriod))
	editable.Audit.FileRetentionSource = s.fileRetentionSource
	return Snapshot{
		Config: editable,
		RecommendedProviderBuild: ProviderBuildRecommendation{
			ClientVersion: recommendedBuildClientVersion,
			UserAgent:     recommendedBuildUserAgent,
		},
		UpdatedAt: s.updatedAt, Revision: s.revision, RestartRequired: restartRequired,
		AppliedRevision: s.lastAppliedRevision, ApplyPending: s.lastAppliedRevision < s.revision,
		ApplyTargets: s.applyStatusesLocked(), Notification: s.notification,
		FileRequestRetry: toEditable(fileBase).RequestRetry,
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
func (s *Service) SetRequestRetryProjection(project func() settingsdomain.RequestRetryConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestRetryProjection = project
}
func (s *Service) persistedConfig(cfg settingsdomain.Config) settingsdomain.Config {
	value := cfg
	if s.requestRetryProjection != nil {
		value.RequestRetry = nil
	}
	// RetentionSource is derived on load/merge; never persist it so the stored
	// row keeps its historical shape.
	value.Audit.RetentionSource = ""
	return value
}

func (s *Service) SetRuntimeValidator(fn func(settingsdomain.Config) error) {
	s.mu.Lock()
	s.validate = fn
	s.mu.Unlock()
}

// SetPersistedResolver installs the file-baseline compatibility overlay. It runs
// before publishing a remotely loaded snapshot or invoking any apply target.
// Install once during composition, before serving requests.
func (s *Service) SetPersistedResolver(resolve func(settingsdomain.Config) (settingsdomain.Config, error)) {
	s.mu.Lock()
	s.resolvePersisted = resolve
	s.mu.Unlock()
}
