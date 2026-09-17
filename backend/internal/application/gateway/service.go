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
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	portcrypto "github.com/chenyme/grok2api/backend/internal/port/crypto"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
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
// RateLimitInterpreter 把上游 429 事实解释为 RateLimitMetadata;
// 方言级文本/正则解析在 Provider 边界,经组合根注入。
type RateLimitInterpreter interface {
	ParseRateLimitMetadata(body []byte) *provider.RateLimitMetadata
}

// keyBillingAuthorizer 是逻辑执行所需的客户端 Key 能力：模型授权检查、
// 密钥读取、费用预留与取消。管理、创建与撤销不在执行合同内。
type keyBillingAuthorizer interface {
	CanUseModel(value clientkey.Key, modelID uint64) bool
	Get(ctx context.Context, id uint64) (clientkey.Key, error)
	ReserveBilling(ctx context.Context, key clientkey.Key, eventID string, amount int64, ttl time.Duration) (bool, error)
	CancelBilling(ctx context.Context, eventID string) error
}

type Service struct {
	identities           historyapp.IdentityResolver
	models               routeResolver
	audits               auditRecorder
	accounts             accountapp.Execution
	clientKeys           keyBillingAuthorizer
	providers            provider.Registry
	selector             *selector.Selector
	responses            responseHistory
	tokens               portcrypto.TokenSource
	physicalJournals     portphysical.JournalFactory
	rateLimits           RateLimitInterpreter
	maxAttempts          atomic.Int64
	videoMaxAttempts     atomic.Int64
	buildForbiddenReauth atomic.Pointer[buildForbiddenReauthPolicy]
	requestTimeout       atomic.Int64
	mediaJobs            JobExecutionStore
	videoResources       *mediaapp.VideoResources
	mediaAssets          videoAssetStore
	// background 持有媒体后台执行的全部可变状态（队列/去重集合/worker/
	// 输入槽位/额度恢复游标）；本服务只经组件方法访问。
	background                  *mediaBackground
	logger                      *slog.Logger
	markBuildChatDeniedAsReauth atomic.Bool
	guardSource                 atomic.Pointer[guardSnapshotSource]
	// qualityEvents 是权威质量回执落盘点(必须持久成功);与可丢弃的
	// 观测遥测不同,这里的失败要按 hold 处理。
	qualityEvents atomic.Pointer[qualityEventRecorder]
	// nodeExitIPResolver 出口 IP 取证面(批6 第4步:调查局差分探针的
	// 重摇 IP 验证,I8);nil=重摇差分一律不可采。
	nodeExitIPResolver atomic.Pointer[nodeExitIPResolverValue]
}

// SetAccountQualityEligibility forwards the account eligibility seam onto
// the routing selector (composition-root convenience; nil is ignored).
func (s *Service) SetAccountQualityEligibility(eligibility selector.AccountEligibility) {
	s.selector.SetQualityEligibility(eligibility)
}

type buildForbiddenReauthPolicy struct {
	enabled bool
	codes   map[string]struct{}
}

func (s *Service) ConfigureMedia(store JobExecutionStore, resources *mediaapp.VideoResources, concurrency int) {
	s.mediaJobs = store
	if resources != nil {
		// Preserves the historical wiring where assets set before media
		// configuration participate in the resource bundle.
		if s.mediaAssets != nil {
			resources = resources.WithAssetReader(s.mediaAssets)
		}
		s.videoResources = resources
	}
	logger := s.logger
	if logger == nil {
		logger = slog.Default()
	}
	s.background = newMediaBackground(concurrency, logger, logger)
	s.background.SetProcessor(func(ctx context.Context, id string) { s.processVideoJob(ctx, id) })
}

// ConfigureMediaAssets injects optional local video asset archival and reading.
// 生产组合根不注入(资源读取经 videoResources 装配);本入口供集成测试
// 替换资产存储,是显式测试缝。
func (s *Service) ConfigureMediaAssets(store videoAssetStore) {
	s.mediaAssets = store
	if s.videoResources != nil {
		s.videoResources = s.videoResources.WithAssetReader(store)
	}
}

