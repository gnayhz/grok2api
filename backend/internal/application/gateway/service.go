package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrModelNotFound              = errors.New("模型不存在或未启用")
	ErrNoAvailableAccount         = errors.New("没有可用上游账号")
	ErrResponseNotFound           = errors.New("Response 不存在或已过期")
	ErrResponseAccountUnavailable = errors.New("Response 绑定的上游账号不可用")
	ErrResponseStateUnsupported   = errors.New("目标模型不支持有状态 Response")
	ErrConversationUnsupported    = errors.New("目标模型不支持当前对话协议")
	ErrVideoInputTooLarge         = errors.New("视频参考图片编码后总输入超过 32 MiB")
	ErrVideoInputUnavailable      = errors.New("视频临时输入不存在或已过期")
	ErrVideoParameterInvalid      = errors.New("视频请求参数无效")
	ErrVideoOperationUnsupported  = errors.New("视频编辑/延长仅支持路由到 Console grok-imagine-video")
	ErrLedgerUnavailable          = errors.New("计费账本暂不可用")
)

const responseOwnershipTTL = 30 * 24 * time.Hour
const finalizationTimeout = 5 * time.Second
const minimumTextBillingReservationTTL = 2 * time.Hour
const billingReservationCrashGrace = 10 * time.Minute
const mediaBillingReservationTTL = 24 * time.Hour
const modelCatalogRefreshTimeout = 30 * time.Second
const accountStateWriteTimeout = 3 * time.Second
const unlimitedRoutingAttempts = -1

type routingAttemptPolicy struct {
	limit     int
	unlimited bool
}

func newRoutingAttemptPolicy(configured int) routingAttemptPolicy {
	if configured == unlimitedRoutingAttempts {
		return routingAttemptPolicy{unlimited: true}
	}
	if configured <= 0 {
		configured = 3
	}
	return routingAttemptPolicy{limit: configured}
}

// newRequestRoutingAttemptPolicy 是请求级入口:pinned 请求(已有 ownership/
// 预选会话)恒允许恰好一次尝试,无视配置上限——重试会破坏响应状态所有权。
func newRequestRoutingAttemptPolicy(configured int, pinned bool) routingAttemptPolicy {
	if pinned {
		return routingAttemptPolicy{limit: 1}
	}
	return newRoutingAttemptPolicy(configured)
}

func (p routingAttemptPolicy) allows(attempt int) bool {
	return p.unlimited || attempt < p.limit
}

func (p routingAttemptPolicy) hasNext(attempt int) bool {
	return p.unlimited || attempt+1 < p.limit
}

// nonAccountFailureFingerprintLimit 仅限制非账号归因故障（网络/5xx 等）。
// 账号级失败持续换号，避免少量瞬时上游故障过早放弃仍可用的凭证池。
const nonAccountFailureFingerprintLimit = 16

// Stream idle failures are commonly provider-wide rather than account-wide.
// Allow one compensating account switch, then stop to prevent a silent
// upstream from multiplying a long idle deadline across the whole pool.
const streamIdleFailureFingerprintLimit = 2

var freeQuotaUsagePattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

type Input struct {
	RequestID       string
	ClientKey       clientkey.Key
	PublicModel     string
	Body            []byte
	Streaming       bool
	PromptCacheKey  string
	PromptCacheSeed string
	// AllowClientToolCacheRoute indicates that the client request is compatible with the Build mixed-tool cache route.
	// It only controls whether native x_search is added to existing client tools; it is not an authentication result.
	AllowClientToolCacheRoute bool
	PreviousResponseID        string
	// GrokTurnIndex forwards only the turn supplied by a real Grok Shell client; the server never infers or increments it.
	GrokTurnIndex string
	Operation     audit.Operation
	// auditOperation may classify a normal protocol request differently for
	// operator visibility without changing routing or Provider semantics.
	auditOperation audit.Operation
	// skipQualityHold is set only by trusted gateway-side request classifiers.
	skipQualityHold bool
}

type Usage struct {
	// Reported distinguishes a real upstream/estimated usage object from the
	// zero value used when a response fails before usage is available. Token
	// counts may legitimately all be zero, so the numeric fields cannot carry
	// this presence information by themselves.
	Reported bool
	// OutputObserved records that the transport actually forwarded generated
	// content even when an interrupted upstream never emitted final usage.
	OutputObserved         bool
	InputTokens            int64
	CachedInputTokens      int64
	OutputTokens           int64
	ReasoningTokens        int64
	TotalTokens            int64
	CostInUSDTicks         int64
	NumSourcesUsed         int64
	NumServerSideToolsUsed int64
	ContextInputTokens     int64
	ContextOutputTokens    int64
	ResponseModel          string
}

type Result struct {
	StatusCode          int
	Status              string
	Header              http.Header
	Body                io.ReadCloser
	MarkFirstToken      func()
	RecordStreamFailure func(StreamFailureDiagnostic)
	// RecordDelivery 由 transport 层在响应体转发完成后调用一次，把实际
	// 交付到客户端的事件/字节统计传回审计（轮26：回答「200 且带错误码
	// 时实际交付了多少」）。nil 时统计不记录（非推理路径）。
	RecordDelivery func(DeliveryStats)
	Finalize       func(usage Usage, responseID, errorCode string)
}

// DeliveryStats 是转发到客户端的交付统计：流式为 SSE data 事件数与累计
// 写出字节；非流式为响应体字节数（Events=1）。
type DeliveryStats struct {
	Events int64
	Bytes  int64
}

// StreamFailureDiagnostic safely projects a failure termination event returned in-stream after downstream 2xx headers.
// Body contains only transport-extracted error fields and still receives the standard redaction and size limits.
type StreamFailureDiagnostic struct {
	Body          []byte
	BodyTruncated bool
}

type auditRecorder interface {
	Create(ctx context.Context, value audit.Record) error
}

type ledgerReadinessChecker interface {
	CheckLedgerReady() error
}

type routeResolver interface {
	Get(ctx context.Context, id uint64) (modeldomain.Route, error)
	GetByPublicID(ctx context.Context, publicID string) (modeldomain.Route, error)
	GetByPublicIDCandidates(ctx context.Context, publicID string) ([]modeldomain.Route, error)
	GetByProviderUpstream(ctx context.Context, providerValue accountdomain.Provider, upstreamModel string) (modeldomain.Route, error)
	HasEnabledRouteByPublicID(ctx context.Context, publicID string) (bool, error)
}

// videoAssetStore archives and reads video results generated by a Provider.
type videoAssetStore interface {
	SaveVideo(ctx context.Context, jobID, contentType string, body io.Reader) (mediadomain.Asset, error)
	OpenVideo(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	OpenInputAsset(ctx context.Context, id string) (mediadomain.Asset, io.ReadCloser, error)
	ReleaseInputAssets(ctx context.Context, references []string) error
}

type accountModelSyncer interface {
	SyncAccount(ctx context.Context, accountID uint64) (int, error)
}

// Service handles model routing, account selection, failover, and audit finalization.
type Service struct {
	models                      routeResolver
	audits                      auditRecorder
	accounts                    *accountapp.Service
	clientKeys                  *clientkeyapp.Service
	providers                   *provider.Registry
	selector                    *Selector
	responses                   repository.ResponseRepository
	maxAttempts                 atomic.Int64
	videoMaxAttempts            atomic.Int64
	buildForbiddenReauth        atomic.Pointer[buildForbiddenReauthPolicy]
	requestTimeout              atomic.Int64
	mediaJobs                   repository.MediaJobRepository
	mediaAssets                 videoAssetStore
	mediaQueue                  chan string
	mediaMu                     sync.Mutex
	mediaQueued                 map[string]struct{}
	mediaWorker                 int
	mediaInputSlots             chan struct{}
	mediaQueueFull              atomic.Uint64
	logger                      *slog.Logger
	rateLimitMu                 sync.Mutex
	rateLimitActive             atomic.Bool
	rateLimitNextExpiry         atomic.Int64
	rateLimits                  map[string]teamModelRateLimit
	rateLimitTeams              map[uint64]teamRateLimitObservation
	modelSyncMu                 sync.Mutex
	modelSyncing                map[uint64]struct{}
	markBuildChatDeniedAsReauth atomic.Bool
	qualityRetry                atomic.Pointer[QualityRetryRuntime]
	// accountRisk receives withhold events for RSC attribution; nil disables.
	// egressGuard receives exit-IP degradation evidence (nodeID+accountID) for
	// cross-account confirmation and node quarantine; nil disables.
	egressGuard atomic.Value // EgressDegradationObserver
	// egressCanary is the exit-IP verification configuration (model route +
	// first-event budget); zero ModelPublicID disables verification.
	egressCanary atomic.Value // EgressCanaryRuntime
	accountRisk  atomic.Value // risk.Attributor
}

type teamModelRateLimit struct {
	TeamFingerprint string
	Until           time.Time
}

type teamRateLimitObservation struct {
	Fingerprint string
	ExpiresAt   time.Time
}

type buildForbiddenReauthPolicy struct {
	enabled bool
	codes   map[string]struct{}
}

func (s *Service) ConfigureMedia(repository repository.MediaJobRepository, concurrency int) {
	if concurrency <= 0 {
		concurrency = 4
	}
	s.mediaJobs = repository
	s.mediaWorker = concurrency
	s.mediaQueue = make(chan string, min(2048, max(64, concurrency*32)))
	s.mediaInputSlots = make(chan struct{}, min(concurrency, videoInputMaterializeConcurrency))
	s.mediaQueued = make(map[string]struct{})
}

// ConfigureMediaAssets injects optional local video asset archival and reading.
func (s *Service) ConfigureMediaAssets(store videoAssetStore) {
	s.mediaAssets = store
}

func NewService(models routeResolver, audits auditRecorder, accounts *accountapp.Service, clientKeys *clientkeyapp.Service, providers *provider.Registry, selector *Selector, responses repository.ResponseRepository, maxAttempts int) *Service {
	service := &Service{
		models: models, audits: audits, accounts: accounts, clientKeys: clientKeys, providers: providers,
		selector: selector, responses: responses, logger: slog.Default(),
		rateLimits: make(map[string]teamModelRateLimit), rateLimitTeams: make(map[uint64]teamRateLimitObservation),
		modelSyncing: make(map[uint64]struct{}),
	}
	service.UpdateMaxAttempts(maxAttempts)
	return service
}

// UpdateBuildForbiddenReauthPolicy atomically replaces the Build account invalidation policy.
func (s *Service) UpdateBuildForbiddenReauthPolicy(enabled bool, codes []string) {
	policy := &buildForbiddenReauthPolicy{enabled: enabled, codes: make(map[string]struct{}, len(codes))}
	for _, value := range codes {
		code := normalizeFailureCode(value)
		if code != "" {
			policy.codes[code] = struct{}{}
		}
	}
	s.buildForbiddenReauth.Store(policy)
}

func (s *Service) shouldInvalidateBuildForbidden(failure *UpstreamFailure) bool {
	if failure == nil || failure.HTTPStatus != http.StatusForbidden {
		return false
	}
	// A configured code is only a second factor. The response body must also
	// contain a high-confidence account-scoped signal; permission-denied alone
	// is shared by content, policy, and other request-level failures.
	if !failure.AccountScoped || failure.SafetyRejection || failure.RequestScopedForbidden {
		return false
	}
	policy := s.buildForbiddenReauth.Load()
	if policy == nil || !policy.enabled {
		return false
	}
	_, matched := policy.codes[normalizeFailureCode(failure.UpstreamCode)]
	return matched
}

func (s *Service) markReauthRequired(ctx context.Context, requestID string, credential accountdomain.Credential, reason string) bool {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), accountStateWriteTimeout)
	defer cancel()
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
		return false
	}
	s.selector.MarkQuotaStateChanged(credential.Provider)
	return true
}

func teamModelRateLimitKey(providerValue accountdomain.Provider, teamFingerprint, upstreamModel string) string {
	return string(providerValue) + "\x00" + teamFingerprint + "\x00" + strings.TrimSpace(upstreamModel)
}

