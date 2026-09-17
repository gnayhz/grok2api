package gateway

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// deliveryPlan freezes the metadata needed after handoff. It deliberately does
// not retain the incoming body or the mutable retry/selection orchestration.
type deliveryPlan struct {
	route                              modeldomain.Route
	operation                          audit.Operation
	guard                              QualityRetryRuntime
	guardEnabled                       bool
	audit                              audit.Record
	usageSource                        audit.UsageSource
	pricingModel                       string
	storeResponse                      bool
	promptCacheKey, reasoningReplayKey string
	requestID                          string
	clientKeyID                        uint64
	streaming                          bool
	startedAt                          time.Time
}

// deliverySession owns the accepted response, release and once-only terminal
// record. Cancellation, explicit finalization and body close share this owner.
type deliverySession struct {
	service            *Service
	ctx, physicalCtx   context.Context
	physicalBudget     *inferencedomain.AttemptBudget
	plan               deliveryPlan
	response           *provider.Response
	credential         accountdomain.Credential
	lease              *selector.Lease
	admission          *admission
	firstToken         *firstTokenTimer
	timing             *generationTiming
	attempts           *failureAttemptRecorder
	egressTrace        *portphysical.Trace
	finishGuardOutcome func(bool)
	once               sync.Once
	deliveryMu         sync.Mutex
	delivery           DeliveryStats
	deliverySet        bool
	completion         completionState
	imageFacts         *imageGeneration
	textFacts          *textGeneration
	lifecycleMu        sync.Mutex
	deliveryClaimed    bool
	terminal           bool
	admissionOutcome   string
}

func (d *deliverySession) recordDelivery(stats DeliveryStats) {
	d.deliveryMu.Lock()
	defer d.deliveryMu.Unlock()
	d.delivery, d.deliverySet = stats, true
}

