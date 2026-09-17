package settings

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
)

// buildForbiddenCodePattern 与文件边界的账号禁用错误码规则一致。
var buildForbiddenCodePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)

// 热更新政策范围常量。文件边界（infra/config）保留部署级检查；可热编辑
// 字段的合法范围在此唯一解释，运行设置的每次应用都经过本校验。
const (
	maxServerConcurrentRequests = 100000

	maxRoutingTTL            = 24 * time.Hour
	maxRoutingCooldown       = 30 * time.Minute
	maxRoutingCapacityWait   = 30 * time.Second
	maxRoutingAttempts       = 65535
	unlimitedRoutingAttempts = -1

	maxAuditBufferSize    = 100000
	maxAuditBatchSize     = 1000
	minAuditFlushInterval = 50 * time.Millisecond
	maxAuditFlushInterval = 60 * time.Second
)

const (
	statsigModeManual = "manual"
	statsigModeURL    = "url"

	clearanceModeManual       = "manual"
	clearanceModeFlareSolverr = "flaresolverr"
	clearanceModeOnDemand     = "on_demand"
)

// Validate 校验可热更新运行设置的合法范围。输入是映射后的领域配置
// （ToRuntimeSettings 的产物，不做修复）；当前非法输入不会被缺省规则
// 修复。外部签名/清理服务 URL 的部署级检查保留在文件边界。
func (c Config) Validate() error {
	if c.Server.MaxConcurrentRequests < 1 || c.Server.MaxConcurrentRequests > maxServerConcurrentRequests {
		return errors.New("server.maxConcurrentRequests 必须在 1 到 100000 之间")
	}
	if err := c.validateProviderBuild(); err != nil {
		return err
	}
	if err := c.validateProviderWeb(); err != nil {
		return err
	}
	if err := c.validateProviderConsole(); err != nil {
		return err
	}
	if c.Batch.ImportConcurrency < 1 || c.Batch.ImportConcurrency > 50 ||
		c.Batch.ConversionConcurrency < 1 || c.Batch.ConversionConcurrency > 50 ||
		c.Batch.SyncConcurrency < 1 || c.Batch.SyncConcurrency > 50 ||
		c.Batch.RefreshConcurrency < 1 || c.Batch.RefreshConcurrency > 50 {
		return errors.New("批量任务并发必须在 1 到 50 之间")
	}
	if delay := optionalDuration(c.Batch.RandomDelay); delay < 0 || delay > 5*time.Second {
		return errors.New("批量任务随机延迟必须在 0 到 5 秒之间")
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
	if c.Media.CleanupInterval < time.Minute || c.Media.CleanupInterval > 24*time.Hour {
		return errors.New("media.cleanupInterval 必须在 1 分钟到 24 小时之间")
	}
	if publicBase := strings.TrimSpace(c.Frontend.FilePublicAPIBaseURL); publicBase != "" {
		if err := ValidateAPIBaseURL("frontend.publicApiBaseURL", publicBase, false); err != nil {
			return err
		}
	}
	if override := strings.TrimSpace(c.Frontend.PublicAPIBaseURL); override != "" {
		if err := ValidateAPIBaseURL("frontend.publicApiBaseURL 运行设置", override, false); err != nil {
			return err
		}
	}
	if err := c.validateRouting(); err != nil {
		return err
	}
	if err := c.validateAudit(); err != nil {
		return err
	}
	if c.RequestRetry != nil {
		if err := validateRequestRetryEncoding(c.RequestRetry.OnExhausted); err != nil {
			return err
		}
	}
	if c.EgressRotation != nil {
		if err := validateEgressRotation(c.EgressRotation); err != nil {
			return err
		}
	}
	if c.ClientKeyDefaults.RPMLimit < 1 || c.ClientKeyDefaults.RPMLimit > clientkeydomain.MaxRPMLimit || c.ClientKeyDefaults.MaxConcurrent < 1 || c.ClientKeyDefaults.MaxConcurrent > clientkeydomain.MaxConcurrent {
		return errors.New("clientKeyDefaults 超出允许范围")
	}
	return c.validateAccounts()
}

func (c Config) validateProviderBuild() error {
	if err := ValidateAPIBaseURL("provider.build.baseURL", c.ProviderBuild.BaseURL, false); err != nil {
		return err
	}
	if err := ValidateAPIBaseURL("provider.build.fallbackBaseURL", c.ProviderBuild.FallbackBaseURL, true); err != nil {
		return err
	}
	if strings.TrimSpace(c.ProviderBuild.ClientVersion) == "" || strings.TrimSpace(c.ProviderBuild.ClientIdentifier) == "" || strings.TrimSpace(c.ProviderBuild.TokenAuth) == "" || strings.TrimSpace(c.ProviderBuild.UserAgent) == "" {
		return errors.New("provider.build 客户端标识不能为空")
	}
	if timeout := c.ProviderBuild.ResponseHeaderTimeout; timeout < MinBuildResponseHeaderTimeout || timeout > MaxBuildResponseHeaderTimeout {
		return errors.New("Grok Build 响应头超时必须在 30 秒到 30 分钟之间")
	}
	if idle := c.ProviderBuild.StreamIdleTimeout; idle < MinBuildStreamIdleTimeout || idle > MaxBuildStreamIdleTimeout {
		return errors.New("Grok Build 流式空闲超时必须在 30 秒到 10 分钟之间")
	}
	return nil
}

func (c Config) validateProviderWeb() error {
	webURL, err := url.ParseRequestURI(strings.TrimSpace(c.ProviderWeb.BaseURL))
	if err != nil || webURL.Scheme != "https" || webURL.Host == "" || webURL.User != nil {
		return errors.New("provider.web.baseURL 必须是无凭据的 HTTPS URL")
	}
	switch c.ProviderWeb.StatsigMode {
	case statsigModeManual:
		if !validStatsigID(c.ProviderWeb.StatsigManualValue) {
			return errors.New("provider.web 手动 x-statsig-id 格式无效")
		}
	case statsigModeURL:
		// 签名服务 URL 的部署级检查在文件边界执行。
	default:
		return errors.New("provider.web Statsig 模式必须是 manual 或 url")
	}
	switch c.ProviderWeb.ClearanceMode {
	case clearanceModeManual:
	case clearanceModeFlareSolverr, clearanceModeOnDemand:
		// FlareSolverr 端点 URL 的部署级检查在文件边界执行。
	default:
		return errors.New("provider.web Clearance 模式必须是 manual、flaresolverr 或 on_demand")
	}
	if c.ProviderWeb.ClearanceTimeout < 10*time.Second || c.ProviderWeb.ClearanceTimeout > 5*time.Minute {
		return errors.New("provider.web Clearance 超时必须在 10 秒到 5 分钟之间")
	}
	if c.ProviderWeb.ClearanceRefresh < time.Minute || c.ProviderWeb.ClearanceRefresh > 24*time.Hour {
		return errors.New("provider.web Clearance 刷新间隔必须在 1 分钟到 24 小时之间")
	}
	if c.ProviderWeb.QuotaTimeout < time.Second || c.ProviderWeb.QuotaTimeout > 2*time.Minute {
		return errors.New("provider.web.quotaTimeout 必须在 1 秒到 2 分钟之间")
	}
	if c.ProviderWeb.ChatTimeout < 5*time.Second || c.ProviderWeb.ChatTimeout > 30*time.Minute {
		return errors.New("provider.web.chatTimeout 必须在 5 秒到 30 分钟之间")
	}
	if c.ProviderWeb.ImageTimeout < 5*time.Second || c.ProviderWeb.ImageTimeout > 30*time.Minute {
		return errors.New("provider.web.imageTimeout 必须在 5 秒到 30 分钟之间")
	}
	if c.ProviderWeb.VideoTimeout < time.Minute || c.ProviderWeb.VideoTimeout > 2*time.Hour {
		return errors.New("provider.web.videoTimeout 必须在 1 分钟到 2 小时之间")
	}
	if idle := c.ProviderWeb.StreamIdleTimeout; idle < MinProviderStreamIdleTimeout || idle > MaxProviderStreamIdleTimeout {
		return errors.New("Grok Web 流式空闲超时必须在 30 秒到 10 分钟之间")
	}
	if c.ProviderWeb.MediaConcurrency < 1 || c.ProviderWeb.MediaConcurrency > 64 {
		return errors.New("provider.web 媒体并发必须在 1 到 64 之间")
	}
	if c.ProviderWeb.RecoveryBackoffBase < 5*time.Second || c.ProviderWeb.RecoveryBackoffMax < c.ProviderWeb.RecoveryBackoffBase || c.ProviderWeb.RecoveryBackoffMax > 6*time.Hour {
		return errors.New("provider.web 恢复退避配置无效")
	}
	return nil
}

func (c Config) validateProviderConsole() error {
	consoleURL, err := url.ParseRequestURI(strings.TrimSpace(c.ProviderConsole.BaseURL))
	if err != nil || consoleURL.Scheme != "https" || consoleURL.Host == "" || consoleURL.User != nil {
		return errors.New("provider.console.baseURL 必须是无凭据的 HTTPS URL")
	}
	if c.ProviderConsole.ChatTimeout < 5*time.Second || c.ProviderConsole.ChatTimeout > 30*time.Minute {
		return errors.New("provider.console.chatTimeout 必须在 5 秒到 30 分钟之间")
	}
	if idle := c.ProviderConsole.StreamIdleTimeout; idle < MinProviderStreamIdleTimeout || idle > MaxProviderStreamIdleTimeout {
		return errors.New("Grok Console 流式空闲超时必须在 30 秒到 10 分钟之间")
	}
	return nil
}

func (c Config) validateRouting() error {
	if c.Routing.StickyTTL <= 0 || c.Routing.StickyTTL > maxRoutingTTL {
		return fmt.Errorf("routing.stickyTTL 必须在 1 纳秒到 %s 之间", maxRoutingTTL)
	}
	if c.Routing.CooldownBase <= 0 || c.Routing.CooldownMax < c.Routing.CooldownBase || c.Routing.CooldownMax > maxRoutingCooldown {
		return errors.New("routing.cooldownBase/cooldownMax 配置无效: 需要 0 < cooldownBase <= cooldownMax <= 30m")
	}
	if c.Routing.CapacityWait <= 0 || c.Routing.CapacityWait > maxRoutingCapacityWait {
		return fmt.Errorf("routing.capacityWait 必须在 1 纳秒到 %s 之间", maxRoutingCapacityWait)
	}
	if c.Routing.MaxAttempts < unlimitedRoutingAttempts || c.Routing.MaxAttempts == 0 || c.Routing.MaxAttempts > maxRoutingAttempts {
		return errors.New("routing.maxAttempts 必须是 -1(不限)、1 到 65535；0 不被接受")
	}
	if c.Routing.VideoMaxAttempts < unlimitedRoutingAttempts || c.Routing.VideoMaxAttempts > maxRoutingAttempts {
		return errors.New("routing.videoMaxAttempts 必须是 -1(不限)、0(默认 3)或 1 到 65535")
	}
	if c.Routing.SegmentedSelector != nil && c.Routing.SegmentedSelector.ActiveEnabled {
		if c.Routing.SegmentedSelector.MinCandidates < 100 || c.Routing.SegmentedSelector.MinCandidates > 1000000 {
			return errors.New("routing.segmentedMinCandidates 必须在 100 到 1000000 之间")
		}
		if c.Routing.SegmentedSelector.WindowSize < 8 || c.Routing.SegmentedSelector.WindowSize > 256 || c.Routing.SegmentedSelector.WindowSize > c.Routing.SegmentedSelector.MinCandidates {
			return errors.New("routing.segmentedWindowSize 必须在 8 到 256 之间且不超过 segmentedMinCandidates")
		}
	}
	return nil
}

func (c Config) validateAudit() error {
	if c.Audit.BufferSize < 1 || c.Audit.BufferSize > maxAuditBufferSize {
		return errors.New("audit.bufferSize 必须在 1 到 100000 之间")
	}
	if c.Audit.BatchSize < 1 || c.Audit.BatchSize > maxAuditBatchSize || c.Audit.BatchSize > c.Audit.BufferSize {
		return errors.New("audit.batchSize 必须在 1 到 1000 之间且不超过 bufferSize")
	}
	if c.Audit.FlushInterval < minAuditFlushInterval || c.Audit.FlushInterval > maxAuditFlushInterval {
		return errors.New("audit.flushInterval 必须在 50ms 到 60s 之间")
	}
	if err := auditdomain.ValidateCommitDelay(c.Audit.CommitDelay); err != nil {
		return err
	}
	return (auditdomain.RetentionPolicy{Period: optionalDuration(c.Audit.RetentionPeriod)}).Validate()
}

func (c Config) validateAccounts() error {
	if c.Accounts.AutoCleanReauthInterval < time.Minute || c.Accounts.AutoCleanReauthInterval > time.Hour {
		return errors.New("accounts.autoCleanReauthInterval 必须在 1 分钟到 1 小时之间")
	}
	if c.Accounts.AutoCleanReauthMinAge < time.Minute || c.Accounts.AutoCleanReauthMinAge > 30*24*time.Hour {
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

// validateRequestRetryEncoding 校验旧版 onExhausted 编码；生效策略恒为
// fail_closed。守卫政策的完整校验依赖 yaml 级字段，保留在文件边界。
func validateRequestRetryEncoding(onExhausted string) error {
	switch strings.TrimSpace(onExhausted) {
	case "", "fail_open", "fail_closed":
		return nil
	default:
		return errors.New("requestRetry.onExhausted: legacy value must be fail_open or fail_closed; effective policy is fail_closed")
	}
}

func validateEgressRotation(rot *EgressRotationConfig) error {
	if rot.MaxAttemptsPerQuarantine < 0 || rot.MaxAttemptsPerQuarantine > 100 {
		return errors.New("egress.rotation.maxAttemptsPerQuarantine 必须在 0 到 100 之间（0=默认 3）")
	}
	if v := rot.MinNodeInterval; v != 0 && (v < 10*time.Second || v > 24*time.Hour) {
		return errors.New("egress.rotation.minNodeInterval 必须在 10 秒到 24 小时之间（0=默认 3m）")
	}
	if rot.MaxGlobalPerHour < 0 || rot.MaxGlobalPerHour > 10000 {
		return errors.New("egress.rotation.maxGlobalPerHour 必须在 0 到 10000 之间（0=默认 6）")
	}
	if v := rot.WebhookTimeout; v != 0 && (v < time.Second || v > 10*time.Minute) {
		return errors.New("egress.rotation.webhookTimeout 必须在 1 秒到 10 分钟之间（0=默认 15s）")
	}
	if rot.WebhookRetries < 0 || rot.WebhookRetries > 10 {
		return errors.New("egress.rotation.webhookRetries 必须在 0 到 10 之间")
	}
	if v := rot.SettleDelay; v != 0 && (v < time.Second || v > 10*time.Minute) {
		return errors.New("egress.rotation.settleDelay 必须在 1 秒到 10 分钟之间（0=默认 20s）")
	}
	if v := rot.ProbeTimeout; v != 0 && (v < time.Second || v > 10*time.Minute) {
		return errors.New("egress.rotation.probeTimeout 必须在 1 秒到 10 分钟之间（0=默认 2m）")
	}
	if v := rot.ProbeInterval; v != 0 && (v < time.Second || v > time.Hour) {
		return errors.New("egress.rotation.probeInterval 必须在 1 秒到 1 小时之间（0=默认 5s）")
	}
	return nil
}

// ValidateAPIBaseURL 仅允许无凭据、query、fragment 的 HTTP(S) API 根地址。
// 文件配置适配(infra/config)与运行设置校验共用同一规则。
func ValidateAPIBaseURL(name, raw string, requireHTTPS bool) error {
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

func validStatsigID(value string) bool {
	value = strings.TrimSpace(value)
	decoded, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.StdEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) == 70
}

func optionalDuration(value *time.Duration) time.Duration {
	if value == nil {
		return 0
	}
	return *value
}

// LegacyRetentionPeriod 把旧 audit.retentionDays(天)换算为时长并校验界。
// 文件基线解析与运行设置应用共用同一规则。
func LegacyRetentionPeriod(days int) (time.Duration, error) {
	if days < 0 || days > 365 {
		return 0, fmt.Errorf("audit.retentionDays 必须在 0 到 365 之间")
	}
	return time.Duration(days) * 24 * time.Hour, nil
}