func rateLimitTeamFingerprint(teamID string) string {
	teamID = strings.ToLower(strings.TrimSpace(teamID))
	if teamID == "" {
		return ""
	}
	return security.HashToken(teamID)
}

func shortTeamFingerprint(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:12]
}

func (s *Service) activeTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, now time.Time) (teamModelRateLimit, bool) {
	if !s.rateLimitActive.Load() {
		return teamModelRateLimit{}, false
	}
	credentialFingerprint := rateLimitTeamFingerprint(credential.TeamID)
	s.rateLimitMu.Lock()
	defer s.rateLimitMu.Unlock()
	if !s.rateLimitActive.Load() {
		return teamModelRateLimit{}, false
	}
	nextExpiry := s.rateLimitNextExpiry.Load()
	if nextExpiry <= 0 || now.UnixNano() >= nextExpiry {
		s.pruneTeamModelRateLimitsLocked(now)
		if len(s.rateLimits) == 0 {
			return teamModelRateLimit{}, false
		}
	}
	// Check the TeamID observed in an upstream response first, then current
	// credential metadata. The fallback prevents a historical observation from
	// permanently masking a later server-side team reassignment.
	observation := s.rateLimitTeams[credential.ID]
	observedFingerprint := observation.Fingerprint
	if observedFingerprint != "" && !now.Before(observation.ExpiresAt) {
		delete(s.rateLimitTeams, credential.ID)
		observedFingerprint = ""
	}
	teamFingerprints := [2]string{observedFingerprint, credentialFingerprint}
	fingerprintCount := 1
	if credentialFingerprint != observedFingerprint {
		fingerprintCount = 2
	}
	for index := 0; index < fingerprintCount; index++ {
		teamFingerprint := teamFingerprints[index]
		if teamFingerprint == "" {
			continue
		}
		key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
		value, ok := s.rateLimits[key]
		if !ok {
			continue
		}
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
			s.refreshTeamModelRateLimitStateLocked()
			continue
		}
		return value, true
	}
	return teamModelRateLimit{}, false
}

func (s *Service) pruneTeamModelRateLimitsLocked(now time.Time) {
	for key, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, key)
		}
	}
	for accountID, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, accountID)
		}
	}
	s.refreshTeamModelRateLimitStateLocked()
}

func (s *Service) refreshTeamModelRateLimitStateLocked() {
	if len(s.rateLimits) == 0 {
		clear(s.rateLimitTeams)
		s.rateLimitNextExpiry.Store(0)
		s.rateLimitActive.Store(false)
		return
	}
	var nextExpiry time.Time
	for _, value := range s.rateLimits {
		if nextExpiry.IsZero() || value.Until.Before(nextExpiry) {
			nextExpiry = value.Until
		}
	}
	for _, observation := range s.rateLimitTeams {
		if nextExpiry.IsZero() || observation.ExpiresAt.Before(nextExpiry) {
			nextExpiry = observation.ExpiresAt
		}
	}
	s.rateLimitNextExpiry.Store(nextExpiry.UnixNano())
	s.rateLimitActive.Store(true)
}

func (s *Service) markTeamModelRateLimit(credential accountdomain.Credential, upstreamModel string, metadata provider.RateLimitMetadata, now time.Time) teamModelRateLimit {
	retryAfter := metadata.RetryAfter
	if retryAfter <= 0 {
		// RPS limits recover within about one second; do not apply the generic 1m cooldown.
		if strings.EqualFold(metadata.Scope, provider.RateLimitScopeRPS) {
			retryAfter = 2 * time.Second
		} else {
			retryAfter = time.Minute
		}
	}
	teamFingerprint := rateLimitTeamFingerprint(metadata.TeamID)
	value := teamModelRateLimit{TeamFingerprint: shortTeamFingerprint(teamFingerprint), Until: now.Add(retryAfter)}
	key := teamModelRateLimitKey(credential.Provider, teamFingerprint, upstreamModel)
	until := now.Add(retryAfter)
	s.rateLimitMu.Lock()
	s.rateLimitActive.Store(true)
	if s.rateLimits == nil {
		s.rateLimits = make(map[string]teamModelRateLimit)
	}
	if s.rateLimitTeams == nil {
		s.rateLimitTeams = make(map[uint64]teamRateLimitObservation)
	}
	if teamFingerprint != rateLimitTeamFingerprint(credential.TeamID) {
		s.rateLimitTeams[credential.ID] = teamRateLimitObservation{Fingerprint: teamFingerprint, ExpiresAt: until}
	} else {
		delete(s.rateLimitTeams, credential.ID)
	}
	for existingKey, value := range s.rateLimits {
		if !now.Before(value.Until) {
			delete(s.rateLimits, existingKey)
		}
	}
	for accountID, observation := range s.rateLimitTeams {
		if !now.Before(observation.ExpiresAt) {
			delete(s.rateLimitTeams, accountID)
		}
	}
	if current, ok := s.rateLimits[key]; ok && !current.Until.Before(until) {
		value = current
	} else {
		s.rateLimits[key] = value
	}
	s.refreshTeamModelRateLimitStateLocked()
	s.rateLimitMu.Unlock()
	return value
}

func (s *Service) SetLogger(logger *slog.Logger) {
	if logger != nil {
		s.logger = logger
	}
}

func (s *Service) UpdateMaxAttempts(maxAttempts int) { s.maxAttempts.Store(int64(maxAttempts)) }

// UpdateVideoMaxAttempts configures create-phase account failover for video jobs.
// 0 is treated as the general default pool size for legacy configs.
func (s *Service) UpdateVideoMaxAttempts(maxAttempts int) {
	s.videoMaxAttempts.Store(int64(maxAttempts))
}

// UpdateMarkBuildChatDeniedAsReauth 热更新 Build chat 永久拒绝是否标 reauthRequired。
// 默认 false：仅模型级冷却；true 时按旧逻辑将账号标为失效并出池。
func (s *Service) UpdateMarkBuildChatDeniedAsReauth(enabled bool) {
	s.markBuildChatDeniedAsReauth.Store(enabled)
}

func (s *Service) UpdateRequestTimeout(value time.Duration) {
	if value <= 0 {
		value = minimumTextBillingReservationTTL
	}
	s.requestTimeout.Store(int64(value))
}

func (s *Service) textBillingReservationTTL() time.Duration {
	ttl := time.Duration(s.requestTimeout.Load()) + finalizationTimeout + billingReservationCrashGrace
	return max(minimumTextBillingReservationTTL, ttl)
}

func (s *Service) checkLedgerReady() error {
	checker, ok := s.audits.(ledgerReadinessChecker)
	if !ok {
		return nil
	}
	if err := checker.CheckLedgerReady(); err != nil {
		return ErrLedgerUnavailable
	}
	return nil
}