func (d *deliverySession) finalize(usage Usage, responseID, errorCode string) {
	d.once.Do(func() {
		defer d.physicalBudget.Close()
		d.lifecycleMu.Lock()
		d.terminal = true
		admissionOutcome := d.admissionOutcome
		d.lifecycleMu.Unlock()
		if admissionOutcome == "" {
			admissionOutcome = "not_admitted"
		}
		usage, responseID, errorCode, completion := d.finishCompletion(usage, responseID, errorCode)
		s := d.service
		accountID := d.credential.ID
		defer d.admission.close()
		// HTTP 状态码保留线上真实值；流在 2xx 响应头之后失败时由 errorCode
		// 决定最终结果，避免把协议状态与业务结果混为一谈。
		successful := auditRequestSucceeded(d.response.StatusCode, errorCode)
		// Capture delivery independently from archival side effects. A failed
		// exchange write cannot turn a completed stream into an interrupted one.
		deliverySucceeded, deliveryError := successful, errorCode
		// Complete any producer and physical body before snapshotting facts.
		// A canceled read still has to publish its final received bytes/close.
		_ = d.response.Body.Close()
		usage, completion.generation = d.textFacts.finish(d.response, usage, completion.generation)
		if d.textFacts != nil && responseID == "" {
			if observed := d.textFacts.entries[d.textFacts.selected]; observed != nil {
				responseID = observed.responseID
			}
		}
		generationSucceeded := completion.generation == "completed"
		var images provider.ImageGenerationObservation
		if d.imageFacts != nil {
			images, completion.generation = d.imageFacts.snapshot()
			generationSucceeded = images.Completed
		}
		qualityReceipt := "not_required"
		if recorder := s.qualityEvents.Load(); recorder != nil && recorder.value != nil {
			if d.plan.guardEnabled {
				qualityReceipt = "committed"
			}
		}
		d.lease.CompleteSelectorObservation(generationSucceeded)
		d.lease.Release()
		if usage.Reported {
			portphysical.ObservePhysicalUsage(d.physicalCtx, d.response.Attempt.ID, physicalUsage(usage))
		}
		physicalReceipt := s.finishPhysicalReceipt(d.physicalCtx)
		if d.plan.guardEnabled {
			outcome := QualityObservedInterrupted
			if deliverySucceeded {
				outcome = QualityObservedCompleted
			} else if deliveryError == "client_disconnected" || deliveryError == "request_canceled" {
				outcome = QualityObservedCanceled
			}
			if eventErr := s.recordQualityEvent(d.ctx, QualityObservation{Attempt: d.response.Attempt, At: time.Now().UTC(),
				AccountID: d.credential.ID, NodeID: d.response.Attempt.Path.NodeID, Provider: string(d.plan.route.Provider), Outcome: outcome, ErrorCode: deliveryError}, 0); eventErr != nil {
				s.logger.Error("quality_completion_event_failed", "attempt_id", d.response.Attempt.ID, "error", eventErr)
				qualityReceipt = "failed"
			}
		}
		// End capture before accepting optional cache updates. Legacy capture may
		// commit only after EOF and Close; Discard must not erase it first.
		if successful && d.response.AcceptOutput != nil {
			d.response.AcceptOutput()
		}
		if d.response.DiscardOutput != nil {
			d.response.DiscardOutput()
		}
		d.finishGuardOutcome(successful)
		budget := newFinalizationBudget(string(d.plan.operation), string(d.plan.route.Provider))
		if isUpstreamStreamFailure(errorCode) {
			status, retryAfter := streamFailureHealthPenalty(errorCode, usage, d.plan.guard.IdleAccountCooldown)
			if err := budget.run("account_health", finalizationHealthBudget, func(stageCtx context.Context) error {
				return s.selector.MarkFailureAfterSuccess(stageCtx, d.credential, status, retryAfter)
			}); err != nil {
				s.logger.Warn("stream_failure_health_write_failed", "account_id", d.credential.ID, "provider", d.credential.Provider, "error", err)
			}
		}
		now := time.Now().UTC()
		record := d.plan.audit
		record.GenerationUsages = d.textFacts.details()
		record.ResponseID = responseID
		record.ProviderStateCommit = completion.providerState
		record.AdmissionOutcome = admissionOutcome
		record.GenerationOutcome = completion.generation
		record.HistoryCommit = completion.history
		record.OwnershipCommit = completion.ownership
		record.PhysicalReceipt, record.QualityReceipt = physicalReceipt, qualityReceipt
		record.DeliveryOutcome = "failed"
		if deliverySucceeded {
			record.DeliveryOutcome = "completed"
		} else if deliveryError == "client_disconnected" || deliveryError == "request_canceled" {
			record.DeliveryOutcome = "canceled"
		}
		record.HistoryOutcome = d.response.HistoryOutcome
		record.HistoryScopeHash = d.response.HistoryScopeHash
		record.HistoryGeneration = d.response.HistoryGeneration
		record.HistoryRestoredItems = d.response.HistoryRestoredItems
		record.HistoryNormalizer = d.response.HistoryNormalizer
		if usage.Reported {
			record.UsageSource = d.plan.usageSource
		}
		record.AccountID = &accountID
		record.AccountName = d.credential.Name
		record.StatusCode = d.response.StatusCode
		record.UpstreamStatusCode = d.response.StatusCode
		d.deliveryMu.Lock()
		if d.deliverySet && d.delivery.StatusCode != 0 {
			record.StatusCode = d.delivery.StatusCode
		}
		d.deliveryMu.Unlock()
		if errorCode == "client_disconnected" {
			// 响应头可能已是 200，但请求结局是客户端断开。用 499
			// 避免审计列表把取消当成成功（与 nginx 约定一致）。
			record.StatusCode = 499
		}
		record.QualityFailOpen = false
		record.InputTokens = usage.InputTokens
		record.CachedInputTokens = usage.CachedInputTokens
		record.CachedInputTokensReported = cacheUsagePresence(usage.Reported, usage.CachedInputTokensReported, usage.CachedInputTokens)
		record.OutputTokens = usage.OutputTokens
		record.ReasoningTokens = usage.ReasoningTokens
		record.TotalTokens = usage.TotalTokens
		record.CostInUSDTicks = usage.CostInUSDTicks
		if d.imageFacts != nil {
			record.MediaOutputImages = int64(images.OutputImages)
			if images.UpstreamStatus != 0 {
				record.UpstreamStatusCode = images.UpstreamStatus
			}
			if images.OutputImages > 0 {
				if price, ok := audit.EstimateOfficialImageCost(d.plan.pricingModel, "", "", images.OutputImages); ok {
					record.EstimatedCostInUSDTicks, record.PricingModel, record.PricingVersion = price.CostInUSDTicks, price.Model, audit.OfficialPricingAsOf
				}
			}
		} else if price, ok := audit.EstimateOfficialCost(d.plan.pricingModel, usage.InputTokens, usage.CachedInputTokens, usage.OutputTokens, usage.ContextInputTokens); ok {
			record.EstimatedCostInUSDTicks, record.PricingModel, record.PricingVersion = price.CostInUSDTicks, price.Model, audit.OfficialPricingAsOf
		}
		record.NumSourcesUsed = usage.NumSourcesUsed
		record.NumServerSideToolsUsed = usage.NumServerSideToolsUsed
		record.ContextInputTokens = usage.ContextInputTokens
		record.ContextOutputTokens = usage.ContextOutputTokens
		if successful && d.plan.streaming {
			record.FirstTokenMS = d.firstToken.milliseconds()
			// TTFT 直方图:审计表的 FirstTokenMS 是毫秒级落库值,排障要查库且
			// 失败流被归一化丢弃;perfmetrics 直方图随指标面暴露,链路优化效果
			// 可实时观测。打点仅请求收尾一次,开销百 ns 级。
			if record.FirstTokenMS != nil {
				perfmetrics.Default.ObserveDuration("first_token_us", perfmetrics.Labels{Subsystem: "gateway", Provider: string(d.plan.route.Provider)}, time.Duration(*record.FirstTokenMS)*time.Millisecond)
			}
		}
		d.deliveryMu.Lock()
		if d.deliverySet {
			record.DeliveredEvents = d.delivery.Events
			record.DeliveredBytes = d.delivery.Bytes
		}
		d.deliveryMu.Unlock()
		record.DurationMS = time.Since(d.plan.startedAt).Milliseconds()
		record.ErrorCode = errorCode
		attempts := d.attempts.snapshot()
		if !successful || len(attempts) > 0 {
			record.Attempts = attempts
		}
		record.CreatedAt = now
		applyAuditEgress(&record, d.egressTrace, d.plan.route.Provider)
		if d.imageFacts != nil {
			s.finishImageQuota(budget, d.credential.Provider, record.EventID, accountID, d.lease.QuotaMode, d.lease.QuotaSnapshotVersion, s.providers.QuotaRefreshGroup(d.credential.Provider, d.plan.route.UpstreamModel), images.QuotaUnits)
		} else {
			s.finishTextQuotas(budget, d.textFacts)
		}
		if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
			return s.audits.Create(stageCtx, record)
		}); err != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", d.plan.requestID, "error", err)
		}
		if usage.ResponseModel != "" {
			_ = budget.run("observed_model", finalizationMetadataBudget, func(stageCtx context.Context) error {
				return s.accounts.ObserveResponseModel(stageCtx, accountID, usage.ResponseModel)
			})
		}
		outcome := "failed"
		if successful {
			outcome = "success"
		}
		d.timing.finish(s.logger, outcome)
	})
}

