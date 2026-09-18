package cli

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/buildtransport"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	"github.com/chenyme/grok2api/backend/internal/pkg/texts"
	"github.com/chenyme/grok2api/backend/internal/pkg/upstreamtrace"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/google/uuid"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	BaseURL               string
	FallbackBaseURL       string
	ClientVersion         string
	ClientIdentifier      string
	TokenAuth             string
	UserAgent             string
	ResponseHeaderTimeout time.Duration
	StreamIdleTimeout     time.Duration
}

const (
	subscriptionTierTimeout = 10 * time.Second
	buildControlTimeout     = 30 * time.Second
	buildGrok45Model        = "grok-4.5"
	buildGrok46Model        = "grok-4.6"
)

// Adapter implements the Grok Build CLI Responses, model, Billing, and OAuth protocols.
type Adapter struct {
	cfgMu                sync.RWMutex
	cfg                  Config
	http                 *http.Client
	oauth                *oauthClient
	cipher               security.Cryptor
	base                 *buildDirectTransport
	agentID              string
	modelsMu             sync.Mutex
	modelsETags          map[uint64]string
	fallbackMarker       FallbackMarker
	uploadIssuer         VideoUploadIssuer
	replay               historydomain.Service
	legacyReplayAccounts []uint64
	compaction           *historydomain.CompactionCodec
	turnsMu              sync.Mutex
	turns                map[string]grokTurnState
	logger               *slog.Logger
}

// grokTurnState 记录会话的官方客户端轮次计数(x-grok-turn-idx)。
type grokTurnState struct {
	index      uint64
	lastUsedAt time.Time
}

// maxTrackedGrokTurns 限制轮次表的内存上界;超限时按 lastUsedAt 淘汰最旧
// 的四分之一。会话中辍后计数自然作废(上游按会话维度使用该值),不需要
// 精确的过期回收。
const (
	maxTrackedGrokTurns = 8192
	grokTurnPruneBatch  = maxTrackedGrokTurns / 4
)

func NewAdapter(cfg Config, cipher security.Cryptor) *Adapter {
	cfg.ResponseHeaderTimeout = normalizeBuildResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	cfg.StreamIdleTimeout = normalizeBuildStreamIdleTimeout(cfg.StreamIdleTimeout)
	transport := newBuildDirectTransport(cfg.ResponseHeaderTimeout)
	httpClient := &http.Client{Transport: transport}
	// The official CLI uses a persistent machine identity. The gateway does not collect machine fingerprints;
	// instead each backend process generates one random UUID for its lifetime as the Agent identity.
	agentID := uuid.NewString()
	adapter := &Adapter{
		cfg: cfg, http: httpClient, cipher: cipher, base: transport,
		agentID: agentID, modelsETags: make(map[uint64]string), turns: make(map[string]grokTurnState), compaction: historydomain.NewCompactionCodec(cipher), logger: slog.Default(),
	}
	// 官方 CLI 的 shared_client 对包括 OAuth 在内的所有请求统一附加 grok-shell
	// User-Agent;网关的 OAuth 平面(client_id/token/device)必须一致,否则 Go 默认
	// 会宣告 Go-http-client/<version>。管理员清空 userAgent 设置时回退到推荐值,
	// 避免退回 Go 默认标识。
	adapter.oauth = newOAuthClient(httpClient,
		func() string { return adapter.config().ClientVersion },
		func() string {
			if userAgent := strings.TrimSpace(adapter.config().UserAgent); userAgent != "" {
				return userAgent
			}
			return provider.RecommendedBuildUserAgent
		},
	)
	return adapter
}

func (a *Adapter) SetLogger(logger *slog.Logger) {
	if logger != nil {
		a.logger = logger
	}
}

// SetEgress injects the network owner's dialer during construction. Quality
// decorators observe selected paths; route and transport decisions stay in M13.
func (a *Adapter) SetEgress(dialer infraegress.Dialer) {
	if dialer != nil {
		a.http.Transport = &egressTransport{manager: dialer, fallback: a.base}
	}
}

// SetReasoningReplay injects M12's history preparation and capture service.
func (a *Adapter) SetReasoningReplay(replay historydomain.Service) {
	a.replay = replay
}

func (a *Adapter) Provider() account.Provider { return account.ProviderBuild }

// CredentialMetadata extracts only non-sensitive risk flags from a Build access token.
// bot_flag_source or its short alias bfs must be JSON number 1 or 2; other values, malformed
// tokens, and decryption failures are not marked. bot_flag_source is preferred when both are set.
func (a *Adapter) CredentialMetadata(credential account.Credential) provider.CredentialMetadata {
	if credential.Provider != account.ProviderBuild || a.cipher == nil || credential.EncryptedAccessToken == "" {
		return provider.CredentialMetadata{}
	}
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return provider.CredentialMetadata{}
	}
	claims := decodeJWTClaims(accessToken)
	if claims == nil {
		return provider.CredentialMetadata{}
	}
	source := buildBotFlagSourceFromClaims(claims)
	return provider.CredentialMetadata{
		BuildBotFlagInspected: true,
		BuildBotFlagged:       source != 0,
		BuildBotFlagSource:    source,
	}
}

// buildBotFlagSourceFromClaims returns the bot-risk source from JWT claims.
// Accepts bot_flag_source or bfs; only JSON numbers 1 and 2 count (string "1"/"2" do not).
// Prefer bot_flag_source when it is 1 or 2; otherwise fall back to bfs.
func buildBotFlagSourceFromClaims(claims map[string]any) int {
	if claims == nil {
		return 0
	}
	if source := botFlagSourceClaim(claims, "bot_flag_source"); source != 0 {
		return source
	}
	return botFlagSourceClaim(claims, "bfs")
}

func botFlagSourceClaim(claims map[string]any, key string) int {
	value, ok := claims[key].(float64)
	if !ok {
		return 0
	}
	switch value {
	case 1, 2:
		return int(value)
	default:
		return 0
	}
}

