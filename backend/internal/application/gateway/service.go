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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/retryafter"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

var (
	ErrModelNotFound              = errors.New("模型不存在或未启用")
	ErrNoAvailableAccount         = errors.New("没有可用上游账号")
	ErrResponseNotFound           = historydomain.ErrResponseNotFound
	ErrResponseAccountUnavailable = errors.New("Response 绑定的上游账号不可用")
	ErrResponseStateUnsupported   = errors.New("目标模型不支持有状态 Response")
	ErrConversationUnsupported    = errors.New("目标模型不支持当前对话协议")
	ErrVideoInputTooLarge         = mediadomain.ErrVideoInputTooLarge
	ErrVideoInputUnavailable      = mediadomain.ErrVideoInputUnavailable
	ErrVideoParameterInvalid      = errors.New("视频请求参数无效")
	ErrVideoOperationUnsupported  = errors.New("视频编辑/延长仅支持路由到 Console grok-imagine-video")
	ErrLedgerUnavailable          = errors.New("计费账本暂不可用")
)

const finalizationTimeout = 5 * time.Second
const minimumTextBillingReservationTTL = 2 * time.Hour
const billingReservationCrashGrace = 10 * time.Minute
const mediaBillingReservationTTL = 24 * time.Hour
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
// 首事件/零证据截止(quality_created_timeout/quality_evidence_timeout)与
// 流空闲同族——service.go 的 idle 处理路径对五者一视同仁(短冷却+RSC 归因),
// 指纹阈值也必须一致:排队期超时是 provider/出口段级现象,换号无法补偿,
// 不封顶会把 CreatedTimeout 预算乘遍整个凭证池(线上实测 6 次 ×10s ≈ 66s
// 死寂才 fail-closed)。
const streamIdleFailureFingerprintLimit = 2

var freeQuotaUsagePattern = regexp.MustCompile(`(?i)tokens\s*\(actual/limit\)\s*:\s*([0-9]+)\s*/\s*([0-9]+)`)

type Input struct {
	RequestID          string
	ClientKey          clientkey.Key
	PublicModel        string
	Body               []byte
	Streaming          bool
	PromptCacheKey     string
	SessionSignals     historydomain.ClientSignals
	PreviousResponseID string
	// StoreResponse is the client's response-resource preference. Model support
	// and this preference jointly determine whether ownership must be committed.
	StoreResponse *bool
	// HistoryRecoveryPolicy overrides the default compatibility policy for this logical request.
	HistoryRecoveryPolicy *historydomain.RecoveryMode
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
	OutputObserved            bool
	InputTokens               int64
	CachedInputTokens         int64
	CachedInputTokensReported bool
	OutputTokens              int64
	ReasoningTokens           int64
	TotalTokens               int64
	CostInUSDTicks            int64
	NumSourcesUsed            int64
	NumServerSideToolsUsed    int64
	ContextInputTokens        int64
	ContextOutputTokens       int64
	ResponseModel             string
}

type Result struct {
	StatusCode int
	Status     string
	Header     http.Header
	Body       io.ReadCloser
	// BeginDelivery transfers finalization ownership to the caller, which must
	// call Finalize and Close even when preflight or admission fails. Cancellation
	// interrupts the body and releases the lease without erasing caller metadata.
	BeginDelivery func() error
	// CommitDelivery must succeed before the transport publishes response headers.
	CommitDelivery func() error
	// CommitCompletion validates required durable writes after clean protocol
	// completion and before publishing success. Finalize never substitutes for it.
	CommitCompletion    func(Completion) error
	MarkFirstToken      func()
	RecordStreamFailure func(StreamFailureDiagnostic)
	// RecordDelivery 由 transport 层在响应体转发完成后调用一次，把实际
	// 交付到客户端的事件/字节统计传回审计（轮26：回答「200 且带错误码
	// 时实际交付了多少」）。nil 时统计不记录（非推理路径）。
	RecordDelivery func(DeliveryStats)
	Finalize       func(usage Usage, responseID, errorCode string)
}

// Completion contains validated protocol facts before server success delivery.
// NativeResponseID is observed before transport compatibility fills missing IDs;
// it cannot be inferred from a generated client-visible placeholder.
type Completion struct {
	Usage            Usage
	ResponseID       string
	NativeResponseID string
}

// DeliveryStats 是转发到客户端的交付统计：流式为 SSE data 事件数与累计
// 写出字节；非流式为响应体字节数（Events=1）。
type DeliveryStats struct {
	StatusCode int
	Events     int64
	Bytes      int64
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
	QueueAccountSync(accountID uint64) bool
}

// Service handles model routing, account selection, failover, and audit finalization.
type Service struct {
	identities                  historyapp.IdentityResolver
	models                      routeResolver
	audits                      auditRecorder
	accounts                    *accountapp.Service
	clientKeys                  *clientkeyapp.Service
	providers                   *provider.Registry
	selector                    *Selector
	responses                   responseHistory
	maxAttempts                 atomic.Int64
	videoMaxAttempts            atomic.Int64
	buildForbiddenReauth        atomic.Pointer[buildForbiddenReauthPolicy]
	requestTimeout              atomic.Int64
	mediaJobs                   repository.MediaJobRepository
	videoResources              *mediaapp.VideoResources
	mediaAssets                 videoAssetStore
	mediaQueue                  chan string
	mediaMu                     sync.Mutex
	mediaQuotaRecoveryMu        sync.Mutex
	mediaQuotaRecoveryCursor    string
	mediaQueued                 map[string]struct{}
	mediaWorker                 int
	mediaInputSlots             chan struct{}
	mediaQueueFull              atomic.Uint64
	logger                      *slog.Logger
	markBuildChatDeniedAsReauth atomic.Bool
	qualityRetry                atomic.Pointer[QualityRetryRuntime]
	guardSource                 atomic.Pointer[guardSnapshotSource]
	// qualityObserver 流观察点缝隙(D3-3a):守卫判决旁路进新证据局;
	// nil disables. RecordQualityObservation 必须非阻塞(I19)。
	qualityObserver atomic.Value // QualityObserver
	qualityEvents   atomic.Pointer[qualityEventRecorder]
	// qualityRetryPolicy 重试原语策略缝隙(D3-3b);nil=内建策略。
	qualityRetryPolicy atomic.Value // QualityRetryPolicy
	// nodeExitIPResolver 出口 IP 取证面(批6 第4步:调查局差分探针的
	// 重摇 IP 验证,I8);nil=重摇差分一律不可采。
	nodeExitIPResolver atomic.Pointer[nodeExitIPResolverValue]
	// modelJurisdiction 管辖判定缝隙(G13):质量层守卫勾选清单;
	// nil=沿用文件白名单 requestRetry.guardedModels。
	modelJurisdiction atomic.Value // QualityJurisdiction
}