func (s *Service) CreateResponse(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationResponses
	switch classifyResponsesCompactionRequest(input.Body) {
	case responsesCompactionTrigger:
		input.Operation = audit.OperationCompaction
	case responsesCompactionTUI:
		// Grok TUI compaction is still a normal Responses request. Keep its
		// routing, Provider normalization, and stored-response behavior intact;
		// only its audit classification and quality-hold policy differ.
		input.auditOperation = audit.OperationCompaction
		input.skipQualityHold = true
	}
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CreateChatCompletion(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationChat
	return s.createResponseAt(ctx, input, "/responses")
}

// CreateMessage executes an Anthropic Messages request through the unified Responses upstream.
func (s *Service) CreateMessage(ctx context.Context, input Input) (*Result, error) {
	input.Operation = audit.OperationMessages
	return s.createResponseAt(ctx, input, "/responses")
}

func (s *Service) CompactResponse(ctx context.Context, input Input) (*Result, error) {
	input.Streaming = false
	input.Operation = audit.OperationCompaction
	return s.createResponseAt(ctx, input, "/responses/compact")
}

// resolvePublicModelRoutes supports both unprefixed downstream model names and explicitly sourced compatibility names.
// Registered Provider aliases are stable compatibility contracts. allowModelAliases gates only dynamically generated
// reasoning-effort aliases so existing clients keep working after the per-key discovery switch is introduced.
// distinguishMissingOrNoAccount 在候选查询返回 ErrNotFound 后区分「模型
// 不存在」（透传 ErrNotFound → 调用者映射 404）与「路由已启用但 Provider
// 当前无可用账号」（ErrNoAvailableAccount → 503 upstream_unavailable，
// 可重试）。availableRoutePredicate 要求路由绑有启用账号，无账号
// Provider 的路由在候选查询里整体消失——没有这一步，无账号 Provider 上
// 的模型会被误报成不存在（2026-08-21 实测：grok-4.20-0309-reasoning 无
// console 账号时返回 404；同日对抗审查发现 effort 别名出口
// grok-4.3-low 同样漏判）。所有候选为空的失败出口都必须经过这里。
func (s *Service) distinguishMissingOrNoAccount(ctx context.Context, publicModel string, err error) error {
	if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if exists, existsErr := s.models.HasEnabledRouteByPublicID(ctx, publicModel); existsErr == nil && exists {
		return ErrNoAvailableAccount
	}
	return err
}

func (s *Service) resolvePublicModelRoutes(ctx context.Context, publicModel string, allowModelAliases bool) ([]modeldomain.Route, string, error) {
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err == nil {
		return routes, "", nil
	}
	if s.providers != nil {
		if alias, ok := s.providers.ResolveModelAlias(publicModel); ok {
			if alias.Provider != "" && alias.UpstreamModel != "" {
				route, routeErr := s.models.GetByProviderUpstream(ctx, alias.Provider, alias.UpstreamModel)
				if routeErr != nil {
					// GetByProviderUpstream 同样带账号可用性谓词：无账号 Provider
					// 的固定别名（如 grok-4.3-low）在此为空。alias.PublicModel 是
					// canonical 路由名，可复用同一消歧（round 16 活体发现此出口
					// 漏判——直查名已 503 而固定别名仍 404）。
					return nil, "", s.distinguishMissingOrNoAccount(ctx, alias.PublicModel, routeErr)
				}
				return []modeldomain.Route{route}, alias.ReasoningEffort, nil
			}
			routes, resolveErr := s.models.GetByPublicIDCandidates(ctx, alias.PublicModel)
			if resolveErr != nil {
				resolveErr = s.distinguishMissingOrNoAccount(ctx, alias.PublicModel, resolveErr)
			}
			return routes, alias.ReasoningEffort, resolveErr
		}
	}
	// Dynamic effort-suffix aliases (e.g. grok-4.5-low) for any Provider that
	// exposes the base model. Fixed-reasoning Providers may compatibility-accept
	// an alias while their wire normalizer drops the unsupported effort.
	if base, effort, ok := modeldomain.ParseReasoningModelAlias(publicModel); ok {
		if !allowModelAliases {
			return nil, "", err
		}
		routes, resolveErr := s.models.GetByPublicIDCandidates(ctx, base)
		if resolveErr != nil {
			return nil, "", s.distinguishMissingOrNoAccount(ctx, base, resolveErr)
		}
		eligible := make([]modeldomain.Route, 0, len(routes))
		for _, route := range routes {
			if modeldomain.SupportsReasoningEffortForProvider(route.Provider, route.PublicID, effort) ||
				modeldomain.IsFixedReasoningForProvider(route.Provider, route.PublicID) {
				eligible = append(eligible, route)
			}
		}
		if len(eligible) == 0 {
			return nil, "", repository.ErrNotFound
		}
		return eligible, effort, nil
	}
	// 候选为空且无别名可解析：见 distinguishMissingOrNoAccount。
	return nil, "", s.distinguishMissingOrNoAccount(ctx, publicModel, err)
}

// eligibleConversationRoutes filters route targets without choosing one. Keeping
// this separate from ordering lets one public name form a schedulable target pool.
func (s *Service) eligibleConversationRoutes(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) ([]modeldomain.Route, modeldomain.Route, error) {
	if len(routes) == 0 || s.providers == nil {
		return nil, modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	eligible := make([]modeldomain.Route, 0, len(routes))
	accountScope := key.AccountScope()
	matchedOwnership := ownership == nil
	scopeMatched := false
	allowed := false
	conversationSupported := false
	storedResponseUnsupported := false
	for _, route := range routes {
		if ownership != nil {
			if ownership.ModelRouteID != 0 {
				if route.ID != ownership.ModelRouteID {
					continue
				}
			} else if route.Provider != ownership.Provider {
				// Backward compatibility for ownership rows created before route IDs
				// were persisted: retain the original Provider-scoped pin.
				continue
			}
		}
		matchedOwnership = true
		fallback = route
		if !accountScope.AllowsProvider(route.Provider) {
			continue
		}
		scopeMatched = true
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if !s.providers.SupportsConversation(route.Provider, string(operation)) {
			continue
		}
		conversationSupported = true
		if path == "/responses/compact" && !s.providers.SupportsResponseCompaction(route.Provider) {
			continue
		}
		if requireStoredResponse && !s.providers.SupportsStoredResponses(route.Provider) {
			storedResponseUnsupported = true
			continue
		}
		eligible = append(eligible, route)
	}
	if len(eligible) > 0 {
		return eligible, fallback, nil
	}
	if !matchedOwnership {
		return nil, fallback, ErrResponseAccountUnavailable
	}
	if !scopeMatched {
		return nil, fallback, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	if storedResponseUnsupported {
		return nil, fallback, ErrResponseStateUnsupported
	}
	if conversationSupported && path == "/responses/compact" {
		return nil, fallback, ErrConversationUnsupported
	}
	return nil, fallback, ErrConversationUnsupported
}

// selectConversationRoute retains the legacy single-target helper for callers
// that do not need target-pool ordering.
func (s *Service) selectConversationRoute(routes []modeldomain.Route, key clientkey.Key, operation audit.Operation, path string, requireStoredResponse bool, ownership *inferencedomain.ResponseOwnership) (modeldomain.Route, error) {
	eligible, fallback, err := s.eligibleConversationRoutes(routes, key, operation, path, requireStoredResponse, ownership)
	if err != nil {
		return fallback, err
	}
	return eligible[0], nil
}

// orderConversationRouteTargets randomizes targets within the same Provider by
// rendezvous score. Provider priority remains stable, while a session seed keeps
// Codex/Claude continuations on the same target without global mutable state.
func orderConversationRouteTargets(routes []modeldomain.Route, seed string) []modeldomain.Route {
	ordered := append([]modeldomain.Route(nil), routes...)
	sort.SliceStable(ordered, func(left, right int) bool {
		leftPriority := routeProviderPriority(ordered[left].Provider)
		rightPriority := routeProviderPriority(ordered[right].Provider)
		if leftPriority != rightPriority {
			return leftPriority < rightPriority
		}
		leftScore := routeTargetScore(seed, ordered[left].ID)
		rightScore := routeTargetScore(seed, ordered[right].ID)
		if leftScore != rightScore {
			return leftScore > rightScore
		}
		return ordered[left].ID < ordered[right].ID
	})
	return ordered
}

func routeTargetScore(seed string, routeID uint64) uint64 {
	digest := sha256.Sum256([]byte(seed + ":" + strconv.FormatUint(routeID, 10)))
	return binary.BigEndian.Uint64(digest[:8])
}

func routeProviderPriority(providerValue accountdomain.Provider) int {
	switch providerValue {
	case accountdomain.ProviderBuild:
		return 0
	case accountdomain.ProviderWeb:
		return 1
	case accountdomain.ProviderConsole:
		return 2
	default:
		return 3
	}
}

func routeTargetSeed(input Input) string {
	// Match the Build account-affinity precedence so Codex and Claude Code keep
	// both the route target and account stable across one logical session.
	anchor := strings.TrimSpace(input.PromptCacheSeed)
	if anchor == "" {
		anchor = strings.TrimSpace(input.PromptCacheKey)
	}
	if anchor == "" {
		system, firstUser, _ := extractMessageAnchors(input.Body)
		system = truncateAnchor(system, 100)
		firstUser = truncateAnchor(firstUser, 200)
		if firstUser != "" {
			anchor = "soft:" + system + ":" + firstUser
		}
	}
	if anchor == "" {
		anchor = strings.TrimSpace(input.RequestID)
	}
	return strconv.FormatUint(input.ClientKey.ID, 10) + ":" + anchor
}

// selectMediaRoute selects a same-name route that satisfies media capability, key permissions, and Provider support.
func (s *Service) selectMediaRoute(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, error) {
	eligible, fallback, err := s.eligibleMediaRoutes(routes, key, capability, providerSupported)
	if err != nil {
		return fallback, err
	}
	return eligible[0], nil
}

func (s *Service) eligibleMediaRoutes(routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, providerSupported func(accountdomain.Provider) bool) ([]modeldomain.Route, modeldomain.Route, error) {
	if len(routes) == 0 {
		return nil, modeldomain.Route{}, ErrModelNotFound
	}
	fallback := routes[0]
	eligible := make([]modeldomain.Route, 0, len(routes))
	accountScope := key.AccountScope()
	capabilityMatched := false
	scopeMatched := false
	allowed := false
	for _, route := range routes {
		if route.Capability != capability {
			continue
		}
		fallback = route
		capabilityMatched = true
		if !accountScope.AllowsProvider(route.Provider) {
			continue
		}
		scopeMatched = true
		if !s.clientKeys.CanUseModel(key, route.ID) {
			continue
		}
		allowed = true
		if providerSupported(route.Provider) {
			eligible = append(eligible, route)
		}
	}
	if len(eligible) > 0 {
		return eligible, fallback, nil
	}
	if !capabilityMatched {
		return nil, fallback, ErrModelNotFound
	}
	if !scopeMatched {
		return nil, fallback, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	return nil, fallback, ErrNoAvailableAccount
}

// selectSchedulableMediaRoute resolves a concrete same-name media target and
// its immutable account plan together. A cooling or exhausted first target
// therefore cannot hide a healthy target from another Provider.
func (s *Service) selectSchedulableMediaRoute(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, *selectionSession, error) {
	return s.selectSchedulableMediaRouteWithQuotaMode(ctx, routes, key, capability, consumesQuota, providerSupported, nil)
}

func (s *Service) selectSchedulableMediaRouteWithQuotaMode(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selectionSession, error) {
	eligible, fallback, err := s.eligibleMediaRoutes(routes, key, capability, providerSupported)
	if err != nil {
		return fallback, nil, err
	}
	return s.selectSchedulableEligibleMediaRouteWithQuotaMode(ctx, eligible, key, consumesQuota, resolveQuotaMode)
}

// selectSchedulableEligibleMediaRouteWithQuotaMode selects an account plan
// from routes that already passed capability, client-key, and Provider support
// checks. Callers may apply request-specific route constraints between the
// eligibility and scheduling phases without evaluating disallowed routes.
func (s *Service) selectSchedulableEligibleMediaRouteWithQuotaMode(ctx context.Context, eligible []modeldomain.Route, key clientkey.Key, consumesQuota bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selectionSession, error) {
	if len(eligible) == 0 {
		return modeldomain.Route{}, nil, ErrNoAvailableAccount
	}
	var firstSelectionErr error
	for _, route := range eligible {
		quotaMode := ""
		if consumesQuota {
			if resolveQuotaMode != nil {
				quotaMode = resolveQuotaMode(route)
			} else {
				quotaMode = s.providers.QuotaMode(route.Provider, route.UpstreamModel)
			}
		}
		session, selectionErr := s.selector.beginSelectionSessionForKey(
			ctx,
			route.Provider,
			route.ID,
			route.UpstreamModel,
			quotaMode,
			"",
			nil,
			false,
			key.AccountScope(),
		)
		if selectionErr == nil {
			return route, session, nil
		}
		if firstSelectionErr == nil {
			firstSelectionErr = selectionErr
		}
	}
	if firstSelectionErr == nil {
		firstSelectionErr = ErrNoAvailableAccount
	}
	return eligible[0], nil, firstSelectionErr
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (*Result, error) {
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	var firstToken *firstTokenTimer
	if input.Streaming {
		firstToken = newFirstTokenTimer(startedAt)
	}
	eventID := newAuditEventID()
	// Use a server-generated scope so repeated or absent client request IDs
	// cannot accidentally join independent Composer conversations.
	requestSessionScope := eventID
	operation := input.Operation
	if operation == "" {
		operation = audit.OperationResponses
	}
	auditOperation := operation
	if input.auditOperation != "" {
		auditOperation = input.auditOperation
	}
	routes, aliasEffort, err := s.resolvePublicModelRoutes(ctx, input.PublicModel, input.ClientKey.AllowModelAliases)
	if err != nil {
		// 无账号（路由在、Provider 当前无可用账号）是可重试的 503 语义，
		// 不能与「模型不存在」（404）一起被扁平化——否则客户端会把暂时
		// 性不可服务当成永久性配置错误放弃重试。
		if errors.Is(err, ErrNoAvailableAccount) {
			return nil, err
		}
		if errors.Is(err, repository.ErrNotFound) {
			return nil, ErrModelNotFound
		}
		// DB 瞬态故障(超时/busy)原样上抛→503:此前与"模型不存在"一起被扁平成
		// 404, SDK 会把暂时性不可服务当成永久性配置错误放弃重试。
		return nil, err
	}
	// Select an initial route only to preserve the existing stateful/stateless
	// previous_response_id boundary. The actual target is chosen from the eligible
	// pool below after ownership and account availability are known.
	initialRoute, routeErr := s.selectConversationRoute(routes, input.ClientKey, operation, path, false, nil)
	var ownership *inferencedomain.ResponseOwnership
	if input.PreviousResponseID != "" && routeErr == nil {
		if s.providers.SupportsStoredResponses(initialRoute.Provider) {
			value, ownershipErr := s.responses.Get(ctx, input.PreviousResponseID, input.ClientKey.ID, time.Now().UTC())
			if ownershipErr != nil {
				return nil, ErrResponseNotFound
			}
			ownership = &value
		} else if initialRoute.Provider == accountdomain.ProviderConsole {
			// Console does not retain Response state, so replay the history statelessly here;
			// Provider normalization removes stale Response IDs.
			input.PreviousResponseID = ""
		} else {
			return nil, ErrResponseStateUnsupported
		}
	}
	eligibleRoutes, fallbackRoute, routeErr := s.eligibleConversationRoutes(routes, input.ClientKey, operation, path, ownership != nil, ownership)
	route := fallbackRoute
	orderedRoutes := eligibleRoutes
	if routeErr == nil {
		orderedRoutes = orderConversationRouteTargets(eligibleRoutes, routeTargetSeed(input))
		route = orderedRoutes[0]
	}
	accountScope := input.ClientKey.AccountScope()
	var preselectedSession *selectionSession
	// Skip targets whose account pool is already known to be unavailable. This
	// gives same-name targets failover before any physical upstream request while
	// preserving pinned Responses.
	if routeErr == nil && ownership == nil {
		for _, candidate := range orderedRoutes {
			affinityKey := ""
			if candidate.Provider == accountdomain.ProviderBuild {
				identity := resolveBuildSessionIdentity(
					input.ClientKey.ID,
					candidate.Provider,
					candidate.UpstreamModel,
					input.PromptCacheKey,
					input.PromptCacheSeed,
					input.RequestID,
					input.Body,
				)
				identity = ensureBuildComposerSessionIdentity(identity, input.ClientKey.ID, candidate.Provider, candidate.UpstreamModel, requestSessionScope)
				affinityKey = identity.affinityKey
			}
			candidateSession, selectionErr := s.selector.beginSelectionSessionForKey(
				ctx,
				candidate.Provider,
				candidate.ID,
				candidate.UpstreamModel,
				s.providers.QuotaMode(candidate.Provider, candidate.UpstreamModel),
				affinityKey,
				nil,
				true,
				accountScope,
			)
			if selectionErr != nil {
				continue
			}
			route = candidate
			preselectedSession = candidateSession
			break
		}
	}
	publicModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	input.PublicModel = publicModel
	if aliasEffort != "" {
		input.Body, err = rewriteAliasedModel(input.Body, publicModel, aliasEffort, operation)
		if err != nil {
			return nil, err
		}
	}
	if routeErr != nil && !errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		return nil, routeErr
	}
	timing := newGenerationTiming(publicModel, route.Provider)
	timingHandedOff := false
	defer func() {
		if !timingHandedOff {
			timing.finish(s.logger, "failed")
		}
	}()
	usageSource := audit.UsageSourceUpstream
	if usageKind, _ := s.providers.UsageKind(route.Provider); usageKind == provider.UsageEstimated {
		usageSource = audit.UsageSourceEstimated
	}
	mediaSummary, _ := summarizeResponseMedia(input.Body)
	logResponseMediaSummary(s.logger, input.RequestID, mediaSummary)
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: publicModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: auditOperation, UsageSource: audit.UsageSourceNone, Streaming: input.Streaming,
		MediaInputImages: mediaSummary.InputImages,
	}
	if errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		record := auditBase
		record.StatusCode = http.StatusForbidden
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.ErrorCode = "model_not_allowed"
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, egressTrace, route.Provider)
		if err := s.audits.Create(ctx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		return nil, clientkeyapp.ErrModelNotAllowed
	}
	affinityKey := ""
	ownershipPromptCacheKey := ""
	reasoningReplayKey := ""
	if route.Provider == accountdomain.ProviderBuild {
		// Derive a stable identity from explicit session signals, message anchors,
		// and model. Composer replaces message-only fallback identities with an
		// isolated request identity that remains stable across retries.
		identity := buildSessionIdentity{}
		if ownership != nil && ownership.PromptCacheKey != "" {
			// previous_response_id belongs to an existing Response chain and must inherit the root session identity;
			// do not recompute the soft key from this turn's incremental input.
			identity.upstreamID = ownership.PromptCacheKey
			identity.replayKey = ownership.ReasoningReplayKey
		} else {
			identity = resolveBuildSessionIdentity(
				input.ClientKey.ID,
				route.Provider,
				route.UpstreamModel,
				input.PromptCacheKey,
				input.PromptCacheSeed,
				input.RequestID,
				input.Body,
			)
		}
		identity = ensureBuildComposerSessionIdentity(identity, input.ClientKey.ID, route.Provider, route.UpstreamModel, requestSessionScope)
		input.PromptCacheKey = identity.upstreamID
		affinityKey = identity.affinityKey
		ownershipPromptCacheKey = identity.upstreamID
		reasoningReplayKey = identity.replayKey
		if identity.upstreamID == "" {
			s.logger.Debug("prompt_cache_session_empty", "request_id", input.RequestID, "model", route.UpstreamModel, "provider", route.Provider)
		} else if identity.soft {
			s.logger.Debug("prompt_cache_session_soft", "request_id", input.RequestID, "model", route.UpstreamModel)
		} else if identity.isolated {
			s.logger.Debug("prompt_cache_session_isolated", "request_id", input.RequestID, "model", route.UpstreamModel)
		}
	}
	adapter, ok := s.providers.Responses(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(route.Provider), string(operation))
	// degradedNodes 收集本请求内被守卫判定降智的出口节点。注入
	// WithNodeExclusions 后,后续 attempt(含同号重试)会换到其他固定出口
	// IP;账号绑定不变,仅本请求绕开。空流/扣留/头预算三条降智路径共用。
	degradedNodes := make(map[uint64]struct{})
	markDegradedEgress := func() uint64 {
		nodeID := degradedEgressNodeID(egressTrace, route.Provider)
		if nodeID == 0 {
			return 0
		}
		if _, exists := degradedNodes[nodeID]; !exists {
			degradedNodes[nodeID] = struct{}{}
			physicalCallCtx = infraegress.WithNodeExclusions(physicalCallCtx, degradedNodes)
			// L2 软冷却：降智证据立即让全池账号避开该出口(不等归因),
			// 归因 CLEAN 升级硬隔离 / RISK 解除 / 到期自动回池并指数递增。
			if observer := s.egressDegradationObserver(); observer != nil {
				observer.MarkDegradeEvidence(nodeID)
			}
		}
		return nodeID
	}
	reportEgressDegradation := func(credential accountdomain.Credential) {
		nodeID := markDegradedEgress()
		if nodeID == 0 {
			return
		}
		if observer := s.egressDegradationObserver(); observer != nil {
			observer.OnEgressDegraded(ctx, nodeID, credential.ID)
		}
	}
	supportsStoredResponses := s.providers.SupportsStoredResponses(route.Provider)
	if input.PreviousResponseID != "" && !supportsStoredResponses {
		return nil, ErrResponseStateUnsupported
	}
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	idempotencyID, _ := security.NewOpaqueToken(18)
	if ownership != nil {
		attemptPolicy = newRoutingAttemptPolicy(1)
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	if reservation, priced := audit.EstimateOfficialTextReservation(pricingModel, input.Body); priced {
		if _, err := s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, s.textBillingReservationTTL()); err != nil {
			return nil, err
		}
	}
	excluded := make(map[uint64]bool)
	failureFingerprints := make(map[string]int)
	authRecoveryAttempted := make(map[uint64]bool)
	holdCfg := s.qualityRetryConfig()
	qualityHoldEnabled := shouldHoldQualityStream(input, ownership, route, operation, holdCfg)
	// Real-time guard observability. Gate lines are emitted only when the hold
	// is engaged: a per-request INFO line while the feature is off would be pure
	// log amplification proportional to traffic.
	if qualityHoldEnabled {
		s.logger.Info("quality_hold_gate", "request_id", input.RequestID, "cfg_enabled", holdCfg.Enabled, "provider", route.Provider, "public_model", input.PublicModel, "upstream_model", route.UpstreamModel, "operation", operation)
	}
	// Count accounts that actually reached the upstream. Credential-only skips
	// do not consume the quality retry budget; refreshes stay on the same account.
	qualityAccountAttempts := 0
	// sameAccountRetried marks that the quality withhold path already used its
	// single same-account retry for this request (see QualityRetryRuntime).
	sameAccountRetried := false
	// headerBudgetArmed 保留单发机制的装填位（helper 参数），但预算现已
	// 对每次流式尝试持续生效（见下方 fired 分支）：实测健康流式的响应头
	// 恒定秒级返回（0.7-2.2s 含代理），而降智复杂生成的头要等整个生成
	// 完成（75-300s，2026-08-21 魔法球实测：单发解除后第二次慢头尝试
	// 悬挂满 5 分钟 ResponseHeaderTimeout）。持续装填把每次头等待都压进
	// 预算内。非流式不受预算约束（其头=生成完成，见 qualityHeaderBudget）。
	headerBudgetArmed := true
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaProbeAttempted := false
	selection := preselectedSession
	var lastErr error
	var lastFailure *UpstreamFailure
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, path)
	normalizedMetadata := &provider.NormalizedRequestMetadata{}
	responseStartedAt := startedAt
	forwardResponse := func(lease *accountLease, credential accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
		started := time.Now()
		responseStartedAt = started
		lease.markSelectorUpstreamStarted()
		request := provider.ResponseResourceRequest{Credential: credential, Billing: billing, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, ReasoningReplayKey: reasoningReplayKey, AllowClientToolCacheRoute: input.AllowClientToolCacheRoute, GrokTurnIndex: input.GrokTurnIndex, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation), NormalizedMetadata: normalizedMetadata}
		var response *provider.Response
		var err error
		if budget := qualityHeaderBudget(holdCfg, qualityHoldEnabled, input.Streaming, headerBudgetArmed); budget > 0 {
			// 响应头预算早断：健康推理路径的头恒定秒级返回，降智路径要等
			// 整个生成完成。预算内头未到即中止换路径，把降智判定从首字节
			// 提前到头阶段（复杂问题可省数十秒）。
			callCtx, cancel := context.WithCancel(physicalCallCtx)
			fired := &atomic.Bool{}
			timerDone := make(chan struct{})
			timer := time.AfterFunc(budget, func() {
				// defer close 保证 channel 关闭时 fired 已置位且 cancel 已
				// 执行——主线程读到的是最终态。
				defer close(timerDone)
				fired.Store(true)
				cancel()
			})
			response, err = adapter.ForwardResponse(callCtx, request)
			if !timer.Stop() {
				// Stop()=false 仅表示回调已触发，不代表已完成：回调可能停在
				// fired.Store 之前。等回调结束再判定，消除“成功交付一个随后
				// 才被取消的 body”窗口。回调无阻塞操作，等待必返回。
				<-timerDone
			}
			if err != nil {
				// Go 惯例 err!=nil 时 response 仍可能非 nil：未关闭会泄漏连接。
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				cancel()
				if fired.Load() {
					s.logger.Warn("quality_degraded_header_budget_abort", "request_id", input.RequestID, "account_id", credential.ID, "budget", budget.String(), "elapsed", time.Since(started).String())
					// 哨兵错误刻意不链底层 context.Canceled：父请求仍在进行。
					err = errQualityHeaderBudget
				}
			} else if fired.Load() {
				// 成功与超时竞态：头恰在预算边缘返回，但 timer 已 cancel 了
				// callCtx，body 读取必然立即失败且不会重试。统一按预算中止
				// 处理——关闭竞态体，转哨兵错误走换路径重试。
				cancel()
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				response = nil
				s.logger.Warn("quality_degraded_header_budget_abort", "request_id", input.RequestID, "account_id", credential.ID, "budget", budget.String(), "elapsed", time.Since(started).String(), "race", true)
				err = errQualityHeaderBudget
			} else {
				// 头在预算内正常返回且未触发：body 生命周期必须长于 callCtx
				// （peek/客户端读取发生在 forwardResponse 返回之后）。这里不
				// cancel，泄漏的 context 由父 physicalCallCtx 释放兜底——
				// WithCancel 子 ctx 会随父 ctx 取消而释放，无真实泄漏。
			}
		} else {
			response, err = adapter.ForwardResponse(physicalCallCtx, request)
		}
		auditBase.ReasoningEffort = normalizedMetadata.ReasoningEffort
		err = failureAttempts.captureResponse(credential, started, response, err)
		timing.markUpstream(time.Since(started))
		return response, err
	}
	ensureCredential := func(credential accountdomain.Credential, force bool) (accountdomain.Credential, error) {
		started := time.Now()
		result, err := s.accounts.EnsureCredential(ctx, credential, force)
		failureAttempts.captureCredentialFailure(credential, started, force, err)
		timing.markCredential(time.Since(started))
		return result, err
	}
	handoffResponse := func(response *provider.Response, lease *accountLease, credential accountdomain.Credential, upstreamStartedAt time.Time, qualityFailOpen bool) *Result {
		accountID := credential.ID
		var once sync.Once
		// 交付统计由 transport 在转发完成后回填，finalize 时进审计行。
		var delivery DeliveryStats
		var deliverySet bool
		recordDelivery := func(stats DeliveryStats) {
			delivery = stats
			deliverySet = true
		}
		finalize := func(usage Usage, responseID, errorCode string) {
			once.Do(func() {
				// HTTP 状态码保留线上真实值；流在 2xx 响应头之后失败时由 errorCode
				// 决定最终结果，避免把协议状态与业务结果混为一谈。
				successful := auditRequestSucceeded(response.StatusCode, errorCode)
				lease.completeSelectorObservation(successful)
				budget := newFinalizationBudget(string(operation), string(route.Provider))
				if isUpstreamStreamFailure(errorCode) {
					status, retryAfter := streamFailureHealthPenalty(errorCode, usage, holdCfg.IdleAccountCooldown)
					if err := budget.run("account_health", finalizationHealthBudget, func(stageCtx context.Context) error {
						return s.selector.MarkFailureAfterSuccess(stageCtx, credential, status, retryAfter)
					}); err != nil {
						s.logger.Warn("stream_failure_health_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
					}
				}
				lease.Release()
				now := time.Now().UTC()
				record := auditBase
				if usage.Reported {
					record.UsageSource = usageSource
				}
				record.AccountID = &accountID
				record.AccountName = credential.Name
				record.StatusCode = response.StatusCode
				record.QualityFailOpen = qualityFailOpen
				record.InputTokens = usage.InputTokens
				record.CachedInputTokens = usage.CachedInputTokens
				record.OutputTokens = usage.OutputTokens
				record.ReasoningTokens = usage.ReasoningTokens
				record.TotalTokens = usage.TotalTokens
				record.CostInUSDTicks = usage.CostInUSDTicks
				imagePricing, imagePriced := audit.EstimateOfficialImageCost(pricingModel, "", "", response.QuotaUnits)
				if imagePriced {
					record.MediaOutputImages = int64(max(0, response.QuotaUnits))
				}
				tokenPricing, tokenPriced := audit.EstimateOfficialCost(pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens)
				if successful && imagePriced {
					record.EstimatedCostInUSDTicks = imagePricing.CostInUSDTicks
					record.PricingModel = imagePricing.Model
					record.PricingVersion = audit.OfficialPricingAsOf
				} else if tokenPriced {
					record.EstimatedCostInUSDTicks = tokenPricing.CostInUSDTicks
					record.PricingModel = tokenPricing.Model
					record.PricingVersion = audit.OfficialPricingAsOf
				}
				record.NumSourcesUsed = usage.NumSourcesUsed
				record.NumServerSideToolsUsed = usage.NumServerSideToolsUsed
				record.ContextInputTokens = usage.ContextInputTokens
				record.ContextOutputTokens = usage.ContextOutputTokens
				if successful && input.Streaming {
					record.FirstTokenMS = firstToken.milliseconds()
				}
				if deliverySet {
					record.DeliveredEvents = delivery.Events
					record.DeliveredBytes = delivery.Bytes
				}
				record.DurationMS = time.Since(startedAt).Milliseconds()
				record.ErrorCode = errorCode
				attempts := failureAttempts.snapshot()
				if !successful || len(attempts) > 0 {
					record.Attempts = attempts
				}
				record.CreatedAt = now
				applyAuditEgress(&record, egressTrace, route.Provider)
				if supportsStoredResponses && operation == audit.OperationResponses && responseID != "" && successful {
					err := budget.run("response_ownership", finalizationOwnershipBudget, func(stageCtx context.Context) error {
						return s.responses.Save(stageCtx, inferencedomain.ResponseOwnership{ResponseID: responseID, AccountID: accountID, ClientKeyID: input.ClientKey.ID, ModelRouteID: route.ID, Provider: route.Provider, PromptCacheKey: ownershipPromptCacheKey, ReasoningReplayKey: reasoningReplayKey, ExpiresAt: now.Add(responseOwnershipTTL), CreatedAt: now, UpdatedAt: now})
					})
					if err != nil {
						s.logger.Error("response_ownership_save_failed", "response_id", responseID, "client_key_id", input.ClientKey.ID, "account_id", accountID, "provider", route.Provider, "error", err)
					}
				}
				if successful && lease.QuotaMode != "" {
					if lease.QuotaMode != "weekly" {
						units := max(1, response.QuotaUnits)
						var updated bool
						err := budget.run("quota_decrement", finalizationQuotaBudget, func(stageCtx context.Context) error {
							var decrementErr error
							updated, decrementErr = s.accounts.DecrementQuota(stageCtx, accountID, lease.QuotaMode, units)
							return decrementErr
						})
						if err != nil {
							s.logger.Warn("provider_quota_decrement_failed", "provider", credential.Provider, "account_id", accountID, "mode", lease.QuotaMode, "units", units, "error", err)
						} else if updated {
							s.selector.ConsumeQuota(credential.Provider, accountID, lease.QuotaMode, units)
						}
					}
				}
				if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
					return s.audits.Create(stageCtx, record)
				}); err != nil {
					s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
				}
				if usage.ResponseModel != "" {
					_ = budget.run("observed_model", finalizationMetadataBudget, func(stageCtx context.Context) error {
						return s.accounts.ObserveResponseModel(stageCtx, accountID, usage.ResponseModel)
					})
				}
				if successful && lease.QuotaMode != "" {
					if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow {
						s.accounts.QueueQuotaRefresh(accountID, lease.QuotaMode)
					}
				}
				outcome := "failed"
				if successful {
					outcome = "success"
				}
				timing.finish(s.logger, outcome)
			})
		}
		response.Body = &firstByteReadCloser{ReadCloser: response.Body, mark: timing.markFirstBody}
		recordStreamFailure := func(diagnostic StreamFailureDiagnostic) {
			failureAttempts.captureStreamFailure(credential, upstreamStartedAt, response, diagnostic)
		}
		var markFirstToken func()
		if firstToken != nil {
			markFirstToken = firstToken.mark
		}
		timingHandedOff = true
		return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: func() { finalize(Usage{}, "", "stream_closed") }}, MarkFirstToken: markFirstToken, RecordStreamFailure: recordStreamFailure, RecordDelivery: recordDelivery, Finalize: finalize}
	}
	// fail_open retains at most one successful no-thinking stream. The account
	// lease is released immediately; the read pump applies upstream backpressure
	// until this response is either delivered or replaced by a better one.
	type qualityFallback struct {
		response          *provider.Response
		lease             *accountLease
		credential        accountdomain.Credential
		usage             Usage
		upstreamStartedAt time.Time
	}
	var fallback *qualityFallback
	discardFallback := func(recordDegraded bool) {
		if fallback == nil {
			return
		}
		if recordDegraded {
			s.recordQualityDegraded(ctx, auditBase, fallback.credential, fallback.usage, startedAt, egressTrace, route.Provider)
			failureAttempts.captureQualityDegraded(fallback.credential, fallback.upstreamStartedAt)
		}
		_ = fallback.response.Body.Close()
		fallback = nil
	}