func (a *Adapter) UpdateConfig(cfg Config) {
	cfg.ResponseHeaderTimeout = normalizeBuildResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	cfg.StreamIdleTimeout = normalizeBuildStreamIdleTimeout(cfg.StreamIdleTimeout)
	a.cfgMu.Lock()
	previousTimeout := a.cfg.ResponseHeaderTimeout
	a.cfg = cfg
	a.cfgMu.Unlock()
	if previousTimeout != cfg.ResponseHeaderTimeout && a.base != nil {
		a.base.UpdateResponseHeaderTimeout(cfg.ResponseHeaderTimeout)
	}
}

type buildDirectTransport struct {
	current atomic.Pointer[http.Transport]
}

func newBuildDirectTransport(responseHeaderTimeout time.Duration) *buildDirectTransport {
	value := &buildDirectTransport{}
	value.current.Store(newBuildHTTPTransport(responseHeaderTimeout))
	return value
}

func (t *buildDirectTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if attemptmeta.FromContext(request.Context()).ID == "" {
		request = request.WithContext(attemptmeta.Begin(request.Context(), attemptmeta.Path{}))
	}
	if err := infraegress.BeginDirectPhysicalCall(request.Context()); err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, err
	}
	meta := physical.MetaFromContext(request.Context())
	traceCtx, finishTrace := requestdiag.Network(request.Context(), meta.Plane, meta.Stage)
	request = request.WithContext(traceCtx)
	response, err := t.current.Load().RoundTrip(request)
	status := -1
	if response != nil {
		status = response.StatusCode
	}
	finishTrace(status, err)
	err = infraegress.MarkPhysicalExecutionError(request.Context(), err)
	attemptmeta.Attach(response, request)
	infraegress.RecordDirectPhysicalCall(request.Context(), response, err)
	return response, err
}

func (t *buildDirectTransport) UpdateResponseHeaderTimeout(responseHeaderTimeout time.Duration) {
	next := newBuildHTTPTransport(responseHeaderTimeout)
	previous := t.current.Swap(next)
	if previous != nil {
		previous.CloseIdleConnections()
	}
}

func newBuildHTTPTransport(responseHeaderTimeout time.Duration) *http.Transport {
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true,
		MaxIdleConns: 256, MaxIdleConnsPerHost: 128, MaxConnsPerHost: 256,
		IdleConnTimeout: buildtransport.IdleConnTimeout, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: normalizeBuildResponseHeaderTimeout(responseHeaderTimeout),
		ExpectContinueTimeout: time.Second,
	}
	if _, err := buildtransport.ConfigureHTTP2Health(transport); err != nil {
		slog.Warn("build_http2_health_config_failed", "error", err)
	}
	return transport
}

func normalizeBuildResponseHeaderTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return settingsdomain.DefaultBuildResponseHeaderTimeout
	}
	return value
}

func normalizeBuildStreamIdleTimeout(value time.Duration) time.Duration {
	if value <= 0 {
		return settingsdomain.DefaultBuildStreamIdleTimeout
	}
	return value
}