func (d *deliverySession) result(upstreamStartedAt time.Time) *Result {
	d.response.Body = &firstByteReadCloser{ReadCloser: d.response.Body, mark: d.timing.markFirstBody}
	d.response.Body = d.lease.OwnBody(d.response.Body)
	stopRelease := context.AfterFunc(d.ctx, d.cancelDelivery)
	finalize := func(usage Usage, responseID, code string) {
		stopRelease()
		d.finalize(usage, responseID, code)
	}
	var markFirstToken func()
	if d.firstToken != nil {
		markFirstToken = d.firstToken.mark
	}
	return &Result{StatusCode: d.response.StatusCode, Status: d.response.Status, Header: d.response.Header,
		BeginDelivery:    d.beginDelivery,
		CommitDelivery:   d.claimDelivery,
		CommitCompletion: d.completionCommitter(),
		Body:             &finalizingBody{budget: responsebuffer.FromContext(d.ctx), ReadCloser: selector.NewAdmissionBody(d.response.Body, d.claimDelivery), finalize: func() { finalize(Usage{}, "", "stream_closed") }},
		MarkFirstToken:   markFirstToken, RecordDelivery: d.recordDelivery, Finalize: finalize,
		RecordStreamFailure: func(diagnostic StreamFailureDiagnostic) {
			d.attempts.captureStreamFailure(d.credential, upstreamStartedAt, d.response, diagnostic)
		},
	}
}

// completionCommitter is present for every successful generation, including
// Providers without a local history callback. It also records protocol success.
func (d *deliverySession) completionCommitter() func(Completion) error {
	if d.response.StatusCode < 200 || d.response.StatusCode >= 300 {
		return nil
	}
	return d.commitCompletion
}

func (d *deliverySession) beginDelivery() error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.terminal {
		return context.Canceled
	}
	d.deliveryClaimed = true
	return nil
}

func (d *deliverySession) claimDelivery() error {
	d.lifecycleMu.Lock()
	defer d.lifecycleMu.Unlock()
	if d.terminal {
		return context.Canceled
	}
	if err := d.admission.commit(); err != nil {
		if d.admissionOutcome != "admitted" {
			d.admissionOutcome = "failed"
		}
		return err
	}
	// A rejected implicit claim leaves cancellation responsible for cleanup.
	// Explicit BeginDelivery callers already own finalization, even on error.
	d.deliveryClaimed = true
	d.admissionOutcome = "admitted"
	return nil
}

func (d *deliverySession) cancelDelivery() {
	d.lifecycleMu.Lock()
	claimed, terminal := d.deliveryClaimed, d.terminal
	if !claimed {
		d.terminal = true
	}
	d.lifecycleMu.Unlock()
	if terminal {
		return
	}
	// Closing this inner body interrupts reads without invoking the transport's
	// Close fallback. The active transport retains finalization ownership.
	_ = d.response.Body.Close()
	d.lease.Release()
	if claimed {
		return
	}
	code := "request_canceled"
	if errors.Is(d.admission.failure(), errAdmissionDeadline) {
		code = "quality_admission_timeout"
	}
	d.finalize(Usage{}, "", code)
}