attemptLoop:
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if qualityHoldEnabled && qualityAccountAttempts >= holdCfg.MaxAttempts {
			break
		}
		var lease *accountLease
		var err error
		selectionStarted := time.Now()
		if ownership != nil {
			lease, err = s.selector.AcquirePinnedForKey(ctx, route.Provider, ownership.AccountID, route.ID, route.UpstreamModel, quotaMode, true, accountScope)
		} else {
			if selection == nil {
				selection, err = s.selector.beginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, affinityKey, excluded, !quotaProbeAttempted, accountScope)
			}
			if err == nil {
				lease, err = selection.Acquire(ctx, excluded, !quotaProbeAttempted)
			}
		}
		timing.markSelection(time.Since(selectionStarted))
		if err != nil {
			if lastFailure == nil {
				lastErr = err
			}
			break
		}
		excluded[lease.Credential.ID] = true
		if limited, ok := s.activeTeamModelRateLimit(lease.Credential, route.UpstreamModel, time.Now().UTC()); ok {
			lease.Release()
			lastFailure = &UpstreamFailure{
				HTTPStatus: http.StatusTooManyRequests, Code: "upstream_rate_limited", PublicMessage: "上游请求频率受限",
				AccountID: lease.Credential.ID, AccountName: lease.Credential.Name,
				Fingerprint: "429:team_model_rate_limit", RetryAfter: time.Until(limited.Until),
			}
			lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
			s.logger.Warn("upstream_team_model_rate_limit_active", "request_id", input.RequestID, "account_id", lease.Credential.ID, "provider", route.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "retry_after", lastFailure.RetryAfter.Round(time.Second))
			// Stored Responses are pinned to one account. Return the cached 429
			// immediately instead of spinning until the cooldown expires or
			// replaying the request on the same account.
			if ownership != nil {
				break attemptLoop
			}
			attempt--
			continue
		}
		if lease.QuotaProbe {
			quotaProbeAttempted = true
		}
		if lease.QuotaProbeKind == accountdomain.QuotaRecoveryKindPaid {
			recovered, probeErr := s.accounts.ProbePaidQuota(ctx, lease.Credential)
			s.selector.MarkQuotaStateChanged(lease.Credential.Provider, lease.Credential.ID)
			if probeErr != nil || !recovered {
				lease.Release()
				lastErr = firstError(probeErr, fmt.Errorf("付费额度尚未恢复"))
				continue
			}
			lease.QuotaProbe = false
			lease.QuotaProbeKind = ""
			lease.Billing = nil
		}
		credential, err := ensureCredential(lease.Credential, false)
		if err != nil {
			lease.Release()
			lastErr = err
			lastFailure = newCredentialUpstreamFailure(err, lease.Credential.ID, lease.Credential.Name)
			continue
		}
		if qualityHoldEnabled {
			qualityAccountAttempts++
		}
		response, err := forwardResponse(lease, credential, lease.Billing)
		if err != nil {
			lease.Release()
			lastErr = err
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
				break
			}
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
			if errors.Is(err, errQualityHeaderBudget) {
				// 头预算早断是降智路径特征:请求内排除该出口节点,并上报出口降级
				// 观测(RSC 归因 clean → 出口 IP 嫌疑;关闭 RSC 时走跨账号确认)。
				reportEgressDegradation(credential)
				if attributor := s.accountRiskAttributor(); attributor != nil {
					attributor.OnDegraded(ctx, credential, degradedEgressNodeID(egressTrace, route.Provider))
				}
			}
			if !isRetryableTransportFailure(credential.Provider, err) {
				break
			}
			if !neterrorpkg.IsUpstreamStreamIdleTimeout(err) {
				s.selector.MarkFailure(ctx, credential, 0, 0)
			}
			if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
				break
			}
			continue
		}
	handleResponse:
		if response.ModelCatalogChanged {
			s.queueAccountModelSync(credential.ID)
		}
		if response.StatusCode == http.StatusUnauthorized {
			response.Body.Close()
			if credential.AuthType == accountdomain.AuthTypeSSO {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				lease.Release()
				lastErr = fmt.Errorf("%s SSO 凭据已失效", credential.Provider)
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
				lease.Release()
				lastErr = accountapp.ErrCredentialRefreshPermanent
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, nil, credential.ID, credential.Name)
				continue
			}
			authRecoveryAttempted[credential.ID] = true
			refreshed, refreshErr := ensureCredential(credential, true)
			if refreshErr == nil {
				response, err = forwardResponse(lease, refreshed, lease.Billing)
				credential = refreshed
			}
			if refreshErr != nil || err != nil {
				if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
					s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
				}
				lease.Release()
				lastErr = firstError(refreshErr, err)
				if refreshErr != nil {
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
				} else if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
					break
				} else {
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(credential.Provider, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break attemptLoop
					}
				}
				continue
			}
			if response.StatusCode == http.StatusUnauthorized {
				body, _ := readRetryableBody(response.Body)
				// WithoutCancel+超时:客户端恰在此刻断开时, 失效标记不得静默丢失
				// (否则该账号留在池中继续被后续请求选中各自撞一次 401)。
				s.markReauthRequired(ctx, input.RequestID, credential, "Grok Build OAuth credential rejected after refresh")
				s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
				lease.Release()
				lastErr = fmt.Errorf("刷新后上游仍返回 401")
				lastFailure = newHTTPUpstreamFailure(http.StatusUnauthorized, body, credential.ID, credential.Name)
				continue
			}
		}
		egressForbidden := s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden
		finalEgressForbidden := egressForbidden && (attempt > 0 || !attemptPolicy.hasNext(attempt))
		// Classify 403 bodies before egress retry. Definitive blocked-account signals invalidate and rotate the account;
		// request-level safety rejections are returned as-is without account side effects;
		// all other 403 responses retain the egress retry path without penalizing the account.
		if response.StatusCode == http.StatusForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if isTerminalRequestForbidden(credential.Provider, lastFailure) {
				// Deterministic request-scoped 403: restore the original body and return it
				// without OAuth refresh, account rotation, cooldown, or invalidation.
				response.Body = io.NopCloser(bytes.NewReader(body))
				lease.completeSelectorObservation(false)
				lease.Release()
				if lastFailure.SafetyRejection {
					s.logger.Warn("upstream_safety_rejection", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode)
				} else {
					s.logger.Warn("upstream_request_scoped_forbidden", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode)
				}
				// Fall through to the common success/error response path so the client receives the original 403.
			} else if lastFailure.AccountBlocked {
				failureHandled := s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
				if lastFailure.AccountScoped && !failureHandled {
					s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
				}
				lease.Release()
				lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
				s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped, "account_blocked", true)
				continue
			} else if egressForbidden && !finalEgressForbidden {
				// A non-blocking 403 is an egress/browser-session failure and must not penalize the account.
				delete(excluded, credential.ID)
				if selection != nil {
					selection.RetryAccount(credential.ID)
				}
				lease.Release()
				lastErr = fmt.Errorf("上游出口会话被拒绝")
				continue
			} else {
				// Restore the consumed final non-blocking 403 body for the common response path.
				response.Body = io.NopCloser(bytes.NewReader(body))
			}
		}
		if isTerminalRequestForbidden(credential.Provider, lastFailure) {
			// already prepared as a terminal 403 response for the client
		} else if response.StatusCode >= 400 && !isRetryable(response.StatusCode) {
			// Non-retryable upstream 4xx, including after an account switch (a prior retryable failure must not disable sanitization):
			// headers would otherwise be forwarded verbatim to the client, leaking
			// upstream internals. Convert to a controlled UpstreamFailure that
			// preserves the status and upstream error code for diagnostics while
			// serving the client the sanitized protocol envelope.
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			_ = response.Body.Close()
			lease.completeSelectorObservation(false)
			lease.Release()
			break attemptLoop
		} else if response.StatusCode >= 400 && !isRetryableResponse(response, route.Provider) {
			// 状态本身可重试但上游显式放弃(X-Should-Retry: false, 或该 Provider 从不
			// 设置该头):与非重试分支同待遇——转受控 UpstreamFailure 脱敏, 而不是把
			// 原始 body/headers 直通客户端(400 被脱敏而 429+该头泄漏 trace-id 的
			// 不对称曾在审查中发现)。finalEgressForbidden 仍按原样交付。
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			_ = response.Body.Close()
			lease.completeSelectorObservation(false)
			lease.Release()
			break attemptLoop
		} else if isRetryableResponse(response, route.Provider) && !finalEgressForbidden {
			retryAfter := parseRetryAfter(response.Header.Get("Retry-After"), time.Now().UTC())
			body, _ := readRetryableBody(response.Body)
			lastFailure = newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
			if response.StatusCode == http.StatusTooManyRequests && response.RateLimit == nil {
				if metadata := provider.ParseRateLimitMetadata(body); metadata != nil {
					response.RateLimit = metadata
					if retryAfter <= 0 && metadata.RetryAfter > 0 {
						retryAfter = metadata.RetryAfter
					}
				}
			}
			buildForbiddenReauth := credential.Provider == accountdomain.ProviderBuild && s.shouldInvalidateBuildForbidden(lastFailure)
			if response.StatusCode == http.StatusTooManyRequests && response.RateLimit != nil && response.RateLimit.Model == route.UpstreamModel {
				rateLimitMeta := *response.RateLimit
				if strings.TrimSpace(rateLimitMeta.TeamID) == "" {
					rateLimitMeta.TeamID = strings.TrimSpace(credential.TeamID)
				}
				if rateLimitMeta.TeamID == "" {
					// Team+Model shielding requires a team identity; fall through to account-scoped 429 handling.
					goto afterTeamRateLimit
				}
				limited := s.markTeamModelRateLimit(credential, route.UpstreamModel, rateLimitMeta, time.Now().UTC())
				lastFailure.AccountScoped = false
				lastFailure.Fingerprint = "429:team_model_rate_limit"
				lastFailure.RetryAfter = time.Until(limited.Until)
				lease.Release()
				lastErr = fmt.Errorf("上游 Team 与模型请求频率受限")
				s.logger.Warn("upstream_team_model_rate_limited", "request_id", input.RequestID, "provider", credential.Provider, "model", route.UpstreamModel, "team_fingerprint", limited.TeamFingerprint, "scope", rateLimitMeta.Scope, "actual", rateLimitMeta.Actual, "limit", rateLimitMeta.Limit, "retry_after", lastFailure.RetryAfter)
				continue
			}
		afterTeamRateLimit:
			// Grok Build treats only HTTP 401 as an OAuth authentication failure.
			// A 403 is already authenticated and must not trigger token rotation or
			// replay the same request with freshly issued credentials.
			if credential.Provider != accountdomain.ProviderBuild && s.providers.SupportsCredentialRefresh(credential.Provider) && !authRecoveryAttempted[credential.ID] && credential.EncryptedRefreshToken != "" && !lastFailure.AccountBlocked && !buildForbiddenReauth && (lastFailure.PermanentAccountDenial || lastFailure.CredentialRejected) {
				authRecoveryAttempted[credential.ID] = true
				refreshed, refreshErr := ensureCredential(credential, true)
				if refreshErr != nil {
					lease.Release()
					lastErr = refreshErr
					lastFailure = newCredentialUpstreamFailure(refreshErr, credential.ID, credential.Name)
					continue attemptLoop
				}
				response, err = forwardResponse(lease, refreshed, lease.Billing)
				credential = refreshed
				if err != nil {
					lease.Release()
					lastErr = err
					if ctx.Err() != nil || errors.Is(err, context.Canceled) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), err)}
						break attemptLoop
					}
					lastFailure = newTransportUpstreamFailure(err, credential.ID, credential.Name)
					if !isRetryableTransportFailure(credential.Provider, err) {
						break attemptLoop
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break attemptLoop
					}
					continue attemptLoop
				}
				goto handleResponse
			}
			failureHandled := false
			if lease.QuotaMode != "" && response.StatusCode == http.StatusTooManyRequests {
				state, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
				s.applyRateLimitReconciliation(ctx, credential, response.StatusCode, retryAfter, state, reconcileErr)
				failureHandled = reconcileErr == nil && state == accountapp.RateLimitReconcileExhausted
			} else if used, limit, exhausted := parseFreeQuotaExhaustion(body); exhausted {
				// The Free subscription signal is account-scoped, but its billing
				// period is not a reliable reset promise. Probe again after 24 hours.
				s.selector.MarkFreeQuotaExhausted(ctx, credential, used, limit)
				failureHandled = true
			} else if lastFailure.ModelQuotaExhausted {
				s.selector.MarkModelQuotaExhausted(ctx, credential, lease.Billing, route.UpstreamModel, retryAfter)
				failureHandled = true
			} else if lastFailure.FreeQuotaExhausted {
				s.selector.MarkFreeQuotaExhausted(ctx, credential, 0, 0)
				failureHandled = true
			} else if lastFailure.SpendingLimitBlocked || lastFailure.QuotaExhausted {
				err := s.selector.MarkPaymentQuotaExhausted(ctx, credential, quotaRecoveryHints{Billing: lease.Billing})
				failureHandled = err == nil
				if err != nil {
					s.logger.Error("account_quota_recovery_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
				}
			}
			if lastFailure.AccountBlocked {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s account is blocked", credential.Provider))
			} else if buildForbiddenReauth {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s upstream error code %s matched the invalidation policy", credential.Provider, lastFailure.UpstreamCode))
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.PermanentAccountDenial {
				if credential.Provider == accountdomain.ProviderBuild {
					// 默认 model-scoped，视频拒绝时配额/OAuth 仍可能可用。
					// 开启 markBuildChatDeniedAsReauth 时再额外标 reauth，便于号池摘除。
					// 同时写入模型 block，避免在候选缓存窗口内本请求再次选中。
					modelErr := s.selector.MarkModelAccessDenied(ctx, credential, route.UpstreamModel, retryAfter)
					failureHandled = modelErr == nil
					if modelErr != nil {
						s.logger.Error("account_model_access_denied_write_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "model", route.UpstreamModel, "error", modelErr)
					}
					if s.markBuildChatDeniedAsReauth.Load() {
						reauthHandled := s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
						failureHandled = failureHandled || reauthHandled
					}
				} else {
					failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s chat endpoint access denied", credential.Provider))
				}
			} else if s.providers.SupportsCredentialRefresh(credential.Provider) && lastFailure.CredentialRejected {
				failureHandled = s.markReauthRequired(ctx, input.RequestID, credential, fmt.Sprintf("%s credential rejected", credential.Provider))
			}
			if lastFailure.AccountScoped && !failureHandled {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			} else if !lastFailure.AccountScoped && response.StatusCode >= http.StatusInternalServerError {
				// Provider 级 5xx:本请求换号,跨请求短暂隔离该账号,但不累积持久
				// 失败计数(#999 防瞬态 5xx 级联成 exponential 冷却)。保留真实状态码
				// 用于诊断,应用显式软失败策略。
				if markErr := s.selector.markSoftFailure(ctx, credential, response.StatusCode, retryAfter); markErr != nil {
					s.logger.Warn("soft_failure_mark_failed", "account_id", credential.ID, "status", response.StatusCode, "error", markErr.Error())
				}
			}
			lease.Release()
			lastErr = fmt.Errorf("上游返回 %d", response.StatusCode)
			s.logger.Warn("upstream_request_failed", "request_id", input.RequestID, "account_id", credential.ID, "provider", credential.Provider, "status", response.StatusCode, "upstream_code", lastFailure.UpstreamCode, "account_scoped", lastFailure.AccountScoped)
			if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
				break
			}
			continue
		}
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			s.selector.markSuccess(ctx, credential, lease.QuotaProbe)
			// 注：曾在此处记录上游响应头全量用于降智早期信号研究；2026-08-21
			// 直连矩阵证实 clean/降智头部完全一致（零判别力），已移除该噪声日志。
			if qualityHoldEnabled {
				proto := qualityProtocolForOperation(operation)
				var replay io.ReadCloser
				var verdict QualityVerdict
				var peekUsage Usage
				var peekErr error
				if input.Streaming {
					replay, verdict, peekUsage, _, peekErr = peekQualityStream(ctx, response.Body, proto, holdCfg)
				} else {
					// 非流式：完整 body 判决（零扣留延迟），证据规则与流式一致。
					replay, verdict, peekUsage, _, peekErr = peekQualityBody(response.Body, holdCfg)
				}
				response.Body = replay
				// Real-time guard observability: per-attempt withhold decision.
				s.logger.Info("quality_hold_verdict", "request_id", input.RequestID, "account_id", credential.ID, "protocol", proto, "streaming", input.Streaming, "verdict", string(verdict), "usage_output", peekUsage.OutputTokens, "usage_reasoning", peekUsage.ReasoningTokens, "peek_err", peekErr)
				if peekErr != nil {
					if replay != nil {
						_ = replay.Close()
					} else {
						_ = response.Body.Close()
					}
					lease.Release()
					lastErr = peekErr
					if isClientRequestCancel(ctx, peekErr) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), peekErr)}
						break
					}
					lastFailure = newTransportUpstreamFailure(peekErr, credential.ID, credential.Name)
					if neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) || neterrorpkg.IsUpstreamStreamIdleTimeout(context.Cause(ctx)) || errors.Is(peekErr, errQualityEmptyStream) || errors.Is(peekErr, errQualityEvidenceTimeout) || errors.Is(peekErr, errQualityCreatedTimeout) {
						// 守卫空闲路径的尝试进审计明细（round 41：此前多账号轮换
						// 轨迹在 attempts 里不可见；对照 quality_hold 路径有明细）。
						failureAttempts.captureQualityIdle(credential, responseStartedAt, lastFailure.Code)
						logPrefix := "quality_peek_idle"
						if errors.Is(peekErr, errQualityEmptyStream) {
							logPrefix = "quality_peek_empty"
						}
						writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
						if markErr := s.selector.MarkQualityIdleFailure(writeCtx, credential, holdCfg.IdleAccountCooldown); markErr != nil {
							s.logger.Warn(logPrefix+"_cooldown_failed", "request_id", input.RequestID, "account_id", credential.ID, "error", markErr)
						} else {
							s.logger.Warn(logPrefix+"_retry", "request_id", input.RequestID, "account_id", credential.ID, "cooldown", holdCfg.IdleAccountCooldown)
						}
						writeCancel()
						// 空流/空闲超时通常与出口 IP 相关而非账号本身。走与扣留路径相同的 RSC 归因：clean
						// 结论自动解除上面的冷却（IP 嫌疑），denied 结论打风控标记。没有这一步，无辜账号
						// 只能干等 24h，且同号重试恰好转为空流时会丢失归因。
						idleNodeID := markDegradedEgress()
						if observer := s.egressDegradationObserver(); observer != nil && idleNodeID != 0 {
							observer.OnEgressDegraded(ctx, idleNodeID, credential.ID)
						}
						if attributor := s.accountRiskAttributor(); attributor != nil {
							attributor.OnDegraded(ctx, credential, idleNodeID)
						}
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break
					}
					continue
				}
				response.Body = replay
				hasNextAccount := attemptPolicy.hasNext(attempt) && selection.hasAvailableCandidate(excluded, !quotaProbeAttempted)
				hasNextAccount = hasNextAccount && qualityAccountAttempts < holdCfg.MaxAttempts
				// 路由 attempt 预算已尽时不得承诺同号重试：承诺会强置 hasNext 并
				// 跳过惩罚与归因，但循环退出后重试根本不会发生（D 审查）。
				// quota-probe 来源用会话原始分类判断：转正会翻转 lease 标志，
				// 但该账号仍在 probeCandidates 队列，RetryAccount 找不到它
				// （外部复核 2：转正后排除失效会静默换号+虚假日志）。
				probeOrigin := lease.QuotaProbe || selection.wasQuotaProbeCandidate(credential.ID)
				// 同号重试仅对流式有意义：流式重试在数秒内重新拿到证据/输出判决。
				// 非流式的重试要重跑整个生成（复杂提示词降智实测 75-146s/次），
				// 对账号级/出口级降智的期望收益为负——直接惩罚换号。
				sameAccountEligible := input.Streaming && verdict == QualityWithhold && attemptPolicy.hasNext(attempt) && commitableSameAccountRetry(holdCfg, sameAccountRetried, probeOrigin, selection)
				if sameAccountEligible {
					// The un-excluded account itself becomes the next candidate.
					hasNextAccount = true
				}
				commit := CommitQualityHold(verdict, qualityAccountAttempts-1, holdCfg.MaxAttempts, hasNextAccount, holdCfg.OnExhausted)
				if verdict == QualityWithhold {
					if sameAccountEligible && commit.Action == QualityActionRetry {
						sameAccountRetried = true
						delete(excluded, credential.ID)
						selection.RetryAccount(credential.ID)
						s.logger.Info("quality_degraded_same_account_retry", "request_id", input.RequestID, "account_id", credential.ID, "quality_attempt", qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
					} else {
						s.applyMissingThinkingPenalty(ctx, input.RequestID, credential, holdCfg.AccountCooldown)
						reportEgressDegradation(credential)
						if attributor := s.accountRiskAttributor(); attributor != nil {
							attributor.OnDegraded(ctx, credential, degradedEgressNodeID(egressTrace, route.Provider))
						}
					}
				}
				deferFailOpenAudit := commit.Action == QualityActionRetry && holdCfg.OnExhausted == qualityRetryFailOpen
				if commit.Audit && !deferFailOpenAudit {
					s.recordQualityDegraded(ctx, auditBase, credential, peekUsage, startedAt, egressTrace, route.Provider)
					failureAttempts.captureQualityDegraded(credential, responseStartedAt)
				}
				switch commit.Action {
				case QualityActionRetry:
					if deferFailOpenAudit {
						discardFallback(true)
						fallback = &qualityFallback{response: response, lease: lease, credential: credential, usage: peekUsage, upstreamStartedAt: responseStartedAt}
						lease.completeSelectorObservation(true)
						lease.Release()
					} else {
						_ = response.Body.Close()
						lease.Release()
					}
					lastErr = errQualityDegraded
					lastFailure = &UpstreamFailure{
						HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
						PublicMessage: "上游响应缺少推理", AccountID: credential.ID, AccountName: credential.Name,
						Cause: errQualityDegraded,
					}
					s.logger.Info("quality_degraded_retry", "request_id", input.RequestID, "account_id", credential.ID, "quality_attempt", qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
					continue
				case QualityActionReject:
					_ = response.Body.Close()
					lease.Release()
					lastErr = errQualityDegraded
					lastFailure = &UpstreamFailure{
						HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
						PublicMessage: "上游响应缺少推理", AccountID: credential.ID, AccountName: credential.Name,
						Cause: errQualityDegraded,
					}
					s.logger.Info("quality_degraded_rejected", "request_id", input.RequestID, "account_id", credential.ID)
					break attemptLoop
				case QualityActionDeliverLast:
					discardFallback(true)
					s.logger.Info("quality_degraded_deliver_last", "request_id", input.RequestID, "account_id", credential.ID, "quality_attempt", qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
				case QualityActionDeliver:
					discardFallback(true)
				}
				if !commit.KeepBody {
					_ = response.Body.Close()
					lease.Release()
					break attemptLoop
				}
			}
			if diagnostic := response.RecoveredPrimaryFailure; diagnostic != nil {
				recoveredFailure := newHTTPUpstreamFailure(diagnostic.StatusCode, diagnostic.Body, credential.ID, credential.Name)
				if recoveredFailure.AccountBlocked || (credential.Provider == accountdomain.ProviderBuild && s.shouldInvalidateBuildForbidden(recoveredFailure)) {
					reason := fmt.Sprintf("%s primary endpoint denied account access", credential.Provider)
					if !s.markReauthRequired(ctx, input.RequestID, credential, reason) {
						s.selector.MarkModelAccessDenied(ctx, credential, route.UpstreamModel, 0)
					}
				}
			}
		}
		if fallback != nil && holdCfg.OnExhausted == qualityRetryFailOpen {
			_ = response.Body.Close()
			lease.completeSelectorObservation(false)
			lease.Release()
			selected := fallback
			fallback = nil
			s.logger.Info("quality_degraded_fallback", "request_id", input.RequestID, "account_id", selected.credential.ID, "quality_attempts", qualityAccountAttempts)
			return handoffResponse(selected.response, selected.lease, selected.credential, selected.upstreamStartedAt, true), nil
		}
		return handoffResponse(response, lease, credential, responseStartedAt, false), nil
	}
	if fallback != nil {
		if ctx.Err() == nil && holdCfg.OnExhausted == qualityRetryFailOpen {
			selected := fallback
			fallback = nil
			s.logger.Info("quality_degraded_fallback", "request_id", input.RequestID, "account_id", selected.credential.ID, "quality_attempts", qualityAccountAttempts)
			return handoffResponse(selected.response, selected.lease, selected.credential, selected.upstreamStartedAt, true), nil
		}
		discardFallback(true)
	}
	if lastFailure != nil {
		record := auditBase
		record.StatusCode = lastFailure.HTTPStatus
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.ErrorCode = lastFailure.AuditCode()
		record.Attempts = failureAttempts.snapshot()
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, egressTrace, route.Provider)
		if lastFailure.AccountID != 0 {
			accountID := lastFailure.AccountID
			record.AccountID = &accountID
			record.AccountName = lastFailure.AccountName
		}
		persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
		defer cancel()
		if err := s.audits.Create(persistCtx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		return nil, lastFailure
	}
	if lastErr == nil {
		lastErr = ErrNoAvailableAccount
	}
	record := auditBase
	record.StatusCode = http.StatusServiceUnavailable
	record.DurationMS = time.Since(startedAt).Milliseconds()
	record.ErrorCode = "upstream_unavailable"
	var selectionFailure *SelectionUnavailableError
	if errors.As(lastErr, &selectionFailure) {
		record.StatusCode = selectionFailure.HTTPStatus()
		record.ErrorCode = selectionFailure.Code()
	}
	record.Attempts = failureAttempts.snapshot()
	record.CreatedAt = time.Now().UTC()
	applyAuditEgress(&record, egressTrace, route.Provider)
	persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(persistCtx, record); err != nil {
		s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
	}
	return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastErr)
}