func (a *Adapter) config() Config {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.cfg
}

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (*provider.Response, error) {
	ctx, finishTrace := upstreamtrace.Network(ctx, "build", request.Operation)
	defer finishTrace()
	if request.NormalizedMetadata != nil {
		*request.NormalizedMetadata = provider.NormalizedRequestMetadata{}
	}
	accessToken, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	body := request.Body
	var toolCompatibility *responsesToolCompatibility
	var conversationOptions conversation.ResponseOptions
	cacheRoute := buildPromptCacheRoute{}
	cachePrepared := false
	compactionRequested := false
	if request.NormalizeBody {
		if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
			body, conversationOptions, err = conversation.ConvertRequestWithOptions(body, request.Model, request.Operation)
			if err == nil && conversationOptions.ReasoningEffortSet && request.NormalizedMetadata != nil {
				request.NormalizedMetadata.ReasoningEffort = conversationOptions.ReasoningEffort
			}
			if err == nil {
				body, err = normalizeBuildRequestWithMetadata(body, request.Model, request.Operation, request.NormalizedMetadata)
			}
		} else {
			preparation, prepareErr := a.compaction.Prepare(body, request.PromptCacheKey)
			if prepareErr != nil {
				return invalidResponsesResponse(prepareErr), nil
			}
			if preparation.Unavailable > 0 {
				if request.HistoryControl == nil {
					return invalidResponsesResponse(compactionPreparationError(historydomain.ErrCompactionLossNotAuthorized)), nil
				}
				if err := request.HistoryControl.PrepareInput(ctx, historydomain.InputPreparation{UnavailableCompactions: preparation.Unavailable}); err != nil {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return invalidResponsesResponse(compactionPreparationError(err)), nil
				}
			}
			body = preparation.Body
			body, toolCompatibility, cacheRoute, err = prepareBuildResponsesRequest(body, request)
			cachePrepared = true
			if toolCompatibility != nil {
				compactionRequested = toolCompatibility.compactionRequested
				if preparation.Unavailable > 0 {
					toolCompatibility.addWarning("foreign_compaction_omitted")
				}
				if preparation.SessionDrifted > 0 {
					toolCompatibility.addWarning("compaction_session_drifted")
				}
			}
		}
		if err != nil {
			if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
				return invalidConversationResponse(request.Operation, err), nil
			}
			return invalidResponsesResponse(err), nil
		}
	}
	continuity := historydomain.ReplayCompatibility{
		TranslatedSearchHistory: request.Operation == conversation.OperationMessages && conversationOptions.AnthropicWebSearch,
	}
	request.ReasoningReplayKey = continuity.Seed(request.ReasoningReplayKey)
	if compactionRequested {
		body, err = prepareGatewayCompactionSample(body)
		if err != nil {
			return invalidResponsesResponse(err), nil
		}
	}
	if len(body) > 0 && request.Method == http.MethodPost {
		if !compactionRequested && !cachePrepared {
			body, cacheRoute, err = prepareBuildPromptCacheRoute(body, request.Operation, request.Model, request.PromptCacheKey, request.ToolCompatibilityPolicy)
			if err != nil {
				err = fmt.Errorf("准备 Build prompt cache 路由: %w", err)
				if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
					return invalidConversationResponse(request.Operation, err), nil
				}
				return invalidResponsesResponse(err), nil
			}
		}
	}
	policy := inferencedomain.ReplayPolicyFromRequest(body)
	ctx = attemptmeta.WithNormalizedProfile(ctx, "responses", policy.ReasoningEffort, policy.Tools, policy.Reason != "unrecognized_request")
	request.DisableAutomaticReplay = request.DisableAutomaticReplay || !policy.Safe
	if request.NormalizedMetadata != nil {
		request.NormalizedMetadata.ReplayPolicy = &policy
		request.NormalizedMetadata.ToolCompatibility = &cacheRoute.plan
	}
	if request.OnNormalized != nil {
		metadata := provider.NormalizedRequestMetadata{ReplayPolicy: &policy, ToolCompatibility: &cacheRoute.plan}
		if request.NormalizedMetadata != nil {
			metadata = *request.NormalizedMetadata
		}
		if err := request.OnNormalized(metadata); err != nil {
			return nil, err
		}
	}
	if compactionRequested {
		warnings := ""
		if toolCompatibility != nil {
			warnings = toolCompatibility.warningHeader()
		}
		return a.forwardGatewayCompaction(ctx, request, accessToken, body, warnings)
	}
	// Explicit mode wins; in auto mode only confirmed Super accounts with bot_flag_source/bfs in {1,2} default to XAI.
	primaryBase := a.primaryBaseURL()
	base := a.inferenceBaseForOperation(request.Credential, request.Billing, request.Method, request.Path)
	// Cache affinity and reasoning replay use separate identities. Replay is also bound to the actual account and upstream plane,
	// preventing opaque reasoning issued for one account or Build plane from reaching another scope.
	replayBaseBody := body
	body, replayKey, prepared, prepareErr := a.prepareReasoningReplay(ctx, request, replayBaseBody, base)
	if prepareErr != nil {
		return nil, prepareErr
	}
	historyHandedOff := false
	defer func() {
		if !historyHandedOff {
			historydomain.Discard(prepared)
		}
	}()
	resp, reqURL, err := a.doResponseRequest(ctx, request, accessToken, body, base)
	if err != nil {
		return nil, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, err
	}
	resp, reqURL, reasoningRecovery := a.recoverReasoningDecodeFailure(ctx, request, accessToken, body, base, replayKey, resp, reqURL, prepared)
	if reasoningRecovery.prepared != nil {
		historydomain.Discard(prepared)
		prepared = reasoningRecovery.prepared
	}
	var recoveredPrimaryFailure *provider.DiagnosticResponse
	// Only eligible operations probe XAI with an equivalent request after the Build primary explicitly returns 403.
	if !request.DisableAutomaticReplay && strings.EqualFold(base, primaryBase) && shouldProbeXAIInferenceFallback(request.Credential, request.Billing, request.Method, request.Path, resp.StatusCode) {
		// Buffer the primary 403 body and replay it unchanged if fallback fails; never issue a second primary POST.
		primaryBody, primaryTruncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		primaryResp := cloneBufferedResponse(resp, primaryBody, primaryTruncated)
		if shouldSkipXAIFallback(primaryBody) {
			resp = primaryResp
		} else {
			fallbackBase := a.fallbackBaseURL()
			if fallbackBase != "" && !strings.EqualFold(fallbackBase, base) {
				fallbackBody, fallbackReplayKey, fallbackPrepared, fallbackPrepareErr := a.prepareReasoningReplay(ctx, request, replayBaseBody, fallbackBase)
				if fallbackPrepareErr != nil {
					_ = primaryResp.Body.Close()
					return nil, fallbackPrepareErr
				}
				fallbackCtx := infraegress.WithPhysicalCallStage(ctx, "plane_fallback")
				fallbackResp, fallbackURL, fallbackErr := a.doResponseRequest(fallbackCtx, request, accessToken, fallbackBody, fallbackBase)
				if fallbackErr == nil {
					fallbackErr = normalizeGzipResponse(fallbackResp)
				}
				fallbackRecovery := reasoningRecoveryOutcome{}
				if fallbackErr == nil {
					fallbackResp, fallbackURL, fallbackRecovery = a.recoverReasoningDecodeFailure(ctx, request, accessToken, fallbackBody, fallbackBase, fallbackReplayKey, fallbackResp, fallbackURL, fallbackPrepared)
				}
				if fallbackRecovery.prepared != nil {
					historydomain.Discard(fallbackPrepared)
					fallbackPrepared = fallbackRecovery.prepared
				}
				if fallbackErr == nil && isHTTPSuccess(fallbackResp.StatusCode) {
					historydomain.Discard(prepared)
					prepared = fallbackPrepared
					recoveredPrimaryFailure = bufferedFailureDiagnostic(primaryResp, primaryBody, primaryTruncated)
					a.activateBuildAPIFallback(ctx, &request.Credential)
					// base/body 赋值死存（后续统一从 resp.Body 重读），staticcheck SA4006；仅保留被消费的槽位。
					resp, reqURL, replayKey = fallbackResp, fallbackURL, fallbackReplayKey
					reasoningRecovery = reasoningRecovery.merge(fallbackRecovery)
				} else {
					if fallbackErr == nil {
						_ = fallbackResp.Body.Close()
					}
					historydomain.Discard(fallbackPrepared)
					// Preserve the original primary 403 URL and buffered body without requesting the primary again.
					resp = primaryResp
				}
			} else {
				resp = primaryResp
			}
		}
	}
	var rateLimit *provider.RateLimitMetadata
	var rateLimitDiagnostic *provider.DiagnosticResponse
	if resp.StatusCode == http.StatusTooManyRequests {
		body, truncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		rateLimit = provider.RateLimitFromResponse(resp.StatusCode, resp.Header, body)
		if truncated {
			rateLimitDiagnostic = &provider.DiagnosticResponse{
				StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(),
				Body: append([]byte(nil), body...), BodyTruncated: true,
			}
		}
	}
	modelCatalogChanged := a.modelCatalogChanged(request.Credential.ID, resp.Header.Get("x-models-etag"))
	responsesOperation := request.Operation == "" || request.Operation == conversation.OperationResponses || request.Operation == conversation.OperationCompaction
	if responsesOperation && toolCompatibility != nil {
		if warnings := toolCompatibility.warningHeader(); warnings != "" {
			resp.Header.Set("X-Grok2API-Compatibility-Warnings", warnings)
		}
	}
	reasoningRecovery.appendWarnings(resp.Header)
	if traceDir, ok := upstreamtrace.Enabled(); ok {
		upstreamtrace.DumpRequest(traceDir, request.Operation, request.Model, request.Streaming, body)
		if request.Streaming && resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body = upstreamtrace.TeeStream(traceDir, request.Operation, request.Model, resp.Body)
		} else if !request.Streaming && resp.StatusCode >= 200 && resp.StatusCode < 300 && request.Operation == conversation.OperationResponses {
			// native responses 非流式：body 直通下游读取，用读取镜像包装捕获。
			resp.Body = upstreamtrace.TeeBody(traceDir, request.Operation, request.Model, resp.Body)
		}
	}
	if request.Streaming && isHTTPSuccess(resp.StatusCode) && resp.Body != nil {
		stream := newBuildResponseStream(ctx, resp.Body, a.config().StreamIdleTimeout)
		physicalID := attemptmeta.FromResponse(resp).ID
		stream.Observe(func(event *responseflow.Event) {
			if event.HasData {
				infraegress.ObservePhysicalPayload(ctx, physicalID, event.Data)
				infraegress.ObservePhysicalGeneration(ctx, physicalID, responsecheck.EventGeneration(string(event.Kind), event.Data))
			}
		})
		resp.Body = stream
	} else if isHTTPSuccess(resp.StatusCode) && resp.Body != nil {
		resp.Body = responsebuffer.AttachBudget(resp.Body, responsebuffer.FromContext(ctx))
	}
	// Capture or clear reasoning replay in the upstream Responses shape before protocol conversion.
	var acceptOutput func()
	var commitOutput func() error
	var discardOutput func()
	if a.shouldCaptureReplay(request, resp, replayKey) {
		if prepared != nil {
			resp.Body, commitOutput, discardOutput = prepared.Capture(resp.Body, request.Streaming)
			historyHandedOff = true
		} else if request.DeferOutputCommit {
			resp.Body, acceptOutput = a.replay.CapturePendingBody(resp.Body, request.Model, replayKey, request.Streaming, isCompactPath(request.Path))
		} else {
			resp.Body = a.replay.CaptureBody(resp.Body, request.Model, replayKey, request.Streaming, isCompactPath(request.Path))
		}
	}
	if pending, ok := resp.Body.(interface{ DiscardOutput() }); ok {
		discardOutput = pending.DiscardOutput
	}
	result := &provider.Response{Attempt: attemptmeta.FromResponse(resp), StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: resp.Body, UpstreamURL: reqURL, Diagnostic: rateLimitDiagnostic, RecoveredPrimaryFailure: recoveredPrimaryFailure, RateLimit: rateLimit, ModelCatalogChanged: modelCatalogChanged, AcceptOutput: acceptOutput, CommitOutput: commitOutput, DiscardOutput: discardOutput}
	if prepared != nil {
		result.HistoryOutcome = prepared.Outcome()
		result.HistoryScopeHash = prepared.ScopeHash()
		result.HistoryGeneration = prepared.Generation()
		result.HistoryRestoredItems = prepared.RestoredItems()
		result.HistoryNormalizer = historydomain.JournalNormalizerVersion
	}
	if isHTTPSuccess(resp.StatusCode) {
		// Return the unread, decompressed upstream body. All semantic rewriting
		// belongs after gateway admission, including native Responses filtering.
		prepareBuildClientConversion(result, request, cacheRoute, toolCompatibility, conversationOptions)
		return result, nil
	}
	if request.Operation == conversation.OperationChat || request.Operation == conversation.OperationMessages {
		data, truncated, readErr := provider.ReadDiagnosticBody(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		result.Diagnostic = &provider.DiagnosticResponse{StatusCode: resp.StatusCode, Status: resp.Status, Header: resp.Header.Clone(), Body: data, BodyTruncated: truncated || rateLimitDiagnostic != nil}
		converted, convertErr := conversation.ConvertResponseJSONWithOptions(data, request.Operation, conversationOptions)
		if convertErr == nil {
			data = converted
			result.Header.Set("Content-Type", "application/json")
		}
		result.Body = io.NopCloser(bytes.NewReader(data))
		result.Header.Set("Content-Length", strconv.Itoa(len(data)))
	}
	return result, nil
}