func NewService(models routeResolver, audits auditRecorder, accounts accountapp.Execution, clientKeys keyBillingAuthorizer, providers provider.Registry, selector *selector.Selector, responses *historyapp.ResponseResources, tokens portcrypto.TokenSource, journals portphysical.JournalFactory, rateLimits RateLimitInterpreter, maxAttempts int) *Service {
	if journals == nil {
		panic("gateway: physical journal factory 不能为空")
	}
	service := &Service{
		models: models, audits: audits, accounts: accounts, clientKeys: clientKeys, providers: providers,
		selector: selector, responses: responses, tokens: tokens, physicalJournals: journals, rateLimits: rateLimits, logger: slog.Default(),
	}
	service.UpdateMaxAttempts(maxAttempts)
	return service
}

// UpdateBuildForbiddenReauthPolicy atomically replaces the Build account invalidation policy.
// parseRateLimit 经注入的解释器读取 429 元数据;未装配时保留包级规则
// (测试/剥离形态行为不变)。
func (s *Service) parseRateLimit(body []byte) *provider.RateLimitMetadata {
	if s.rateLimits != nil {
		return s.rateLimits.ParseRateLimitMetadata(body)
	}
	return provider.ParseRateLimitMetadata(body)
}

// startPhysicalTrace installs a fresh execution-owned journal for one logical
// execution; the context only carries the accounting contract. The factory is
// injected by the composition root and NewService refuses a nil one, so there
// is exactly one wiring path and no gateway-local default ledger.
func (s *Service) startPhysicalTrace(ctx context.Context, provider, operation string) context.Context {
	return portphysical.WithPhysicalCallTrace(ctx, s.physicalJournals.NewPhysicalJournal(), provider, operation)
}

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
		return nil, fallback, &selector.SelectionUnavailableError{Reason: selector.SelectionNoAccounts, Scope: accountScope}
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
		return nil, fallback, &selector.SelectionUnavailableError{Reason: selector.SelectionNoAccounts, Scope: accountScope}
	}
	if !allowed {
		return nil, fallback, clientkeyapp.ErrModelNotAllowed
	}
	return nil, fallback, ErrNoAvailableAccount
}

// selectSchedulableMediaRoute resolves a concrete same-name media target and
// its immutable account plan together. A cooling or exhausted first target
// therefore cannot hide a healthy target from another Provider.
func (s *Service) selectSchedulableMediaRoute(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool) (modeldomain.Route, *selector.SelectionSession, error) {
	return s.selectSchedulableMediaRouteWithQuotaMode(ctx, routes, key, capability, consumesQuota, providerSupported, nil)
}

func (s *Service) selectSchedulableMediaRouteWithQuotaMode(ctx context.Context, routes []modeldomain.Route, key clientkey.Key, capability modeldomain.Capability, consumesQuota bool, providerSupported func(accountdomain.Provider) bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selector.SelectionSession, error) {
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
func (s *Service) selectSchedulableEligibleMediaRouteWithQuotaMode(ctx context.Context, eligible []modeldomain.Route, key clientkey.Key, consumesQuota bool, resolveQuotaMode func(modeldomain.Route) string) (modeldomain.Route, *selector.SelectionSession, error) {
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
		session, selectionErr := s.selector.BeginSelectionSessionForKey(
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

func (s *Service) newAuditEventID() string {
	// Test-constructed services may omit the token source; the timestamp
	// fallback below preserves the original package-level behavior.
	if s.tokens != nil {
		value, err := s.tokens.NewOpaqueToken(18)
		if err == nil && value != "" {
			return "evt_" + value
		}
	}
	return fmt.Sprintf("evt_%d", time.Now().UnixNano())
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
		if err := s.selector.MarkSoftFailure(ctx, credential, status, retryAfter); err != nil {
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