func isUpstreamStreamFailure(errorCode string) bool {
	switch errorCode {
	case "upstream_stream_incomplete", "upstream_stream_interrupted", "upstream_stream_idle_timeout":
		return true
	default:
		return false
	}
}

func streamFailureHealthPenalty(errorCode string, usage Usage, idleCooldown time.Duration) (int, time.Duration) {
	if idleCooldown <= 0 {
		idleCooldown = qualityIdleAccountCooldown
	}
	if errorCode == "upstream_stream_idle_timeout" && !usage.OutputObserved && usage.OutputTokens == 0 && usage.ReasoningTokens == 0 {
		return http.StatusGatewayTimeout, idleCooldown
	}
	return 0, 0
}

// auditRequestSucceeded keeps transport truth (the HTTP status) separate from
// the terminal request outcome. A stream that fails after 2xx headers is not a
// successful request even though its HTTP status remains 2xx.
func auditRequestSucceeded(statusCode int, errorCode string) bool {
	return statusCode >= 200 && statusCode < 300 && errorCode == ""
}

func isRetryableTransportFailure(providerValue accountdomain.Provider, err error) bool {
	return providerValue != accountdomain.ProviderBuild || !neterrorpkg.IsResponseHeaderTimeout(err)
}

func isSSOCredentialRejected(err error, credential accountdomain.Credential) bool {
	if credential.AuthType != accountdomain.AuthTypeSSO || err == nil {
		return false
	}
	if errors.Is(err, provider.ErrUnauthorized) {
		return true
	}
	status, ok := provider.ErrorHTTPStatus(err)
	return ok && status == http.StatusUnauthorized
}