func (a *Adapter) shouldCaptureReplay(request provider.ResponseResourceRequest, resp *http.Response, replayKey string) bool {
	if a.replay == nil || !a.replay.Enabled() || resp == nil {
		return false
	}
	if request.Method != http.MethodPost || strings.TrimSpace(replayKey) == "" {
		return false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	return true
}

func (a *Adapter) applyReasoningReplay(ctx context.Context, request provider.ResponseResourceRequest, body []byte, base string) ([]byte, string) {
	if a.replay == nil || !a.replay.Enabled() || request.Method != http.MethodPost {
		return body, ""
	}
	key := a.scopedReasoningReplayKey(request, base)
	if key == "" {
		return body, ""
	}
	if isCompactPath(request.Path) {
		// compact does not inject history, but a successful request still clears old replay in the same scope.
		return body, key
	}
	return a.replay.Apply(ctx, request.Model, key, body), key
}

func (a *Adapter) replayPlane(base string) historydomain.ReplayPlane {
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		return historydomain.ReplayPlaneXAI
	}
	return historydomain.ReplayPlaneBuild
}

func (a *Adapter) scopedReasoningReplayKey(request provider.ResponseResourceRequest, base string) string {
	return historydomain.ReplayScope(request.ReasoningReplayKey, request.Credential.ID, a.replayPlane(base))
}

func isCompactPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "compact")
}

// nextGrokTurnIndex 返回该会话的下一个轮次号(从 1 开始单调递增)。
// 表超上界时按 lastUsedAt 淘汰最旧的一批;会话中辍后计数自然作废,
// 不需要精确过期回收。
func (a *Adapter) nextGrokTurnIndex(key string) string {
	now := time.Now()
	a.turnsMu.Lock()
	defer a.turnsMu.Unlock()
	if a.turns == nil {
		a.turns = make(map[string]grokTurnState)
	}
	state := a.turns[key]
	state.index++
	state.lastUsedAt = now
	a.turns[key] = state
	if len(a.turns) > maxTrackedGrokTurns {
		type agedEntry struct {
			key        string
			lastUsedAt time.Time
		}
		candidates := make([]agedEntry, 0, len(a.turns))
		for k, s := range a.turns {
			candidates = append(candidates, agedEntry{key: k, lastUsedAt: s.lastUsedAt})
		}
		sort.Slice(candidates, func(i, j int) bool { return candidates[i].lastUsedAt.Before(candidates[j].lastUsedAt) })
		for i := 0; i < grokTurnPruneBatch && i < len(candidates); i++ {
			delete(a.turns, candidates[i].key)
		}
	}
	return strconv.FormatUint(state.index, 10)
}

func (a *Adapter) doResponseRequest(ctx context.Context, request provider.ResponseResourceRequest, accessToken string, body []byte, base string) (*http.Response, string, error) {
	ctx = requestdiag.WithPrompt(ctx, body)
	// Preserve client turn indices; otherwise maintain a local request sequence.
	// This is CLI telemetry, not a documented cache checkpoint mechanism.
	if request.GrokTurnIndex == "" && request.PromptCacheKey != "" && request.Credential.ID != 0 {
		request.GrokTurnIndex = a.nextGrokTurnIndex(request.PromptCacheKey)
	}
	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}
	requestCtx := infraegress.WithCredential(ctx, request.Credential)
	// Suggest network reuse within its pool/isolation/fresh policy. The hint
	// cannot change historical identity or guarantee an upstream cache hit.
	if request.PromptCacheKey != "" {
		requestCtx = infraegress.WithBuildSession(requestCtx, request.PromptCacheKey)
	}
	plane := "build"
	if fallback := a.fallbackBaseURL(); fallback != "" && strings.EqualFold(strings.TrimRight(base, "/"), fallback) {
		plane = "xai"
	}
	requestCtx = infraegress.WithPhysicalCallPlane(requestCtx, plane)
	requestCtx = attemptmeta.Begin(requestCtx, attemptmeta.Path{})
	req, err := http.NewRequestWithContext(requestCtx, request.Method, a.urlWithBase(base, request.Path), bodyReader)
	if err != nil {
		return nil, "", err
	}
	if err := a.applyHeaders(req, request.Credential, accessToken, request.Model, request.PromptCacheKey, true); err != nil {
		return nil, "", err
	}
	applyGrokTurnIndexHeader(req, request.GrokTurnIndex)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	if request.Streaming {
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Accept-Encoding", "identity")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if request.IdempotencyID != "" {
		req.Header.Set("Idempotency-Key", request.IdempotencyID)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, "", err
	}
	if attemptmeta.FromResponse(resp).ID == "" {
		attemptmeta.Attach(resp, req)
	}
	return resp, req.URL.String(), nil
}

// applyGrokTurnIndexHeader forwards a real client turn only when the request has a stable Grok session.
func applyGrokTurnIndexHeader(request *http.Request, value string) {
	if request.Header.Get("x-grok-session-id") == "" {
		return
	}
	if turnIndex := normalizeGrokTurnIndex(value); turnIndex != "" {
		request.Header.Set("x-grok-turn-idx", turnIndex)
	}
}

// normalizeGrokTurnIndex accepts only non-negative decimal u64 values generated by an official client.
// Empty or invalid values are omitted; the gateway never fabricates turns from history, tool loops, or compaction.
func normalizeGrokTurnIndex(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 20 {
		return ""
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return ""
		}
	}
	if _, err := strconv.ParseUint(value, 10, 64); err != nil {
		return ""
	}
	return value
}

// invalidResponsesResponse converts local protocol validation errors to a standard OpenAI error response,
// avoiding an upstream account retry.
func localRequestValidation(err error) *inferencedomain.RequestValidationError {
	code := "invalid_request"
	param := ""
	message := err.Error()
	var requestErr *responsesRequestError
	if errors.As(err, &requestErr) {
		code = requestErr.Code
		param = requestErr.Param
		message = requestErr.Message
	}
	return &inferencedomain.RequestValidationError{Code: code, Param: param, Message: message}
}

