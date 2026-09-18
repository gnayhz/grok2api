package gateway

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/chenyme/grok2api/backend/internal/application/selector"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestdiag"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// responseExecution owns one logical request until deliverySession accepts it.
// Preparation, account attempts and response admission mutate this state serially.
// The delivery handoff freezes only completion metadata; it does not retain this
// request body, candidate state or retry bookkeeping.
type responseExecution struct {
	service                 *Service
	handedOff               bool
	snapshotScope           QualityJurisdiction
	auditOperation          audit.Operation
	degradedNodes           map[uint64]struct{}
	ctx                     context.Context
	input                   Input
	path                    string
	recoverHistory          *historyRecoveryState
	holdCfg                 QualityRetryRuntime
	egressTrace             *portphysical.Trace
	startedAt               time.Time
	admission               *admission
	firstToken              *firstTokenTimer
	eventID                 string
	operation               audit.Operation
	route                   modeldomain.Route
	ownership               *inferencedomain.ResponseOwnership
	timing                  *generationTiming
	usageSource             audit.UsageSource
	auditBase               audit.Record
	affinityKey             string
	ownershipPromptCacheKey string
	reasoningReplayKey      string
	priorReasoningReplayKey string
	physicalCallCtx         context.Context
	handoffPhysicalID       string
	supportsStoredResponses bool
	attemptPolicy           routingAttemptPolicy
	idempotencyID           string
	requestBudget           *inferencedomain.AttemptBudget
	pricingModel            string
	reserved                bool
	excluded                map[uint64]bool
	failureFingerprints     map[string]int
	qualityHoldEnabled      bool
	peekSchedule            QualityRetryRuntime
	qualityAccountAttempts  int
	replaySafety            inferencedomain.ReplayPolicy
	physicalStarted         bool
	firstGuardSignal        GuardSignal
	quotaMode               string
	quotaProbeAttempted     bool
	selection               *selector.SelectionSession
	lastErr                 error
	lastFailure             *UpstreamFailure
	failureAttempts         *failureAttemptRecorder
	normalizedMetadata      *provider.NormalizedRequestMetadata
	responseStartedAt       time.Time
	pendingOutput           *provider.Response
	imageFacts              *imageGeneration
	textFacts               *textGeneration
	lease                   *selector.Lease
}

func (s *Service) createResponseAt(ctx context.Context, input Input, path string) (result *Result, resultErr error) {
	ctx = requestdiag.WithCollector(ctx)
	// Large inputs include inline media and their encrypted durable history.
	// Give that sequential preparation room within the unchanged process pool;
	// an explicitly supplied (for example probe) budget is never relaxed.
	if len(input.Body) > 8<<20 {
		ctx = responsebuffer.WithRequestLimit(ctx, 256<<20)
	}
	r := &responseExecution{service: s, ctx: ctx, input: input, path: path, startedAt: time.Now()}
	r.holdCfg, r.snapshotScope = s.requestGuardSnapshot()
	r.ctx, r.egressTrace = portphysical.WithTrace(r.ctx)
	// Admission starts unarmed until model jurisdiction and exemptions are known.
	r.admission = newAdmission(r.ctx, r.startedAt, 0)
	r.ctx = responsebuffer.WithContext(r.admission.Context(), responsebuffer.FromContext(r.ctx))
	defer r.finish(&result, &resultErr)
	if input.Streaming {
		r.firstToken = newFirstTokenTimer(r.startedAt)
	}
	r.eventID = s.newAuditEventID()
	r.ctx = attemptmeta.WithRequest(r.ctx, r.eventID, r.holdCfg.Revision, r.holdCfg.RuleVersion, r.holdCfg.PathResolver())
	r.operation = input.Operation
	if r.operation == "" {
		r.operation = audit.OperationResponses
	}
	r.auditOperation = r.operation
	if input.auditOperation != "" {
		r.auditOperation = input.auditOperation
	}
	routeStarted := time.Now()
	if err := r.prepareRoute(); err != nil {
		return nil, err
	}
	requestdiag.Stage(r.ctx, "route", routeStarted)
	if err := r.prepareExecution(); err != nil {
		return nil, err
	}
	return r.runAttempts()
}