func (s *Service) markSSOCredentialRejected(ctx context.Context, credential accountdomain.Credential, reason string) {
	if credential.AuthType != accountdomain.AuthTypeSSO {
		return
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.ID, reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
	}
	// Discard the process-local one-second candidate snapshot even if persistence fails,
	// preventing the invalid account from being selected by the next request.
	s.selector.MarkQuotaStateChanged(credential.Provider)
}

func (s *Service) queueAccountModelSync(accountID uint64) {
	syncer, ok := s.models.(accountModelSyncer)
	if !ok || accountID == 0 {
		return
	}
	s.modelSyncMu.Lock()
	if s.modelSyncing == nil {
		s.modelSyncing = make(map[uint64]struct{})
	}
	if _, exists := s.modelSyncing[accountID]; exists {
		s.modelSyncMu.Unlock()
		return
	}
	s.modelSyncing[accountID] = struct{}{}
	s.modelSyncMu.Unlock()

	go func() {
		defer func() {
			s.modelSyncMu.Lock()
			delete(s.modelSyncing, accountID)
			s.modelSyncMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), modelCatalogRefreshTimeout)
		defer cancel()
		logger := s.logger
		if logger == nil {
			logger = slog.Default()
		}
		count, err := syncer.SyncAccount(ctx, accountID)
		if err != nil {
			logger.Warn("model_etag_refresh_failed", "account_id", accountID, "error", err)
			return
		}
		logger.Info("model_etag_refresh_completed", "account_id", accountID, "models", count)
	}()
}