func invalidResponsesResponse(err error) *provider.Response {
	validation := localRequestValidation(toolConstraintError(err))
	code, param, message := validation.Code, validation.Param, validation.Message
	errorBody := map[string]any{"type": "invalid_request_error", "message": message, "code": code}
	if param != "" {
		errorBody["param"] = param
	}
	data, _ := json.Marshal(map[string]any{"error": errorBody})
	return &provider.Response{
		RequestValidation: validation,
		StatusCode:        http.StatusBadRequest, Status: "400 Bad Request",
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}

func invalidConversationResponse(operation string, err error) *provider.Response {
	validation := localRequestValidation(toolConstraintError(err))
	var payload any = map[string]any{"error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}
	if operation == conversation.OperationMessages {
		payload = map[string]any{"type": "error", "error": map[string]any{"type": "invalid_request_error", "message": err.Error()}}
	}
	data, _ := json.Marshal(payload)
	return &provider.Response{
		RequestValidation: validation,
		StatusCode:        http.StatusBadRequest, Status: "400 Bad Request",
		Header: http.Header{"Content-Type": []string{"application/json"}, "Content-Length": []string{strconv.Itoa(len(data))}},
		Body:   io.NopCloser(bytes.NewReader(data)),
	}
}

func (a *Adapter) ListModels(ctx context.Context, credential account.Credential) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	// Always request the model catalog from the Build primary; do not preemptively switch to XAI because 1.5 or Super entitlement is absent.
	// NormalizeAccountModelCapabilities fills session-contract capabilities such
	// as Composer and paid video entitlement locally.
	models, status, err := a.listModelsAt(ctx, credential, accessToken, a.primaryBaseURL())
	if err != nil {
		return nil, err
	}
	if models != nil {
		return models, nil
	}
	return nil, fmt.Errorf("上游模型接口返回 %d", status)
}

// NormalizeAccountModelCapabilities normalizes capabilities that the OAuth
// session contract exposes independently of the account's sparse /models list.
// Composer is available to Build OAuth sessions independently of the sparse
// live catalog. Grok 4.6 sessions retain the still-supported Grok 4.5 route for
// backwards compatibility. Super always includes video 1.5; Free and Unknown
// remove video 1.5 exactly. BuildAPIFallback is ignored.
func (a *Adapter) NormalizeAccountModelCapabilities(models []string, billing *account.Billing, credential account.Credential) []string {
	super := account.IsBuildSuper(credential, billing)
	composer := credential.Provider == account.ProviderBuild && credential.AuthType == account.AuthTypeOAuth
	result := make([]string, 0, len(models)+2)
	seen := make(map[string]struct{}, len(models)+2)
	hasVideo15 := false
	hasGrok46 := false
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, exists := seen[model]; exists {
			continue
		}
		if model == buildVideoModel {
			if !super {
				continue
			}
			hasVideo15 = true
		}
		if model == buildGrok46Model {
			hasGrok46 = true
		}
		seen[model] = struct{}{}
		result = append(result, model)
	}
	if credential.Provider == account.ProviderBuild && hasGrok46 {
		if _, exists := seen[buildGrok45Model]; !exists {
			seen[buildGrok45Model] = struct{}{}
			result = append(result, buildGrok45Model)
		}
	}
	if super && !hasVideo15 {
		result = append(result, buildVideoModel)
	}
	if composer {
		if _, exists := seen[modeldomain.GrokComposer25Fast]; !exists {
			result = append(result, modeldomain.GrokComposer25Fast)
		}
	}
	return result
}

type buildModelCatalogEntry struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	ModelID string `json:"modelId"`
	Hidden  bool   `json:"hidden"`
	Meta    struct {
		Model   string `json:"model"`
		ModelID string `json:"modelId"`
		Hidden  bool   `json:"hidden"`
	} `json:"_meta"`
}

// modelIdentifier keeps the legacy top-level id authoritative and uses the
// additional official Grok Build shapes only as fallbacks. This makes catalog
// parsing additive: existing route IDs never change merely because model or
// modelId metadata appears alongside id.
func (e buildModelCatalogEntry) modelIdentifier() string {
	if e.Hidden || e.Meta.Hidden {
		return ""
	}
	return texts.FirstNonEmptyTrimmed(e.ID, e.Model, e.ModelID, e.Meta.Model, e.Meta.ModelID)
}

func (a *Adapter) listModelsAt(ctx context.Context, credential account.Credential, accessToken, base string) ([]string, int, error) {
	requestCtx := infraegress.WithTrafficClass(infraegress.WithCredential(ctx, credential), domainegress.TrafficClassModelSync)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, a.urlWithBase(base, "/models"), nil)
	if err != nil {
		return nil, 0, err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return nil, 0, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, 0, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, truncated, err := readControlDocument(resp.Body, 4<<20)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, nil
	}
	if truncated {
		return nil, resp.StatusCode, fmt.Errorf("Build models 响应超过 4 MiB")
	}
	var payload struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, resp.StatusCode, err
	}
	models := make([]string, 0, len(payload.Data))
	seen := make(map[string]struct{}, len(payload.Data))
	for _, raw := range payload.Data {
		var item buildModelCatalogEntry
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		identifier := item.modelIdentifier()
		if identifier == "" {
			continue
		}
		if _, exists := seen[identifier]; exists {
			continue
		}
		seen[identifier] = struct{}{}
		models = append(models, identifier)
	}
	a.recordModelsETag(credential.ID, resp.Header.Get("ETag"))
	return models, resp.StatusCode, nil
}

func (a *Adapter) recordModelsETag(accountID uint64, etag string) {
	etag = strings.TrimSpace(etag)
	if etag == "" {
		return
	}
	a.modelsMu.Lock()
	if a.modelsETags == nil {
		a.modelsETags = make(map[uint64]string)
	}
	a.modelsETags[accountID] = etag
	a.modelsMu.Unlock()
}

func (a *Adapter) modelCatalogChanged(accountID uint64, etag string) bool {
	etag = strings.TrimSpace(etag)
	if etag == "" {
		return false
	}
	a.modelsMu.Lock()
	defer a.modelsMu.Unlock()
	if a.modelsETags == nil {
		a.modelsETags = make(map[uint64]string)
	}
	current := a.modelsETags[accountID]
	if current == "" {
		// After a process restart there is no in-memory catalog baseline. Let the Gateway perform one account-level
		// /models sync; recordModelsETag establishes the baseline after success.
		return true
	}
	return current != etag
}

func (a *Adapter) GetBilling(ctx context.Context, credential account.Credential) (account.Billing, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	accessToken, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return account.Billing{}, err
	}
	billing, err := a.getBilling(ctx, credential, accessToken, "format=credits")
	if err != nil {
		return account.Billing{}, err
	}
	// A weekly quota at 0% usage cannot distinguish Free from a newly activated paid plan.
	// The official CLI uses /user?include=subscription for the live subscription tier, then falls back to the JWT tier.
	if tier, tierErr := a.getSubscriptionTier(ctx, credential, accessToken); tierErr == nil && tier != "" {
		billing.PlanName = tier
	} else if billing.PlanCode == "" && billing.PlanName == "" {
		billing.PlanName = subscriptionTierFromJWT(accessToken)
	}
	billing.AccountID = credential.ID
	billing.SyncedAt = time.Now().UTC()
	return billing, nil
}