// finish preserves cleanup order: request-owned resources, billing, physical
// facts, timing, admission, and finally the history recovery diagnostic.
func (r *responseExecution) finish(result **Result, resultErr *error) {
	// Independent defers preserve cleanup even if a downstream close or discard
	// callback panics. Register in reverse order of dependency release.
	defer func() {
		var failure *UpstreamFailure
		if r.recoverHistory != nil && errors.As(*resultErr, &failure) {
			failure.HistoryRecovery = r.recoverHistory.snapshot()
		}
	}()
	defer func() {
		if err := r.admission.failure(); err != nil {
			if *result != nil {
				_ = (*result).Body.Close()
				*result = nil
			}
			*resultErr = err
		}
		if *result == nil {
			r.admission.close()
		}
	}()
	defer func() {
		if r.timing != nil && !r.handedOff {
			r.timing.finish(r.service.logger, "failed")
		}
	}()
	defer r.finishPhysicalEvents(result, resultErr)
	defer func() {
		if r.requestBudget != nil && !r.handedOff {
			r.requestBudget.Close()
		}
	}()
	defer func() {
		if r.reserved && !r.handedOff {
			r.service.cancelBillingReservation(r.eventID)
		}
	}()
	defer r.discardPendingOutput()
	if !r.handedOff {
		r.lease.Release()
	}
}

func (r *responseExecution) finishPhysicalEvents(result **Result, resultErr *error) {
	if r.physicalCallCtx != nil {
		if err := r.service.recordPhysicalEvents(r.physicalCallCtx, r.handoffPhysicalID); err != nil {
			r.service.logger.Error("physical_attempt_events_failed", "request_id", r.input.RequestID, "error", err)
			if *result != nil {
				_ = (*result).Body.Close()
				*result = nil
			}
			*resultErr = &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_event_unavailable", PublicMessage: "上游尝试记录暂时无法保存，请稍后重试", Cause: err}
		}
	}
}

func (r *responseExecution) discardPendingOutput() {
	if r.pendingOutput != nil && r.pendingOutput.DiscardOutput != nil {
		r.pendingOutput.DiscardOutput()
	}
	r.pendingOutput = nil
}