// SetModelJurisdiction 安装管辖判定缝隙(G13);nil 保持未设。
func (s *Service) SetModelJurisdiction(jurisdiction QualityJurisdiction) {
	if jurisdiction == nil {
		return
	}
	s.modelJurisdiction.Store(jurisdiction)
}

func (s *Service) modelJurisdictionObserver() QualityJurisdiction {
	if value, ok := s.modelJurisdiction.Load().(QualityJurisdiction); ok {
		return value
	}
	return nil
}

// SetQualityObserver installs the quality-layer stream-observation seam
// (D3-3a). Nil is ignored (seam stays off).
func (s *Service) SetQualityObserver(observer QualityObserver) {
	if observer == nil {
		return
	}
	s.qualityObserver.Store(observer)
}

func (s *Service) qualityObservationObserver() QualityObserver {
	if value, ok := s.qualityObserver.Load().(QualityObserver); ok {
		return value
	}
	return nil
}

// SetQualityRetryPolicy installs the quality-layer retry-policy seam
// (D3-3b). Nil is ignored: the base keeps its built-in policy.
func (s *Service) SetQualityRetryPolicy(policy QualityRetryPolicy) {
	if policy == nil {
		return
	}
	s.qualityRetryPolicy.Store(policy)
}

func (s *Service) qualityPolicyObserver() QualityRetryPolicy {
	if value, ok := s.qualityRetryPolicy.Load().(QualityRetryPolicy); ok {
		return value
	}
	return nil
}

// SetAccountQualityEligibility forwards the account eligibility seam onto
// the routing selector (composition-root convenience; nil is ignored).
func (s *Service) SetAccountQualityEligibility(eligibility AccountEligibility) {
	s.selector.SetQualityEligibility(eligibility)
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
	s.videoResources = mediaapp.NewVideoResources(repository, s.mediaAssets)
	s.mediaWorker = concurrency
	s.mediaQueue = make(chan string, min(2048, max(64, concurrency*32)))
	s.mediaInputSlots = make(chan struct{}, min(concurrency, videoInputMaterializeConcurrency))
	s.mediaQueued = make(map[string]struct{})
}

// ConfigureMediaAssets injects optional local video asset archival and reading.
func (s *Service) ConfigureMediaAssets(store videoAssetStore) {
	s.mediaAssets = store
	s.videoResources = mediaapp.NewVideoResources(s.mediaJobs, store)
}

func NewService(models routeResolver, audits auditRecorder, accounts *accountapp.Service, clientKeys *clientkeyapp.Service, providers *provider.Registry, selector *Selector, responses repository.ResponseRepository, maxAttempts int) *Service {
	service := &Service{
		models: models, audits: audits, accounts: accounts, clientKeys: clientKeys, providers: providers,
		selector: selector, responses: historyapp.NewResponseResources(responses), logger: slog.Default(),
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
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.CredentialRef(), reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "request_id", requestID, "account_id", credential.ID, "provider", credential.Provider, "error", err)
		return false
	}
	s.selector.MarkQuotaStateChanged(credential.Provider)
	return true
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

// distinguishMissingOrNoAccount 在候选查询返回 ErrNotFound 后区分「模型
// 不存在」（透传 ErrNotFound → 调用者映射 404）与「路由已启用但 Provider
// 当前无可用账号」（ErrNoAvailableAccount → 503 upstream_unavailable，
// 可重试）。availableRoutePredicate 要求路由绑有启用账号，无账号
// Provider 的路由在候选查询里整体消失——没有这一步，无账号 Provider 上
// 的模型会被误报成不存在（实测：grok-4.20-0309-reasoning 无
// console 账号时返回 404；同日对抗审查发现 effort 别名出口
// grok-4.3-low 同样漏判）。所有候选为空的失败出口都必须经过这里。
func (s *Service) distinguishMissingOrNoAccount(ctx context.Context, publicModel string, err error) error {
	var unavailable *repository.ModelRouteUnavailableError
	if errors.As(err, &unavailable) {
		if unavailable.Unsupported {
			return modeldomain.ErrUnsupportedCapability
		}
		if unavailable.Enabled {
			return ErrNoAvailableAccount
		}
		return repository.ErrNotFound
	}
	if !errors.Is(err, repository.ErrNotFound) {
		return err
	}
	if exists, existsErr := s.models.HasEnabledRouteByPublicID(ctx, publicModel); existsErr == nil && exists {
		return ErrNoAvailableAccount
	}
	return err
}