func rewriteAliasedModel(body []byte, publicModel, reasoningEffort string, operation audit.Operation) ([]byte, error) {
	// UseNumber 保精度:map[string]any 默认把数字解码为 float64, >2^53 的整数
	// 字段(seed/id/token)会被静默改成不精确值;json.Number 原样回写。
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload map[string]any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析兼容模型请求: %w", err)
	}
	payload["model"] = publicModel
	if reasoningEffort != "" {
		switch operation {
		case audit.OperationChat:
			payload["reasoning_effort"] = reasoningEffort
		case audit.OperationMessages:
			config, _ := payload["output_config"].(map[string]any)
			if reasoningEffort == modeldomain.ReasoningEffortNone {
				if config != nil {
					delete(config, "effort")
				}
				if len(config) == 0 {
					delete(payload, "output_config")
				} else {
					payload["output_config"] = config
				}
				payload["thinking"] = map[string]any{"type": "disabled"}
				break
			}
			if config == nil {
				config = make(map[string]any)
			}
			config["effort"] = reasoningEffort
			payload["output_config"] = config
			payload["thinking"] = map[string]any{"type": "adaptive"}
		default:
			reasoning, _ := payload["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = make(map[string]any)
			}
			reasoning["effort"] = reasoningEffort
			payload["reasoning"] = reasoning
		}
	}
	return json.Marshal(payload)
}