func (r *responseExecution) prepareExecution() error {
	defer requestdiag.Stage(r.ctx, "execution_prepare", time.Now())
	r.physicalCallCtx = r.service.startPhysicalTrace(r.ctx, string(r.route.Provider), string(r.operation))

	// degradedNodes 收集本请求内被守卫判定降智的出口节点。注入
	// WithNodeExclusions 后,后续 attempt 会换到其他固定出口 IP;账号
	// 绑定不变,仅本请求绕开(G14:扣留后的重试必须换路)。空流/扣留/
	// 头预算三条降智路径共用。
	r.degradedNodes = make(map[uint64]struct{})
	r.supportsStoredResponses = r.service.providers.SupportsStoredResponseModel(r.route.Provider, r.route.UpstreamModel)
	if r.input.PreviousResponseID != "" && !r.supportsStoredResponses {
		return ErrResponseStateUnsupported
	}
	r.attemptPolicy = newRoutingAttemptPolicy(int(r.service.maxAttempts.Load()))
	r.idempotencyID, _ = r.service.tokens.NewOpaqueToken(18)
	if r.ownership != nil {
		r.attemptPolicy = newRoutingAttemptPolicy(1)
	}
	physicalLimit := r.attemptPolicy.limit
	if r.route.Provider != accountdomain.ProviderBuild || r.attemptPolicy.unlimited || physicalLimit > portphysical.MaxPhysicalCalls {
		physicalLimit = portphysical.MaxPhysicalCalls
	}
	r.requestBudget = inferencedomain.NewAttemptBudget(physicalLimit)

	r.physicalCallCtx = portphysical.WithPhysicalCallBudget(r.physicalCallCtx, r.requestBudget)
	recoveryMode := historydomain.AllowLossyRecovery
	if r.input.HistoryRecoveryPolicy != nil {
		recoveryMode = *r.input.HistoryRecoveryPolicy
	}
	r.recoverHistory = newHistoryRecoveryState(recoveryMode, r.requestBudget)
	r.pricingModel = r.service.providers.PricingModel(r.route.Provider, r.route.UpstreamModel)
	if err := r.service.checkLedgerReady(); err != nil {
		return err
	}
	r.reserved = false

	if reservation, priced := audit.EstimateOfficialTextReservation(r.pricingModel, r.input.Body); priced {
		var err error
		if r.reserved, err = r.service.clientKeys.ReserveBilling(r.ctx, r.input.ClientKey, r.eventID, reservation.CostInUSDTicks, r.service.textBillingReservationTTL()); err != nil {
			return err
		}
	}
	r.excluded = make(map[uint64]bool)
	r.failureFingerprints = make(map[string]int)
	exemptReason := qualityHoldExemptReason(r.input, r.ownership, r.route, r.operation, r.holdCfg, r.snapshotScope)
	if r.holdCfg.Unavailable() != nil {
		return &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "quality_guard_unavailable", PublicMessage: "响应守卫暂不可用", Cause: r.holdCfg.Unavailable()}
	}
	r.qualityHoldEnabled = exemptReason == ""
	if r.qualityHoldEnabled && r.operation == audit.OperationChat {
		if choices, ok := jsonpeek.RootIntFieldScan(r.input.Body, "n"); ok && choices > 1 {
			return &UpstreamFailure{HTTPStatus: http.StatusBadRequest, Code: "unsupported_choices", PublicMessage: "当前响应守卫仅支持 n=1", Cause: errQualityChoices}
		}
	}
	// 活跃度预算制度表先用客户端请求体预计算一次(128KB body 的 tools/effort
	// 全量探测实测 1.16ms/次,原实现在循环内每次质量尝试重复支付)。adapter
	// 归一化回调会在上游调用前用规范化轮廓覆盖它:web_search_options、
	// Messages thinking 和 max 等别名只在归一化后才成为工具或 high/xhigh。
	r.peekSchedule = r.holdCfg
	// Real-time guard observability. Gate lines are emitted only when the hold
	// is engaged: a per-request INFO line while the feature is off would be pure
	// log amplification proportional to traffic.
	if r.qualityHoldEnabled {
		r.peekSchedule = qualityLivenessSchedule(r.input.Body, string(r.operation), r.holdCfg)
		// Arm the deadline only after jurisdiction and exemptions are settled.
		// The budget measures from request start, so governed requests keep the
		// same absolute deadline; exempt requests never carry one.
		r.admission.setBudget(r.peekSchedule.AdmissionTimeout)
		r.service.logger.Info("quality_hold_gate", "request_id", r.input.RequestID, "provider", r.route.Provider, "public_model", r.input.PublicModel, "upstream_model", r.route.UpstreamModel, "operation", r.operation)
	} else {
		r.admission.disable()
		// 豁免留痕：每条路径放行多少请求进 guard-stats（exempts 计数），
		// 不再重演"连续多发裸奔却无任何痕迹可查"。
		// 豁免 token 同步落审计主行（QualityExempt）：守卫不在场的交付，
		// 事后从审计列表即可回答"为何没拦"，不必交叉日志与计数器复原。
		guardStats.recordExempt(exemptReason)
		r.auditBase.QualityExempt = exemptReason
	}
	// Count accounts that actually reached the upstream. Credential-only skips
	// do not consume the quality retry budget; refreshes stay on the same account.
	r.qualityAccountAttempts = 0
	r.replaySafety = inferencedomain.ReplayPolicyFromRequest(r.input.Body)
	r.physicalStarted = false
	// firstGuardSignal 记录本请求首个触发的守卫特征(请求级结局归因:
	// 该特征触发的请求最终被救回还是失败——量化每个规则对降智的拦截价值)。
	r.firstGuardSignal = GuardSignal("")
	r.quotaMode = r.service.providers.QuotaMode(r.route.Provider, r.route.UpstreamModel)
	r.quotaProbeAttempted = false

	r.failureAttempts = newFailureAttemptRecorder(http.MethodPost, r.path)
	r.normalizedMetadata = &provider.NormalizedRequestMetadata{}
	r.responseStartedAt = r.startedAt

	if r.route.Capability != modeldomain.CapabilityImage {
		r.textFacts = newTextGeneration(r.physicalCallCtx, r.usageSource, r.pricingModel)
	}
	return nil
}
func (r *responseExecution) forwardResponse(lease *selector.Lease, credential accountdomain.Credential, billing *accountdomain.Billing) (*provider.Response, error) {
	r.discardPendingOutput()
	started := time.Now()
	r.responseStartedAt = started
	if r.physicalStarted && !r.replaySafety.Safe {
		return nil, &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "unsafe_replay_blocked",
			PublicMessage: "上游请求无法安全自动重放，请确认工具执行结果", Cause: errors.New(r.replaySafety.Reason)}
	}
	r.physicalStarted = true
	if r.route.Capability == modeldomain.CapabilityImage {
		r.imageFacts = &imageGeneration{}
	}
	r.textFacts.begin(credential, lease.QuotaMode, lease.QuotaSnapshotVersion)
	lease.MarkSelectorUpstreamStarted()
	request := provider.ResponseResourceRequest{Credential: credential, Billing: billing, Method: http.MethodPost, Path: r.path, Model: r.route.UpstreamModel, PromptCacheKey: r.input.PromptCacheKey, ReasoningReplayKey: r.reasoningReplayKey, PriorReasoningReplayKey: r.priorReasoningReplayKey, ToolCompatibilityPolicy: inferencedomain.AllowDisabledCacheTools, GrokTurnIndex: r.input.GrokTurnIndex, IdempotencyID: r.idempotencyID, Body: r.input.Body, Streaming: r.input.Streaming, NormalizeBody: true, Operation: string(r.operation), NormalizedMetadata: r.normalizedMetadata, DeferOutputCommit: true, DisableAutomaticReplay: !r.replaySafety.Safe, HistoryControl: r.recoverHistory,
		OnNormalized: func(metadata provider.NormalizedRequestMetadata) error {
			if metadata.ImageOutputCount > 0 && !r.reserved {
				if price, priced := audit.EstimateOfficialImageCost(r.pricingModel, "", "", metadata.ImageOutputCount); priced {
					var err error
					r.reserved, err = r.service.clientKeys.ReserveBilling(r.ctx, r.input.ClientKey, r.eventID, price.CostInUSDTicks, mediaBillingReservationTTL)
					if err != nil {
						return err
					}
				}
			}

			if metadata.ToolCompatibility != nil {
				if err := metadata.ToolCompatibility.Validate(inferencedomain.AllowDisabledCacheTools); err != nil {
					return err
				}
			}
			if r.qualityHoldEnabled && metadata.ReplayPolicy != nil {
				// The normalized profile is the actual upstream shape: reschedule
				// total admission and both liveness phases from it.
				r.peekSchedule = qualityLivenessScheduleForProfile(metadata.ReplayPolicy.Tools, metadata.ReasoningEffort, r.holdCfg)
				r.admission.setBudget(r.peekSchedule.AdmissionTimeout)
			}
			return r.admission.failure()
		},
	}
	if r.imageFacts != nil {
		request.ObserveImage = r.imageFacts.observe
	}
	attemptCtx, resources := selector.NewAttemptResources(r.physicalCallCtx)
	lease.ReplaceResources(resources)
	attemptCtx = attemptmeta.WithAccount(attemptCtx, credential.ID, string(r.route.Provider), r.route.UpstreamModel)
	response, err := r.service.runPhysicalAttempt(attemptCtx, request, resources)
	r.recoverHistory.annotate(response)
	r.textFacts.accept(response, r.supportsStoredResponses && r.operation == audit.OperationResponses)
	if r.normalizedMetadata.ReplayPolicy != nil && !r.normalizedMetadata.ReplayPolicy.Safe {
		r.replaySafety = *r.normalizedMetadata.ReplayPolicy
	}
	r.auditBase.ReasoningEffort = r.normalizedMetadata.ReasoningEffort
	err = r.failureAttempts.captureResponse(credential, started, response, err)
	if r.imageFacts != nil {
		facts, _ := r.imageFacts.snapshot()
		if facts.OutputImages > 0 && err != nil {
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			response = imageCompatibilityFailure(err)
			err = nil
		}
	}
	if response != nil {
		response.Body = resources.Own(response.Body)
	}
	r.pendingOutput = response
	r.timing.markUpstream(time.Since(started))
	return response, err
}
func (r *responseExecution) ensureCredential(credential accountdomain.Credential, force bool) (accountdomain.Credential, error) {
	started := time.Now()
	result, err := r.service.accounts.EnsureCredential(r.ctx, credential, force)
	r.failureAttempts.captureCredentialFailure(credential, started, force, err)
	r.timing.markCredential(time.Since(started))
	requestdiag.Stage(r.ctx, "credential", started)
	return result, err
}
func (r *responseExecution) handoffResponse(response *provider.Response, lease *selector.Lease, credential accountdomain.Credential, upstreamStartedAt time.Time) *Result {
	r.handoffPhysicalID = response.Attempt.ID
	if r.pendingOutput == response {
		r.pendingOutput = nil
	}
	// Capture only this value; a method value would retain the entire request
	// execution (including its body and candidate state) for the stream lifetime.
	signal := r.firstGuardSignal
	session := &deliverySession{
		service: r.service, ctx: r.ctx, physicalCtx: r.physicalCallCtx, response: response, credential: credential, lease: lease, admission: r.admission,
		physicalBudget: r.requestBudget, imageFacts: r.imageFacts, textFacts: r.textFacts,
		firstToken: r.firstToken, timing: r.timing, attempts: r.failureAttempts, egressTrace: r.egressTrace, finishGuardOutcome: func(rescued bool) { finishGuardOutcome(signal, rescued) },
		plan: deliveryPlan{route: r.route, operation: r.operation, guard: r.holdCfg, guardEnabled: r.qualityHoldEnabled, audit: r.auditBase,
			usageSource: r.usageSource, pricingModel: r.pricingModel, storeResponse: r.supportsStoredResponses && historydomain.ResponseStorageRequested(r.input.StoreResponse),
			promptCacheKey: r.ownershipPromptCacheKey, reasoningReplayKey: r.reasoningReplayKey,
			requestID: r.input.RequestID, clientKeyID: r.input.ClientKey.ID, streaming: r.input.Streaming, startedAt: r.startedAt, handedOffAt: time.Now()},
	}
	r.handedOff = true
	return session.result(upstreamStartedAt)
}
func (r *responseExecution) markDegradedEgress(response *provider.Response) uint64 {
	nodeID := response.Attempt.Path.NodeID
	if nodeID == 0 {
		return 0
	}
	if _, exists := r.degradedNodes[nodeID]; !exists {
		r.degradedNodes[nodeID] = struct{}{}
		r.physicalCallCtx = portphysical.WithNodeExclusions(r.physicalCallCtx, r.degradedNodes)
	}
	return nodeID
}
func (r *responseExecution) noteGuardSignal(signal GuardSignal) {
	guardStats.recordSignal(signal)
	if r.firstGuardSignal == "" {
		r.firstGuardSignal = signal
		guardStats.recordRequestSignal(signal)
	}
}
func (r *responseExecution) finishGuardOutcome(rescued bool) {
	finishGuardOutcome(r.firstGuardSignal, rescued)
}

func finishGuardOutcome(signal GuardSignal, rescued bool) {
	if signal != "" {
		guardStats.recordOutcome(signal, rescued)
	}
}