func (a *Adapter) RefreshCredential(ctx context.Context, credential account.Credential) (provider.RefreshedCredential, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	refreshToken, err := a.cipher.Decrypt(credential.EncryptedRefreshToken)
	if err != nil {
		// Decryption failures are usually temporary or mismatched local encryption keys and are recoverable;
		// do not mark them permanent, or manual/batch refresh will never retry after the key is fixed.
		// True permanent OAuth failures such as invalid_grant are returned by oauth.refresh with Permanent=true.
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "credential_decrypt_failed", Message: "Stored refresh credential could not be decrypted", Permanent: false, Cause: err}
	}
	if strings.TrimSpace(refreshToken) == "" {
		return provider.RefreshedCredential{}, &provider.CredentialRefreshError{Code: "missing_refresh_token", Message: "Refresh token is missing", Permanent: true}
	}
	refreshCtx := infraegress.WithCredential(ctx, credential)
	tokens, err := a.oauth.refreshWithClientID(refreshCtx, refreshToken, credential.OIDCClientID)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	accessEncrypted, err := a.cipher.Encrypt(tokens.AccessToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	refreshEncrypted, err := a.cipher.Encrypt(tokens.RefreshToken)
	if err != nil {
		return provider.RefreshedCredential{}, err
	}
	return provider.RefreshedCredential{EncryptedAccessToken: accessEncrypted, EncryptedRefreshToken: refreshEncrypted, ExpiresAt: tokens.ExpiresAt, RefreshTokenRotated: tokens.RefreshTokenRotated}, nil
}

func (a *Adapter) StartDeviceAuthorization(ctx context.Context) (provider.DeviceAuthorization, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	return a.oauth.startDevice(ctx)
}

func (a *Adapter) PollDeviceAuthorization(ctx context.Context, deviceCode string) (provider.CredentialSeed, error) {
	ctx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	tokens, err := a.oauth.pollDevice(ctx, deviceCode)
	if err != nil {
		return provider.CredentialSeed{}, err
	}
	claims := decodeJWTClaims(texts.FirstNonEmptyTrimmed(tokens.IDToken, tokens.AccessToken))
	userID := stringClaim(claims, "sub")
	email := stringClaim(claims, "email")
	return provider.CredentialSeed{Name: texts.FirstNonEmptyTrimmed(email, userID, "Grok Build account"), Email: email, UserID: userID, TeamID: stringClaim(claims, "team_id"), OIDCClientID: defaultOAuthClientID, AccessToken: tokens.AccessToken, RefreshToken: tokens.RefreshToken, ExpiresAt: tokens.ExpiresAt}, nil
}

func (a *Adapter) ParseImportedCredentials(data []byte) ([]provider.CredentialSeed, error) {
	return parseImportedCredentials(data)
}

// PrepareImportedCredential validates a refresh-token-only import and keeps
// the rotated tokens returned by xAI before the account is persisted.
func (a *Adapter) PrepareImportedCredential(ctx context.Context, seed provider.CredentialSeed) (provider.CredentialSeed, error) {
	if strings.TrimSpace(seed.AccessToken) != "" || strings.TrimSpace(seed.RefreshToken) == "" {
		return seed, nil
	}
	refreshCtx, cancel := context.WithTimeout(ctx, buildControlTimeout)
	defer cancel()
	tokens, err := a.oauth.refreshWithClientID(refreshCtx, strings.TrimSpace(seed.RefreshToken), seed.OIDCClientID)
	if err != nil {
		return provider.CredentialSeed{}, fmt.Errorf("验证 Grok Build refresh token: %w", err)
	}
	claims := decodeJWTClaims(texts.FirstNonEmptyTrimmed(tokens.IDToken, tokens.AccessToken))
	seed.AccessToken = tokens.AccessToken
	seed.RefreshToken = tokens.RefreshToken
	seed.ExpiresAt = tokens.ExpiresAt
	seed.OIDCClientID = texts.FirstNonEmptyTrimmed(seed.OIDCClientID, defaultOAuthClientID)
	seed.UserID = texts.FirstNonEmptyTrimmed(seed.UserID, stringClaim(claims, "sub"))
	seed.Email = texts.FirstNonEmptyTrimmed(seed.Email, stringClaim(claims, "email"))
	seed.TeamID = texts.FirstNonEmptyTrimmed(seed.TeamID, stringClaim(claims, "team_id"))
	if seed.Name == "" || seed.Name == "Grok Build account" {
		seed.Name = texts.FirstNonEmptyTrimmed(seed.Email, seed.UserID, "Grok Build account")
	}
	identity := texts.FirstNonEmptyTrimmed(seed.UserID, strings.ToLower(seed.Email), seed.TeamID, seed.RefreshToken, seed.AccessToken)
	seed.SourceKey = "import:" + security.HashToken(strings.Join([]string{credentialImportProvider, seed.OIDCClientID, identity}, "|"))
	return seed, nil
}

func (a *Adapter) MarshalCredentials(values []provider.CredentialSeed) ([]byte, error) {
	return marshalCredentials(values)
}