func (s *Service) resolvePublicModelRoutes(ctx context.Context, publicModel string, allowModelAliases bool) ([]modeldomain.Route, string, error) {
	routes, effort, err := modelapp.ResolvePublicRoutes(ctx, s.models, s.providers, publicModel, allowModelAliases)
	if err != nil {
		return nil, "", s.distinguishMissingOrNoAccount(ctx, publicModel, err)
	}
	return routes, effort, nil
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
		if requireStoredResponse && !s.providers.SupportsStoredResponseModel(route.Provider, route.UpstreamModel) {
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

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (result *Result, resultErr error) {
	var recoverHistory *historyRecoveryState
	defer func() {
		var failure *UpstreamFailure
		if recoverHistory != nil && errors.As(resultErr, &failure) {
			failure.HistoryRecovery = recoverHistory.snapshot()
		}
	}()

	holdCfg, snapshotScope := s.requestGuardSnapshot()
	ctx, egressTrace := infraegress.WithTrace(ctx)
	startedAt := time.Now()
	// The admission deadline stays unarmed until jurisdiction and exemptions
	// are settled below: arming here would let model and candidate lookups
	// consume the budget of a request the guard later exempts. Arming later
	// still measures from request start, and client cancellation keeps
	// propagating through the parent context.
	admission := newAdmission(ctx, startedAt, 0)
	ctx = responsebuffer.WithContext(admission.ctx, responsebuffer.FromContext(ctx))
	defer func() {
		if err := admission.failure(); err != nil {
			if result != nil {
				_ = result.Body.Close()
				result = nil
			}
			resultErr = err
		}
		if result == nil {
			admission.close()
		}
	}()
	var firstToken *firstTokenTimer
	if input.Streaming {
		firstToken = newFirstTokenTimer(startedAt)
	}
	eventID := newAuditEventID()
	ctx = attemptmeta.WithRequest(ctx, eventID, holdCfg.Revision, holdCfg.RuleVersion, holdCfg.pathResolver)
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
		if s.providers.SupportsStoredResponseModel(initialRoute.Provider, initialRoute.UpstreamModel) {
			value, ownershipErr := s.responses.Lookup(ctx, input.PreviousResponseID, input.ClientKey.ID, time.Now().UTC())
			if ownershipErr != nil {
				return nil, ownershipErr
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
	// 消息锚点记忆器:路由排序、预选候选、选中身份三处共享一次全量解析
	//(128KB body 的锚点提取是毫秒级;别名模型重写只改 model/effort 字段,
	// 不触碰 instructions/system/messages,重写前后锚点等价)。
	identityRequest := historyapp.NewIdentityRequest(input.ClientKey.ID, input.SessionSignals, input.PromptCacheKey, input.RequestID, requestSessionScope, input.Body)
	eligibleRoutes, fallbackRoute, routeErr := s.eligibleConversationRoutes(routes, input.ClientKey, operation, path, ownership != nil, ownership)
	route := fallbackRoute
	orderedRoutes := eligibleRoutes
	if routeErr == nil {
		orderedRoutes = orderConversationRouteTargets(eligibleRoutes, identityRequest.RouteSeed())
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
				identity := s.identities.Resolve(identityRequest, historyapp.IdentityTarget{
					Provider: string(candidate.Provider), Model: candidate.UpstreamModel,
					IsolatedWithoutSession: modeldomain.IsGrokComposerModel(candidate.UpstreamModel),
				}, historyapp.Identity{})
				affinityKey = identity.AffinityKey
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
	priorReasoningReplayKey := ""
	if route.Provider == accountdomain.ProviderBuild {
		// Derive a stable identity from explicit session signals, message anchors,
		// and model. Composer replaces message-only fallback identities with an
		// isolated request identity that remains stable across retries.
		inherited := historyapp.Identity{}
		if ownership != nil && ownership.PromptCacheKey != "" {
			inherited.UpstreamID = ownership.PromptCacheKey
			inherited.ReplayKey = ownership.ReasoningReplayKey
		}
		identity := s.identities.Resolve(identityRequest, historyapp.IdentityTarget{
			Provider: string(route.Provider), Model: route.UpstreamModel,
			IsolatedWithoutSession: modeldomain.IsGrokComposerModel(route.UpstreamModel),
		}, inherited)
		input.PromptCacheKey = identity.UpstreamID
		affinityKey = identity.AffinityKey
		ownershipPromptCacheKey = identity.UpstreamID
		reasoningReplayKey = identity.ReplayKey
		priorReasoningReplayKey = identity.PriorReplayKey
		if identity.UpstreamID == "" {
			s.logger.Debug("prompt_cache_session_empty", "request_id", input.RequestID, "model", route.UpstreamModel, "provider", route.Provider)
		} else if identity.Soft {
			s.logger.Debug("prompt_cache_session_soft", "request_id", input.RequestID, "model", route.UpstreamModel)
		} else if identity.Isolated {
			s.logger.Debug("prompt_cache_session_isolated", "request_id", input.RequestID, "model", route.UpstreamModel)
		}
	}
	_, ok := s.providers.Responses(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	physicalCallCtx := infraegress.WithPhysicalCallTrace(ctx, string(route.Provider), string(operation))
	var handoffPhysicalID string
	defer func() {
		if err := s.recordPhysicalEvents(physicalCallCtx, handoffPhysicalID); err != nil {
			s.logger.Error("physical_attempt_events_failed", "request_id", input.RequestID, "error", err)
			if result != nil {
				_ = result.Body.Close()
				result = nil
			}
			resultErr = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "上游尝试记录暂时无法保存，请稍后重试", Cause: err}
		}
	}()

	// degradedNodes 收集本请求内被守卫判定降智的出口节点。注入
	// WithNodeExclusions 后,后续 attempt 会换到其他固定出口 IP;账号
	// 绑定不变,仅本请求绕开(G14:扣留后的重试必须换路)。空流/扣留/
	// 头预算三条降智路径共用。
	degradedNodes := make(map[uint64]struct{})
	markDegradedEgress := func(response *provider.Response) uint64 {
		nodeID := response.Attempt.Path.NodeID
		if nodeID == 0 {
			return 0
		}
		if _, exists := degradedNodes[nodeID]; !exists {
			degradedNodes[nodeID] = struct{}{}
			physicalCallCtx = infraegress.WithNodeExclusions(physicalCallCtx, degradedNodes)
		}
		return nodeID
	}
	supportsStoredResponses := s.providers.SupportsStoredResponseModel(route.Provider, route.UpstreamModel)
	if input.PreviousResponseID != "" && !supportsStoredResponses {
		return nil, ErrResponseStateUnsupported
	}
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	idempotencyID, _ := security.NewOpaqueToken(18)
	if ownership != nil {
		attemptPolicy = newRoutingAttemptPolicy(1)
	}
	physicalLimit := attemptPolicy.limit
	if route.Provider != accountdomain.ProviderBuild || attemptPolicy.unlimited || physicalLimit > infraegress.MaxPhysicalCalls {
		physicalLimit = infraegress.MaxPhysicalCalls
	}
	requestBudget := inferencedomain.NewAttemptBudget(physicalLimit)
	budgetHandedOff := false
	defer func() {
		if !budgetHandedOff {
			requestBudget.Close()
		}
	}()
	physicalCallCtx = infraegress.WithPhysicalCallBudget(physicalCallCtx, requestBudget)
	recoveryMode := historydomain.AllowLossyRecovery
	if input.HistoryRecoveryPolicy != nil {
		recoveryMode = *input.HistoryRecoveryPolicy
	}
	recoverHistory = newHistoryRecoveryState(recoveryMode, requestBudget)
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	reserved := false
	defer func() {
		if reserved && !timingHandedOff {
			s.cancelBillingReservation(eventID)
		}
	}()
	if reservation, priced := audit.EstimateOfficialTextReservation(pricingModel, input.Body); priced {
		if reserved, err = s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, reservation.CostInUSDTicks, s.textBillingReservationTTL()); err != nil {
			return nil, err
		}
	}
	excluded := make(map[uint64]bool)
	failureFingerprints := make(map[string]int)
	authRecoveryAttempted := make(map[uint64]bool)
	exemptReason := qualityHoldExemptReason(input, ownership, route, operation, holdCfg, snapshotScope)
	if holdCfg.unavailable != nil {
		return nil, &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_guard_unavailable", PublicMessage: "响应守卫暂不可用", Cause: holdCfg.unavailable}
	}
	qualityHoldEnabled := exemptReason == ""
	if qualityHoldEnabled && operation == audit.OperationChat {
		if choices, ok := jsonpeek.RootIntFieldScan(input.Body, "n"); ok && choices > 1 {
			return nil, &UpstreamFailure{HTTPStatus: http.StatusBadRequest, Code: "unsupported_choices", PublicMessage: "当前响应守卫仅支持 n=1", Cause: errQualityChoices}
		}
	}
	// 活跃度预算制度表先用客户端请求体预计算一次(128KB body 的 tools/effort
	// 全量探测实测 1.16ms/次,原实现在循环内每次质量尝试重复支付)。adapter
	// 归一化回调会在上游调用前用规范化轮廓覆盖它:web_search_options、
	// Messages thinking 和 max 等别名只在归一化后才成为工具或 high/xhigh。
	peekSchedule := holdCfg
	// Real-time guard observability. Gate lines are emitted only when the hold
	// is engaged: a per-request INFO line while the feature is off would be pure
	// log amplification proportional to traffic.
	if qualityHoldEnabled {
		peekSchedule = qualityLivenessSchedule(input.Body, string(operation), holdCfg)
		// Arm the deadline only after jurisdiction and exemptions are settled.
		// The budget measures from request start, so governed requests keep the
		// same absolute deadline; exempt requests never carry one.
		admission.setBudget(peekSchedule.AdmissionTimeout)
		s.logger.Info("quality_hold_gate", "request_id", input.RequestID, "provider", route.Provider, "public_model", input.PublicModel, "upstream_model", route.UpstreamModel, "operation", operation)
	} else {
		admission.disable()
		// 豁免留痕：每条路径放行多少请求进 guard-stats（exempts 计数），
		// 不再重演"连续多发裸奔却无任何痕迹可查"。
		// 豁免 token 同步落审计主行（QualityExempt）：守卫不在场的交付，
		// 事后从审计列表即可回答"为何没拦"，不必交叉日志与计数器复原。
		guardStats.recordExempt(exemptReason)
		auditBase.QualityExempt = exemptReason
	}
	// Count accounts that actually reached the upstream. Credential-only skips
	// do not consume the quality retry budget; refreshes stay on the same account.
	qualityAccountAttempts := 0
	replaySafety := inferencedomain.ReplayPolicyFromRequest(input.Body)
	physicalStarted := false
	// firstGuardSignal 记录本请求首个触发的守卫特征(请求级结局归因:
	// 该特征触发的请求最终被救回还是失败——量化每个规则对降智的拦截价值)。
	firstGuardSignal := GuardSignal("")
	noteGuardSignal := func(signal GuardSignal) {
		guardStats.recordSignal(signal)
		if firstGuardSignal == "" {
			firstGuardSignal = signal
			guardStats.recordRequestSignal(signal)
		}
	}
	finishGuardOutcome := func(rescued bool) {
		if firstGuardSignal != "" {
			guardStats.recordOutcome(firstGuardSignal, rescued)
		}
	}
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaProbeAttempted := false
	selection := preselectedSession
	var lastErr error
	var lastFailure *UpstreamFailure
	failureAttempts := newFailureAttemptRecorder(http.MethodPost, path)
	normalizedMetadata := &provider.NormalizedRequestMetadata{}
	responseStartedAt := startedAt
	var pendingOutput *provider.Response
	var imageFacts *imageGeneration
	var textFacts *textGeneration
	if route.Capability != modeldomain.CapabilityImage {
		textFacts = newTextGeneration(physicalCallCtx, usageSource, pricingModel)
	}
	discardPendingOutput := func() {
		if pendingOutput != nil && pendingOutput.DiscardOutput != nil {
			pendingOutput.DiscardOutput()
		}
		pendingOutput = nil
	}
	defer discardPendingOutput()
	toolCompatibilityPolicy := inferencedomain.AllowDisabledCacheTools
	forwardResponse := func(lease *accountLease, credential accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
		discardPendingOutput()
		started := time.Now()
		responseStartedAt = started
		if physicalStarted && !replaySafety.Safe {
			return nil, &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "unsafe_replay_blocked",
				PublicMessage: "上游请求无法安全自动重放，请确认工具执行结果", Cause: errors.New(replaySafety.Reason)}
		}
		physicalStarted = true
		if route.Capability == modeldomain.CapabilityImage {
			imageFacts = &imageGeneration{}
		}
		textFacts.begin(credential, lease.QuotaMode, lease.QuotaSnapshotVersion)
		lease.markSelectorUpstreamStarted()
		request := provider.ResponseResourceRequest{Credential: credential, Billing: billing, Method: http.MethodPost, Path: path, Model: route.UpstreamModel, PromptCacheKey: input.PromptCacheKey, ReasoningReplayKey: reasoningReplayKey, PriorReasoningReplayKey: priorReasoningReplayKey, ToolCompatibilityPolicy: toolCompatibilityPolicy, GrokTurnIndex: input.GrokTurnIndex, IdempotencyID: idempotencyID, Body: input.Body, Streaming: input.Streaming, NormalizeBody: true, Operation: string(operation), NormalizedMetadata: normalizedMetadata, DeferOutputCommit: true, DisableAutomaticReplay: !replaySafety.Safe, HistoryControl: recoverHistory,
			OnNormalized: func(metadata provider.NormalizedRequestMetadata) error {
				if metadata.ImageOutputCount > 0 && !reserved {
					if price, priced := audit.EstimateOfficialImageCost(pricingModel, "", "", metadata.ImageOutputCount); priced {
						var err error
						reserved, err = s.clientKeys.ReserveBilling(ctx, input.ClientKey, eventID, price.CostInUSDTicks, mediaBillingReservationTTL)
						if err != nil {
							return err
						}
					}
				}

				if metadata.ToolCompatibility != nil {
					if err := metadata.ToolCompatibility.Validate(toolCompatibilityPolicy); err != nil {
						return err
					}
				}
				if qualityHoldEnabled && metadata.ReplayPolicy != nil {
					// The normalized profile is the actual upstream shape: reschedule
					// total admission and both liveness phases from it.
					peekSchedule = qualityLivenessScheduleForProfile(metadata.ReplayPolicy.Tools, metadata.ReasoningEffort, holdCfg)
					admission.setBudget(peekSchedule.AdmissionTimeout)
				}
				return admission.failure()
			},
		}
		if imageFacts != nil {
			request.ObserveImage = imageFacts.observe
		}
		attemptCtx, resources := newAttemptResources(physicalCallCtx)
		lease.replaceResources(resources)
		attemptCtx = attemptmeta.WithAccount(attemptCtx, credential.ID, string(route.Provider), route.UpstreamModel)
		response, err := s.runPhysicalAttempt(attemptCtx, request, resources)
		recoverHistory.annotate(response)
		textFacts.accept(response, supportsStoredResponses && operation == audit.OperationResponses)
		if normalizedMetadata.ReplayPolicy != nil && !normalizedMetadata.ReplayPolicy.Safe {
			replaySafety = *normalizedMetadata.ReplayPolicy
		}
		auditBase.ReasoningEffort = normalizedMetadata.ReasoningEffort
		err = failureAttempts.captureResponse(credential, started, response, err)
		if imageFacts != nil {
			facts, _ := imageFacts.snapshot()
			if facts.OutputImages > 0 && err != nil {
				if response != nil && response.Body != nil {
					_ = response.Body.Close()
				}
				response = imageCompatibilityFailure(err)
				err = nil
			}
		}
		if response != nil {
			response.Body = resources.own(response.Body)
		}
		pendingOutput = response
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
	handoffResponse := func(response *provider.Response, lease *accountLease, credential accountdomain.Credential, upstreamStartedAt time.Time) *Result {
		handoffPhysicalID = response.Attempt.ID
		if pendingOutput == response {
			pendingOutput = nil
		}
		session := &deliverySession{
			service: s, ctx: ctx, physicalCtx: physicalCallCtx, response: response, credential: credential, lease: lease, admission: admission,
			physicalBudget: requestBudget, imageFacts: imageFacts, textFacts: textFacts,
			firstToken: firstToken, timing: timing, attempts: failureAttempts, egressTrace: egressTrace, finishGuardOutcome: finishGuardOutcome,
			plan: deliveryPlan{route: route, operation: operation, guard: holdCfg, guardEnabled: qualityHoldEnabled, audit: auditBase,
				usageSource: usageSource, pricingModel: pricingModel, storeResponse: supportsStoredResponses && historydomain.ResponseStorageRequested(input.StoreResponse),
				promptCacheKey: ownershipPromptCacheKey, reasoningReplayKey: reasoningReplayKey,
				requestID: input.RequestID, clientKeyID: input.ClientKey.ID, streaming: input.Streaming, startedAt: startedAt},
		}
		timingHandedOff = true
		budgetHandedOff = true
		return session.result(upstreamStartedAt)
	}
	var lease *accountLease
	defer func() {
		if !budgetHandedOff {
			lease.Release()
		}
	}()
attemptLoop:
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if requestBudget.Remaining() == 0 {
			if lastFailure == nil {
				lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "physical_attempt_limit", PublicMessage: "上游尝试次数达到安全上限", Cause: inferencedomain.ErrAttemptBudget}
			}
			break
		}

		if err := s.recordPhysicalEvents(physicalCallCtx); err != nil {
			lastErr = err
			lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "上游尝试记录暂时无法保存，请稍后重试", Cause: err}
			break
		}

		if qualityHoldEnabled {
			if err := s.checkQualityEventCapacity(ctx); err != nil {
				lastErr = err
				lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", Cause: err}
				break
			}
		}
		if physicalStarted && !replaySafety.Safe {
			break
		}
		if err := admission.failure(); err != nil {
			lastErr = err
			break
		}
		if qualityHoldEnabled && qualityAccountAttempts >= holdCfg.MaxAttempts {
			break
		}
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
		if limited, ok := s.accounts.ActiveTeamModelRateLimit(lease.Credential, route.UpstreamModel, time.Now().UTC()); ok {
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
			promoted, recovered, probeErr := s.accounts.ProbePaidQuota(ctx, lease.Credential, *lease.QuotaRecoveryRef)
			s.selector.MarkQuotaStateChanged(lease.Credential.Provider, lease.Credential.ID)
			if probeErr != nil || !recovered {
				lease.Release()
				lastErr = firstError(probeErr, fmt.Errorf("付费额度尚未恢复"))
				continue
			}
			lease.Credential = promoted
			lease.QuotaRecoveryRef = nil
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
		if lease.QuotaRecoveryRef != nil {
			credential.QuotaRecoveryRevision = lease.QuotaRecoveryRef.Revision
		}
		if qualityHoldEnabled {
			qualityAccountAttempts++
		}
		response, err := forwardResponse(lease, credential, lease.Billing)
		if err != nil {
			if errors.Is(err, clientkeyapp.ErrBillingLimit) || errors.Is(err, clientkeyapp.ErrRuntimeUnavailable) {
				lease.skipSelectorObservation()
				lease.Release()
				return nil, err
			}
			lease.Release()
			lastErr = err
			if errors.Is(err, infraegress.ErrPhysicalCallLimit) {
				lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "physical_attempt_limit", PublicMessage: "上游尝试次数达到安全上限", Cause: err}
				break
			}
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
		if imageFacts != nil {
			facts, _ := imageFacts.snapshot()
			if facts.OutputImages > 0 && response.StatusCode >= 400 {
				return handoffResponse(response, lease, credential, responseStartedAt), nil
			}
		}
		if response.RequestValidation != nil {
			_ = response.Body.Close()
			lease.skipSelectorObservation()
			lease.Release()
			record := auditBase
			record.StatusCode = http.StatusBadRequest
			record.ErrorCode = response.RequestValidation.Code
			record.DurationMS = time.Since(startedAt).Milliseconds()
			record.CreatedAt = time.Now().UTC()
			record.Attempts = failureAttempts.snapshot()
			s.finishUnhandedText(&record, textFacts, physicalCallCtx, qualityHoldEnabled)
			persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
			defer cancel()
			if err := s.audits.Create(persistCtx, record); err != nil {
				s.logger.Error("request_validation_audit_write_failed", "event_id", record.EventID, "error", err)
			}
			finishGuardOutcome(false)
			return nil, response.RequestValidation
		}
		if response.ModelCatalogChanged {
			if syncer, ok := s.models.(accountModelSyncer); ok {
				syncer.QueueAccountSync(credential.ID)
			}
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
			retryAfter := retryafter.Header(response.Header.Get("Retry-After"), time.Now().UTC())
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
			retryAfter := retryafter.Header(response.Header.Get("Retry-After"), time.Now().UTC())
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
				limited, known := s.accounts.ObserveTeamModelRateLimit(credential, route.UpstreamModel, rateLimitMeta, time.Now().UTC())
				if !known {
					// Without a team identity, retain account-scoped 429 handling.
					goto afterTeamRateLimit
				}
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
			// No built-in provider currently reaches this branch (only Build
			// declares credential refresh and is excluded above); it is kept for
			// future refreshable non-Build providers.
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
			// Definitive account-block signals only arise from 401/403, and both
			// statuses are fully handled (with continue) before this generic retry
			// path, so no blocked response can reach here.
			if buildForbiddenReauth {
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
			credential = s.selector.markSuccess(ctx, credential, lease.QuotaRecoveryRef)
			// 注：曾在此处记录上游响应头全量用于降智早期信号研究；
			// 直连矩阵证实 clean/降智头部完全一致（零判别力），已移除该噪声日志。
			if qualityHoldEnabled {
				// ConvertStream/ConvertJSON mean the body is still native
				// Responses; peek that shape before client conversion.
				proto := qualityPeekProtocol(operation, response)
				var replay io.ReadCloser
				var verdict QualityVerdict
				var peekUsage Usage
				var peekErr error
				var peekFingerprint qualityHoldFingerprint
				// 活跃度预算制度表（qualityLivenessSchedule）：按请求类（搜索工具/
				// 重推理/默认）给 created/evidence 预算——证据与依据见该函数注释。
				// 流式/非流式共用：预算只对流式生效；完整 body 判决与流式共用
				// 同一套证据规则。
				peekCfg := peekSchedule
				// 思考期望来自 resolved effort（含别名档位）：终态纯语义输出
				// （裸工具调用）在期望思考时按 missing-thinking 扣留，堵住
				// 语义放行出口的降智交付。
				peekCfg.ReasoningExpected = reasoningExpectedForEffort(normalizedMetadata.ReasoningEffort)
				if input.Streaming {
					replay, verdict, peekUsage, peekFingerprint, peekErr = peekQualityStreamReport(ctx, response.Body, proto, peekCfg)
				} else {
					// 非流式：完整 body 判决（零扣留延迟），证据规则与流式一致。
					replay, verdict, peekUsage, peekFingerprint, peekErr = peekQualityBodyReportWithBudget(response.Body, peekCfg, responsebuffer.FromContext(ctx))
				}
				response.Body = lease.ownBody(replay)
				if data, release, ok := responsebuffer.Borrow(response.Body); ok {
					textFacts.observeJSON(response, data)
					release()
				}
				if input.Streaming {
					textFacts.observeStream(response, peekUsage, peekFingerprint.Completed, peekFingerprint.Failed)
				}
				if verdict == QualityWithhold {
					textFacts.withhold()
				}
				// Real-time guard observability: per-attempt withhold decision.
				// 日志中的 usage 是判决时刻快照而非终值（规则 1 早交付先于 usage
				// 帧到达）。
				s.logger.Info("quality_hold_verdict", "request_id", input.RequestID, "account_id", credential.ID, "protocol", proto, "streaming", input.Streaming, "verdict", string(verdict), "rule", peekFingerprint.Rule, "first_item", peekFingerprint.FirstItem, "has_thinking", peekFingerprint.HasThinking, "encrypted", peekFingerprint.Encrypted, "usage_output", peekUsage.OutputTokens, "usage_reasoning", peekUsage.ReasoningTokens, "peek_err", peekErr)

				if peekErr == nil {
					observed := QualityObservedAdmitted
					if verdict == QualityWithhold {
						observed = QualityObservedDegraded
						if response.Body != nil {
							_ = response.Body.Close()
						}
						lease.Release()
					}
					obs := QualityObservation{Attempt: response.Attempt, At: time.Now().UTC(),
						AccountID: credential.ID, NodeID: response.Attempt.Path.NodeID, Provider: string(route.Provider),
						Outcome: observed, Rule: peekFingerprint.Rule}
					if eventErr := s.recordQualityEvent(ctx, obs, holdCfg.AccountCooldown); eventErr != nil {
						if response.Body != nil {
							_ = response.Body.Close()
						}
						lease.Release()
						lastErr = eventErr
						lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable",
							PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", AccountID: credential.ID, AccountName: credential.Name, Cause: eventErr}
						break attemptLoop
					}

				}

				if peekErr != nil {
					if replay != nil {
						_ = replay.Close()
					} else {
						_ = response.Body.Close()
					}
					lease.Release()
					lastErr = peekErr
					errorCode := qualityHoldRule(QualityStreamSignals{}, peekErr)
					if isClientRequestCancel(ctx, peekErr) {
						errorCode = "request_canceled"
					}
					if eventErr := s.recordQualityEvent(ctx, QualityObservation{Attempt: response.Attempt, At: time.Now().UTC(), AccountID: credential.ID, NodeID: response.Attempt.Path.NodeID, Provider: string(route.Provider), Outcome: QualityObservedRejected, Rule: peekFingerprint.Rule, ErrorCode: errorCode}, 0); eventErr != nil {
						lastErr = eventErr
						lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", Cause: eventErr}
						break attemptLoop
					}
					if errors.Is(peekErr, errQualityChoices) {
						lastFailure = &UpstreamFailure{HTTPStatus: http.StatusBadGateway, Code: "unsupported_upstream_choices", PublicMessage: "上游返回了不支持的响应选项", AccountID: credential.ID, AccountName: credential.Name, Cause: peekErr}
						break attemptLoop
					}
					if errors.Is(peekErr, responsebuffer.ErrExhausted) {
						lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "response_resource_exhausted", PublicMessage: "响应处理容量暂时不足，请稍后重试", AccountID: credential.ID, AccountName: credential.Name, Cause: peekErr}
						break attemptLoop
					}
					if isClientRequestCancel(ctx, peekErr) {
						lastFailure = &UpstreamFailure{HTTPStatus: 499, Code: "request_canceled", PublicMessage: "请求已取消", AccountID: credential.ID, AccountName: credential.Name, Cause: firstError(ctx.Err(), peekErr)}
						break
					}
					lastFailure = newTransportUpstreamFailure(peekErr, credential.ID, credential.Name)
					switch {
					case errors.Is(peekErr, errQualityCreatedTimeout):
						noteGuardSignal(GuardSignalCreatedTimeout)
					case errors.Is(peekErr, errQualityEvidenceTimeout):
						noteGuardSignal(GuardSignalEvidenceTimeout)
					case errors.Is(peekErr, errQualityEmptyStream):
						noteGuardSignal(GuardSignalEmptyStream)
					}

					if neterrorpkg.IsUpstreamStreamIdleTimeout(peekErr) || neterrorpkg.IsUpstreamStreamIdleTimeout(context.Cause(ctx)) || errors.Is(peekErr, errQualityEmptyStream) || errors.Is(peekErr, errQualityEvidenceTimeout) || errors.Is(peekErr, errQualityCreatedTimeout) {
						// 守卫空闲路径的尝试进审计明细（round 41：此前多账号轮换
						// 轨迹在 attempts 里不可见；对照 quality_hold 路径有明细）。
						failureAttempts.captureQualityIdle(credential, responseStartedAt, lastFailure.Code, response, peekFingerprint)
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
						// 空闲路径同样把出口节点加入本请求的排除集(G14:扣留后的
						// 重试必须换路);账号侧由 MarkQualityIdleFailure 短冷却承担。
						markDegradedEgress(response)
					}
					if shouldStopForNonAccountFingerprint(failureFingerprints, lastFailure) {
						break
					}
					continue
				}
				response.Body = replay
				hasNextAccount := verdict == QualityWithhold && attemptPolicy.hasNext(attempt) && selection.hasAvailableCandidate(excluded, !quotaProbeAttempted)
				hasNextAccount = hasNextAccount && replaySafety.Safe && qualityAccountAttempts < holdCfg.MaxAttempts
				commit := s.decideQualityCommit(verdict, qualityAccountAttempts-1, holdCfg.MaxAttempts, hasNextAccount, holdCfg.OnExhausted)
				if commit.KeepBody {
					// 交付尝试的判决规则落审计主行：rule=thinking 表示流内观察到
					// 可见思考增量；其余规则的 200 交付为 fail-open 形态（配合
					// QualityFailOpen），面板可据此识别"零思考交付"。
					auditBase.QualityRule = peekFingerprint.Rule

				}
				if verdict == QualityWithhold {
					noteGuardSignal(GuardSignalWithhold)
					// 出口节点加入本请求的排除集:后续重试换到别的路径。
					markDegradedEgress(response)
				}
				// 扣留只记在本请求的 attempt 明细上，不再 Create 第二条主审计。
				// 线上曾出现同一 requestId 两行 200：一行 quality_degraded
				// events=0，一行成功放行——列表看起来像「有的抓住有的漏」。
				if verdict == QualityWithhold {
					failureAttempts.captureQualityDegraded(credential, responseStartedAt, response, peekFingerprint)
				}
				switch commit.Action {
				case QualityActionRetry:
					_ = response.Body.Close()
					lease.Release()
					lastErr = errQualityDegraded
					lastFailure = &UpstreamFailure{
						HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
						PublicMessage: "上游模型暂不可用或缺少推理能力，请稍后重试", AccountID: credential.ID, AccountName: credential.Name,
						Cause: errQualityDegraded,
					}
					s.logger.Info("quality_degraded_retry", "request_id", input.RequestID, "account_id", credential.ID, "quality_attempt", qualityAccountAttempts, "output_tokens", peekUsage.OutputTokens)
					continue
				case QualityActionReject:
					guardStats.recordExhausted()
					// 耗尽拒绝的主行也带最终判决规则指纹:
					// 面板对 503 的归因不再需要逐条展开 attempt
					// 明细(attempt 级详情仍保留全量)。
					auditBase.QualityRule = peekFingerprint.Rule
					_ = response.Body.Close()
					lease.Release()
					lastErr = errQualityDegraded
					lastFailure = &UpstreamFailure{
						HTTPStatus: http.StatusServiceUnavailable, Code: ErrorQualityDegraded,
						PublicMessage: "上游模型暂不可用或缺少推理能力，请稍后重试", AccountID: credential.ID, AccountName: credential.Name,
						Cause: errQualityDegraded,
					}
					s.logger.Info("quality_degraded_rejected", "request_id", input.RequestID, "account_id", credential.ID)
					break attemptLoop
				case QualityActionDeliver:
				}
				if !commit.KeepBody {
					_ = response.Body.Close()
					lease.Release()
					break attemptLoop
				}
			}
			if err := prepareResponseDelivery(response, input.Streaming, textFacts); err != nil {
				if imageFacts != nil {
					facts, _ := imageFacts.snapshot()
					if facts.OutputImages > 0 {
						if response.Body != nil {
							_ = response.Body.Close()
						}
						return handoffResponse(imageCompatibilityFailure(err), lease, credential, responseStartedAt), nil
					}
				}
				if response.Body != nil {
					_ = response.Body.Close()
				}
				lease.Release()
				lastErr = err
				lastFailure = &UpstreamFailure{
					HTTPStatus: http.StatusBadGateway, Code: "response_conversion_failed",
					PublicMessage: "上游响应格式转换失败，请稍后重试", AccountID: credential.ID, AccountName: credential.Name,
					Cause: err,
				}
				if errors.Is(err, errResponseTerminalFailure) {
					lastFailure.Code = "upstream_response_incomplete"
					lastFailure.PublicMessage = "上游响应未成功完成，请稍后重试"
				}
				if errors.Is(err, responsecheck.ErrEmptyOutput) {
					lastFailure.Code = "upstream_empty_output"
					lastFailure.PublicMessage = "上游已结束但未返回答案或工具输出"
				}
				if errors.Is(err, responsebuffer.ErrExhausted) {
					lastFailure.HTTPStatus = http.StatusServiceUnavailable
					lastFailure.Code = "response_resource_exhausted"
					lastFailure.PublicMessage = "响应处理容量暂时不足，请稍后重试"
				}
				if isClientRequestCancel(ctx, err) {
					lastFailure.HTTPStatus, lastFailure.Code, lastFailure.PublicMessage = 499, "request_canceled", "请求已取消"
				}
				if qualityHoldEnabled {
					if eventErr := s.recordQualityEvent(ctx, QualityObservation{Attempt: response.Attempt, At: time.Now().UTC(),
						AccountID: credential.ID, NodeID: response.Attempt.Path.NodeID, Provider: string(route.Provider),
						Outcome: QualityObservedInterrupted, ErrorCode: lastFailure.Code}, 0); eventErr != nil {
						lastErr = eventErr
						lastFailure = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable",
							PublicMessage: "质量守卫事件暂时无法保存，请稍后重试", Cause: eventErr}
					}
				}
				// The complete JSON body has not reached the client. Retry only
				// replay-safe requests, within the existing physical attempt budget.
				if lastFailure.Code == "upstream_empty_output" {
					_ = failureAttempts.captureResponse(credential, responseStartedAt, response, err)
				}
				if lastFailure.Code == "upstream_empty_output" && replaySafety.Safe && attemptPolicy.hasNext(attempt) {
					markDegradedEgress(response)
					continue
				}
				break attemptLoop
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
		return handoffResponse(response, lease, credential, responseStartedAt), nil
	}
	if failure := admission.failure(); failure != nil {
		var upstream *UpstreamFailure
		if errors.As(failure, &upstream) {
			lastFailure = upstream
		}
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
		s.finishUnhandedText(&record, textFacts, physicalCallCtx, qualityHoldEnabled)
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
		defer cancel()
		if err := s.audits.Create(persistCtx, record); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
		}
		finishGuardOutcome(false)
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
	s.finishUnhandedText(&record, textFacts, physicalCallCtx, qualityHoldEnabled)
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), finalizationTimeout)
	defer cancel()
	if err := s.audits.Create(persistCtx, record); err != nil {
		s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", err)
	}
	finishGuardOutcome(false)
	return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastErr)
}