type ResourceInput struct {
	ClientKey  clientkey.Key
	ResponseID string
	RawQuery   string
}

func (s *Service) cancelBillingReservation(eventID string) {
	ctx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
	defer cancel()
	if err := s.clientKeys.CancelBilling(ctx, eventID); err != nil {
		s.logger.Error("billing_reservation_cancel_failed", "event_id", eventID, "error", err)
	}
}

func newAuditEventID() string {
	value, err := security.NewOpaqueToken(18)
	if err != nil || value == "" {
		return fmt.Sprintf("evt_%d", time.Now().UnixNano())
	}
	return "evt_" + value
}

func (s *Service) GetResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodGet)
}

func (s *Service) DeleteResponse(ctx context.Context, input ResourceInput) (*Result, error) {
	return s.forwardOwnedResponse(ctx, input, http.MethodDelete)
}

func (s *Service) forwardOwnedResponse(ctx context.Context, input ResourceInput, method string) (*Result, error) {
	ownership, err := s.responses.Get(ctx, input.ResponseID, input.ClientKey.ID, time.Now().UTC())
	if err != nil {
		return nil, ErrResponseNotFound
	}
	if !s.providers.SupportsStoredResponses(ownership.Provider) {
		_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		return nil, ErrResponseNotFound
	}
	accountScope := input.ClientKey.AccountScope()
	if !accountScope.AllowsProvider(ownership.Provider) {
		return nil, &SelectionUnavailableError{Reason: SelectionNoAccounts, Scope: accountScope}
	}
	adapter, ok := s.providers.Responses(ownership.Provider)
	if !ok {
		return nil, ErrResponseAccountUnavailable
	}
	operation := "response_get"
	if method == http.MethodDelete {
		operation = "response_delete"
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(ownership.Provider), operation)
	lease, err := s.selector.AcquirePinnedForKey(ctx, ownership.Provider, ownership.AccountID, 0, "", "", false, accountScope)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	credential, err := s.accounts.EnsureCredential(ctx, lease.Credential, false)
	if err != nil {
		lease.Release()
		return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, err)
	}
	path := "/responses/" + url.PathEscape(input.ResponseID)
	if input.RawQuery != "" {
		path += "?" + input.RawQuery
	}
	response, err := adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: credential, Method: method, Path: path})
	if err != nil {
		if isSSOCredentialRejected(err, credential) {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
		}
		lease.Release()
		return nil, err
	}
	if response.StatusCode == http.StatusUnauthorized {
		response.Body.Close()
		if credential.AuthType == accountdomain.AuthTypeSSO {
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			lease.Release()
			return nil, ErrResponseAccountUnavailable
		}
		if s.markPermanentlyUnrefreshableCredentialRejected(ctx, credential) {
			lease.Release()
			return nil, fmt.Errorf("%w: %w", ErrResponseAccountUnavailable, accountapp.ErrCredentialRefreshPermanent)
		}
		refreshed, refreshErr := s.accounts.EnsureCredential(ctx, credential, true)
		if refreshErr != nil {
			if errors.Is(refreshErr, accountapp.ErrCredentialRefreshPermanent) {
				s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
			}
			lease.Release()
			return nil, refreshErr
		}
		response, err = adapter.ForwardResponse(physicalCallCtx, provider.ResponseResourceRequest{Credential: refreshed, Method: method, Path: path})
		credential = refreshed
		if err != nil {
			lease.Release()
			return nil, err
		}
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		s.selector.markSuccess(ctx, credential, false)
		if method == http.MethodDelete {
			_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		}
	} else if response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone {
		// 上游已删除该 stored response：本地 ownership 失效同步删除，且不得把
		// 上游原文 Header+Body 透传给客户端（same leak class as the ≥400 branch
		// below——2026-08-21 补漏：此前该分支直接把原文流回客户端）。读空并
		// 关闭 body 供分类审计，客户端统一收到受控的 response_not_found 信封。
		_, _ = readRetryableBody(response.Body)
		_ = response.Body.Close()
		lease.Release()
		_ = s.responses.Delete(ctx, input.ResponseID, input.ClientKey.ID)
		return nil, ErrResponseNotFound
	} else if response.StatusCode >= 400 {
		// Non-2xx stored-response fetches (400/422...) must not hand the raw
		// upstream body and headers to the client (same leak class as the
		// chat-path fix); surface a controlled failure instead.
		body, _ := readRetryableBody(response.Body)
		_ = response.Body.Close()
		lease.Release()
		return nil, newHTTPUpstreamFailure(response.StatusCode, body, credential.ID, credential.Name)
	}
	var once sync.Once
	release := func() { once.Do(lease.Release) }
	finalize := func(Usage, string, string) { release() }
	return &Result{StatusCode: response.StatusCode, Status: response.Status, Header: response.Header, Body: &finalizingBody{ReadCloser: response.Body, finalize: release}, Finalize: finalize}, nil
}

// markPermanentlyUnrefreshableCredentialRejected removes an account from the pool after a real upstream request confirms its access token is invalid.
func (s *Service) markPermanentlyUnrefreshableCredentialRejected(ctx context.Context, credential accountdomain.Credential) bool {
	if !credential.RefreshPermanent {
		return false
	}
	s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
	return true
}

func (s *Service) markCredentialRejectedAfterPermanentRefresh(ctx context.Context, credential accountdomain.Credential) {
	_ = s.accounts.MarkReauthRequired(ctx, credential.ID, fmt.Sprintf("%s OAuth access token rejected after permanent refresh failure", credential.Provider))
	s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
}

func readRetryableBody(body io.ReadCloser) ([]byte, error) {
	if body == nil {
		return nil, nil
	}
	defer func() { _ = body.Close() }()
	data, _, err := provider.ReadDiagnosticBody(body)
	return data, err
}

func parseFreeQuotaExhaustion(body []byte) (int64, int64, bool) {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, "subscription:free-usage-exhausted") {
		return 0, 0, false
	}
	matches := freeQuotaUsagePattern.FindSubmatch(body)
	if len(matches) != 3 {
		return 0, 0, true
	}
	used, usedErr := strconv.ParseInt(string(matches[1]), 10, 64)
	limit, limitErr := strconv.ParseInt(string(matches[2]), 10, 64)
	if usedErr != nil || limitErr != nil {
		return 0, 0, true
	}
	return used, limit, true
}

type finalizingBody struct {
	io.ReadCloser
	finalize func()
}

func (b *finalizingBody) Close() error {
	err := b.ReadCloser.Close()
	if b.finalize != nil {
		b.finalize()
	}
	return err
}

// shouldStopForNonAccountFingerprint 仅对非账号归因故障累计指纹并在达到阈值后停止换号。
// 账号级失败（额度、鉴权、冷却等）继续轮询其它凭证。
// 未知 403、Team 模型限流只跳过当前号，不累计指纹、不提前结束整次请求。
func shouldStopForNonAccountFingerprint(fingerprints map[string]int, failure *UpstreamFailure) bool {
	if failure == nil || failure.AccountScoped || failure.Fingerprint == "" {
		return false
	}
	if failure.HTTPStatus == http.StatusForbidden {
		return false
	}
	if failure.Fingerprint == "429:team_model_rate_limit" {
		return false
	}
	fingerprints[failure.Fingerprint]++
	limit := nonAccountFailureFingerprintLimit
	if failure.Code == "upstream_stream_idle_timeout" || failure.Fingerprint == "upstream_stream_idle_timeout" || failure.Code == "upstream_stream_empty" || failure.Fingerprint == "upstream_stream_empty" {
		limit = streamIdleFailureFingerprintLimit
	}
	return fingerprints[failure.Fingerprint] >= limit
}

func isRetryable(status int) bool {
	return status == 402 || status == 403 || status == 429 || status >= 500
}

func isRetryableResponse(response *provider.Response, upstreamProvider accountdomain.Provider) bool {
	if response == nil || !isRetryable(response.StatusCode) {
		return false
	}
	// Account-scoped payment failures must always rotate accounts.
	// Upstream X-Should-Retry:false is only honored for non-account errors (e.g. 5xx history).
	if forcesAccountFailover(response.StatusCode, upstreamProvider) {
		return true
	}
	return !strings.EqualFold(strings.TrimSpace(response.Header.Get("X-Should-Retry")), "false")
}

// isTerminalRequestForbidden identifies request-level 403 responses that must
// be returned without account or egress side effects. Unknown 403 responses,
// including bare permission-denied, remain on the credential traversal path.
// General request policy classification is Build-specific so Web and Console
// keep their browser/clearance recovery behavior. The exact Console DPoP rollout
// error is also terminal because changing account or egress cannot satisfy it.
func isTerminalRequestForbidden(upstreamProvider accountdomain.Provider, failure *UpstreamFailure) bool {
	if failure == nil {
		return false
	}
	return failure.SafetyRejection ||
		(upstreamProvider == accountdomain.ProviderBuild && failure.RequestScopedForbidden) ||
		(upstreamProvider == accountdomain.ProviderConsole && failure.RequestScopedForbidden && isDPoPProofRequired(failure.UpstreamCode))
}

// forcesAccountFailover keeps Build account-scoped billing, permission, and rate-limit
// failures on the account-rotation path so their state can be recorded before another
// account is selected. free-usage 429 and Team RPS 429 both need rotation even when
// upstream sets X-Should-Retry:false.
func forcesAccountFailover(status int, upstreamProvider accountdomain.Provider) bool {
	return upstreamProvider == accountdomain.ProviderBuild &&
		(status == http.StatusPaymentRequired || status == http.StatusForbidden || status == http.StatusTooManyRequests)
}

// applyRateLimitReconciliation 把配额对账结果映射为选择器动作(#1003 Console
// 429 对账):exhausted 才落 durable 失败;Console 429 的非 exhausted 态
// (available/refreshing/inconclusive)是瞬态,按 Retry-After 软隔离即可,
// 不增大失败计数(防止瞬态 429 级联成长期冷却)。
func (s *Service) applyRateLimitReconciliation(ctx context.Context, credential accountdomain.Credential, status int, retryAfter time.Duration, state accountapp.RateLimitReconcileState, reconcileErr error) {
	s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
	if reconcileErr == nil && state == accountapp.RateLimitReconcileExhausted {
		return
	}
	if credential.Provider == accountdomain.ProviderConsole && status == http.StatusTooManyRequests {
		// A Console 429 with available quota, an in-progress cross-instance probe,
		// or an inconclusive /usage request is transient. Isolate the account for
		// this Retry-After window without growing its durable failure count.
		if err := s.selector.markSoftFailure(ctx, credential, status, retryAfter); err != nil {
			s.logger.Warn("console_rate_limit_soft_cooldown_failed", "account_id", credential.ID, "state", state, "error", err)
		}
		return
	}
	s.selector.MarkFailure(ctx, credential, status, retryAfter)
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if parsed, err := http.ParseTime(value); err == nil && parsed.After(now) {
		return parsed.Sub(now)
	}
	return 0
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return errors.New("未知上游错误")
}