func (a *Adapter) applyHeaders(req *http.Request, credential account.Credential, accessToken, model, promptCacheKey string, trace bool) error {
	cfg := a.config()
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-XAI-Token-Auth", cfg.TokenAuth)
	req.Header.Set("x-grok-client-version", cfg.ClientVersion)
	req.Header.Set("x-grok-client-identifier", cfg.ClientIdentifier)
	req.Header.Set("x-grok-client-mode", "headless")

	if trace {
		requestID := uuid.NewString()
		// Set x-grok-conv-id and session-id only when a stable session exists.
		// Never generate a random UUID per request; it breaks xAI session affinity and keeps cached_tokens at zero.
		sessionID, err := grokSessionID(promptCacheKey)
		if err != nil {
			return err
		}
		req.Header.Set("x-authenticateresponse", "authenticate-response")
		req.Header.Set("x-grok-agent-id", a.agentID)
		if sessionID != "" {
			req.Header.Set("x-grok-session-id", sessionID)
			req.Header.Set("x-grok-conv-id", sessionID)
		}
		req.Header.Set("x-grok-req-id", requestID)
		// The gateway cannot reliably recover the CLI prompt index from a stateless API request.
		// The field is optional in the official protocol, so do not fabricate x-grok-turn-idx.
		if credential.UserID != "" {
			req.Header.Set("x-grok-user-id", credential.UserID)
		}
		traceID, traceErr := randomHex(16)
		if traceErr != nil {
			return traceErr
		}
		spanID, spanErr := randomHex(8)
		if spanErr != nil {
			return spanErr
		}
		req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	} else {
		if credential.UserID != "" {
			req.Header.Set("x-userid", credential.UserID)
		}
		if credential.Email != "" {
			req.Header.Set("x-email", credential.Email)
		}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("User-Agent", cfg.UserAgent)
	if model != "" {
		req.Header.Set("x-grok-model-override", model)
	}
	return nil
}

// grokSessionID converts a stable session key to the upstream x-grok-conv-id.
// An empty key returns an empty string; stateless requests never receive a random ID.
func grokSessionID(promptCacheKey string) (string, error) {
	key := strings.TrimSpace(promptCacheKey)
	if key == "" {
		return "", nil
	}
	if parsed, err := uuid.Parse(key); err == nil {
		return parsed.String(), nil
	}
	return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, []byte("grok2api:session:"+key), 8).String(), nil
}

func randomHex(bytesLength int) (string, error) {
	value := make([]byte, bytesLength)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func normalizeGzipResponse(response *http.Response) error {
	if response == nil || response.Body == nil || !strings.EqualFold(strings.TrimSpace(response.Header.Get("Content-Encoding")), "gzip") {
		return nil
	}
	reader, err := gzip.NewReader(response.Body)
	if err != nil {
		_ = response.Body.Close()
		return err
	}
	response.Body = &gzipResponseBody{Reader: reader, source: response.Body}
	response.Header.Del("Content-Encoding")
	response.Header.Del("Content-Length")
	response.ContentLength = -1
	return nil
}

type gzipResponseBody struct {
	*gzip.Reader
	source io.Closer
}

func (b *gzipResponseBody) Close() error {
	readerErr := b.Reader.Close()
	sourceErr := b.source.Close()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func (a *Adapter) url(path string) string {
	return strings.TrimRight(a.config().BaseURL, "/") + "/" + strings.TrimLeft(path, "/")
}

func (a *Adapter) getBilling(ctx context.Context, credential account.Credential, accessToken, query string) (account.Billing, error) {
	endpoint := a.url("/billing")
	if query != "" {
		endpoint += "?" + query
	}
	requestCtx := infraegress.WithTrafficClass(infraegress.WithCredential(ctx, credential), domainegress.TrafficClassBilling)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return account.Billing{}, err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return account.Billing{}, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return account.Billing{}, err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return account.Billing{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, truncated, err := readControlDocument(resp.Body, 2<<20)
	if err != nil {
		return account.Billing{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return account.Billing{}, fmt.Errorf("上游 Billing 接口返回 %d", resp.StatusCode)
	}
	if truncated {
		return account.Billing{}, fmt.Errorf("Build Billing 响应超过 2 MiB")
	}
	return parseBilling(body)
}

func (a *Adapter) getSubscriptionTier(ctx context.Context, credential account.Credential, accessToken string) (string, error) {
	endpoint := a.url("/user") + "?include=subscription"
	requestCtx, cancel := context.WithTimeout(infraegress.WithTrafficClass(infraegress.WithCredential(ctx, credential), domainegress.TrafficClassBilling), subscriptionTierTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	if err := a.applyHeaders(req, credential, accessToken, "", "", false); err != nil {
		return "", err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	if err := normalizeGzipResponse(resp); err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, truncated, err := readControlDocument(resp.Body, 1<<20)
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("上游订阅接口返回 %d", resp.StatusCode)
	}
	if truncated {
		return "", fmt.Errorf("Build subscription 响应超过 1 MiB")
	}
	return parseSubscriptionTier(body)
}

func (a *Adapter) prepareReasoningReplay(ctx context.Context, request provider.ResponseResourceRequest, body []byte, base string) ([]byte, string, historydomain.Prepared, error) {
	if a.replay == nil || request.Method != http.MethodPost {
		next, key := a.applyReasoningReplay(ctx, request, body, base)
		return next, key, nil, nil
	}
	key := a.scopedReasoningReplayKey(request, base)
	next, prepared, err := a.replay.Prepare(ctx, request.Model, key, body, historydomain.ReplayPreparation{LegacyKeys: a.legacyReasoningReplayKeys(request, base), PriorKeys: a.priorReasoningReplayKeys(request, base), Authorizer: request.HistoryControl})
	return next, key, prepared, err
}

// SetLegacyReplayAccounts is startup-only migration wiring, including retired accounts.
func (a *Adapter) SetLegacyReplayAccounts(ids []uint64) {
	a.legacyReplayAccounts = append([]uint64(nil), ids...)
}

func (a *Adapter) legacyReasoningReplayKeys(request provider.ResponseResourceRequest, base string) []string {
	return historydomain.LegacyReplayScopes(request.ReasoningReplayKey, request.Credential.ID, a.replayPlane(base), a.legacyReplayAccounts)
}

func (a *Adapter) priorReasoningReplayKeys(request provider.ResponseResourceRequest, base string) []string {
	if request.PriorReasoningReplayKey == "" {
		return nil
	}
	plane := a.replayPlane(base)
	keys := []string{historydomain.ReplayScope(request.PriorReasoningReplayKey, request.Credential.ID, plane)}
	return append(keys, historydomain.LegacyReplayScopes(request.PriorReasoningReplayKey, request.Credential.ID, plane, a.legacyReplayAccounts)...)
}