func isUpstreamStreamFailure(errorCode string) bool {
	switch errorCode {
	case "upstream_stream_incomplete", "upstream_stream_interrupted", "upstream_stream_idle_timeout", "upstream_output_loop":
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
	if provider.IsMediaPostProcessingError(err) || errors.Is(err, historydomain.ErrHistoryPrepare) || errors.Is(err, historydomain.ErrHistoryCommit) || errors.Is(err, responsebuffer.ErrExhausted) || errors.Is(err, responsebuffer.ErrLimit) {
		return false
	}
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
	if err := s.accounts.MarkReauthRequired(writeCtx, credential.CredentialRef(), reason); err != nil {
		s.logger.Error("account_reauth_required_write_failed", "account_id", credential.ID, "provider", credential.Provider, "error", err)
	}
	// Discard the process-local one-second candidate snapshot even if persistence fails,
	// preventing the invalid account from being selected by the next request.
	s.selector.MarkQuotaStateChanged(credential.Provider)
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

// markPermanentlyUnrefreshableCredentialRejected removes an account from the pool after a real upstream request confirms its access token is invalid.
func (s *Service) markPermanentlyUnrefreshableCredentialRejected(ctx context.Context, credential accountdomain.Credential) bool {
	if !credential.RefreshPermanent {
		return false
	}
	s.markCredentialRejectedAfterPermanentRefresh(ctx, credential)
	return true
}

func (s *Service) markCredentialRejectedAfterPermanentRefresh(ctx context.Context, credential accountdomain.Credential) {
	_ = s.accounts.MarkReauthRequired(ctx, credential.CredentialRef(), fmt.Sprintf("%s OAuth access token rejected after permanent refresh failure", credential.Provider))
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
	budget *responsebuffer.Budget
	io.ReadCloser
	finalize func()
	once     sync.Once
	err      error
}

func (b *finalizingBody) ResponseBudget() *responsebuffer.Budget {
	if b.budget != nil {
		return b.budget
	}
	return responsebuffer.BudgetOf(b.ReadCloser)
}

func (b *finalizingBody) BorrowBytes() ([]byte, func(), bool) {
	return responsebuffer.Borrow(b.ReadCloser)
}

func (b *finalizingBody) Close() error {
	b.once.Do(func() {
		b.err = b.ReadCloser.Close()
		if b.finalize != nil {
			b.finalize()
		}
	})
	return b.err
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
	if isStreamIdleClassFailure(failure.Code) || isStreamIdleClassFailure(failure.Fingerprint) {
		limit = streamIdleFailureFingerprintLimit
	}
	return fingerprints[failure.Fingerprint] >= limit
}

// isStreamIdleClassFailure 报告失败码是否属于"上游静默/排队"族:流空闲、
// 空流,以及实时守卫的首事件/零证据截止。族内失败与 idle 处理路径的分组
// (短冷却+RSC 归因)保持一致,共享 streamIdleFailureFingerprintLimit 封顶。
func isStreamIdleClassFailure(value string) bool {
	switch value {
	case "upstream_stream_idle_timeout", "upstream_stream_empty", "quality_created_timeout", "quality_evidence_timeout":
		return true
	}
	return false
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

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return errors.New("未知上游错误")
}
