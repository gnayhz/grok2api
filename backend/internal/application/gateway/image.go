package gateway

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"net/http"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/retryafter"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// ImageGenerationInput 表示图片生成用例已经完成协议校验后的输入。
type ImageGenerationInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
	Method         string
	Path           string
	Headers        map[string][]string
}

// ImageEditInput 表示图片编辑用例已经完成协议校验后的输入。
type ImageEditInput struct {
	RequestID      string
	ClientKey      clientkey.Key
	PublicModel    string
	Prompt         string
	ImageURLs      []string
	Count          int
	Size           string
	AspectRatio    string
	Resolution     string
	Quality        string
	ResponseFormat string
	Streaming      bool
	PartialImages  int
	Method         string
	Path           string
	Headers        map[string][]string
}

type imageProviderSupport func(accountdomain.Provider) bool

type imageExecution func(context.Context, accountdomain.Provider, accountdomain.Credential, string, func(provider.ImageGenerationObservation)) (*provider.Response, error)

// GenerateImage 选择支持图片生成的路由和账号，并返回可统一审计的上游响应。
func (s *Service) GenerateImage(ctx context.Context, input ImageGenerationInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImage, modeldomain.CapabilityImage, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.ImageGeneration(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string, observe func(provider.ImageGenerationObservation)) (*provider.Response, error) {
		adapter, ok := s.providers.ImageGeneration(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.GenerateImage(executionCtx, provider.ImageGenerationRequest{Observe: observe,
			Credential: credential, Model: upstream, Prompt: input.Prompt, Count: input.Count,
			Size: input.Size, AspectRatio: input.AspectRatio, Resolution: input.Resolution, Quality: input.Quality,
			ResponseFormat: input.ResponseFormat, Streaming: input.Streaming, PartialImages: input.PartialImages,
		})
	}, input.Streaming, input.Resolution, input.Quality, input.Count, 0, input.Method, input.Path, input.Headers)
}

// EditImage 选择支持图片编辑的路由和账号，并返回可统一审计的上游响应。
func (s *Service) EditImage(ctx context.Context, input ImageEditInput) (*Result, error) {
	return s.executeImage(ctx, input.RequestID, input.ClientKey, input.PublicModel, audit.OperationImageEdit, modeldomain.CapabilityImageEdit, func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.ImageEdit(providerValue)
		return ok
	}, func(executionCtx context.Context, providerValue accountdomain.Provider, credential accountdomain.Credential, upstream string, observe func(provider.ImageGenerationObservation)) (*provider.Response, error) {
		adapter, ok := s.providers.ImageEdit(providerValue)
		if !ok {
			return nil, ErrNoAvailableAccount
		}
		return adapter.EditImage(executionCtx, provider.ImageEditRequest{Observe: observe,
			Credential: credential, Model: upstream, Prompt: input.Prompt,
			ImageURLs: input.ImageURLs, Count: input.Count, Size: input.Size, AspectRatio: input.AspectRatio,
			Resolution: input.Resolution, Quality: input.Quality, ResponseFormat: input.ResponseFormat,
			Streaming: input.Streaming, PartialImages: input.PartialImages,
		})
	}, input.Streaming, input.Resolution, input.Quality, input.Count, len(input.ImageURLs), input.Method, input.Path, input.Headers)
}

func (s *Service) executeImage(
	ctx context.Context,
	requestID string,
	key clientkey.Key,
	publicModel string,
	operation audit.Operation,
	capability modeldomain.Capability,
	supports imageProviderSupport,
	execute imageExecution,
	streaming bool,
	resolution string,
	quality string,
	requestedCount int,
	inputImageCount int,
	method string,
	path string,
	headers map[string][]string,
) (*Result, error) {
	ctx, egressTrace := portphysical.WithTrace(ctx)
	startedAt := time.Now()
	eventID := s.newAuditEventID()
	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		// 候选为空出口统一消歧（round 60：image/video/voice-ws 三处与
		// resolve/voice 同型漏判——路由在但池无该 Provider 账号时误报 404）。
		return nil, s.distinguishMissingOrNoAccount(ctx, publicModel, err)
	}
	route, preselectedSession, err := s.selectSchedulableMediaRoute(ctx, routes, key, capability, true, supports)
	if err != nil {
		// Preserve the established failure-audit path when every eligible target
		// is currently unschedulable. The request loop will reproduce the
		// selection error for the representative route after auditBase exists.
		route, err = s.selectMediaRoute(routes, key, capability, supports)
		if err != nil {
			return nil, err
		}
		preselectedSession = nil
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: requestID, ClientKeyID: key.ID, ClientKeyName: key.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: streaming,
		RequestMethod: method, RequestPath: path, RequestHeaders: headers,
	}
	if operation == audit.OperationImageEdit {
		auditBase.MediaInputImages = int64(max(0, inputImageCount))
	}
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	ctx = attemptmeta.WithRequest(ctx, eventID, 0, "", nil)
	ctx = s.startPhysicalTrace(ctx, string(route.Provider), string(operation))
	requestBudget := inferencedomain.NewAttemptBudget(portphysical.MaxPhysicalCalls)
	ctx = portphysical.WithPhysicalCallBudget(ctx, requestBudget)
	// A 128 MiB image JSON may briefly retain its old array while growing. It
	// uses the process pool and never replaces a stricter caller-owned budget.
	ctx = responsebuffer.WithRequestLimit(ctx, 256<<20)
	handedOff := false
	defer func() {
		if !handedOff {
			requestBudget.Close()
		}
	}()
	generation := &imageGeneration{}
	writeFailureAudit := func(statusCode int, errorCode string, credential *accountdomain.Credential) {
		record := auditBase
		record.StatusCode = statusCode
		record.ErrorCode = errorCode
		facts, outcome := generation.snapshot()
		record.GenerationOutcome, record.UpstreamStatusCode = outcome, facts.UpstreamStatus
		record.AdmissionOutcome, record.DeliveryOutcome = "not_admitted", "not_started"
		record.HistoryCommit, record.ProviderStateCommit, record.OwnershipCommit, record.QualityReceipt = "not_required", "not_required", "not_required", "not_required"
		record.PhysicalReceipt = s.finishPhysicalReceipt(ctx)
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.CreatedAt = time.Now().UTC()
		if credential != nil {
			accountID := credential.ID
			record.AccountID = &accountID
			record.AccountName = credential.Name
		}
		applyAuditEgress(&record, egressTrace, route.Provider)
		persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
		defer cancel()
		if auditErr := s.audits.Create(persistCtx, record); auditErr != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", auditErr)
		}
	}
	pricingModel := s.providers.PricingModel(route.Provider, route.UpstreamModel)
	pricingResolution, pricingQuality := resolution, quality
	if route.Provider == accountdomain.ProviderWeb && operation == audit.OperationImage {
		// Grok Web Imagine selects the product through the catalog model and
		// only forwards aspect_ratio/n. Do not reserve or record a price tier
		// derived from Console-only resolution/quality compatibility fields.
		pricingResolution, pricingQuality = "", ""
	}
	var reservation audit.PricingResult
	var priced bool
	switch operation {
	case audit.OperationImage:
		reservation, priced = audit.EstimateOfficialImageCost(pricingModel, pricingResolution, pricingQuality, requestedCount)
	case audit.OperationImageEdit:
		reservation, priced = audit.EstimateOfficialImageEditCost(pricingModel, pricingResolution, pricingQuality, requestedCount, inputImageCount)
	}
	reserved := false
	if priced {
		reserved, err = s.clientKeys.ReserveBilling(ctx, key, eventID, reservation.CostInUSDTicks, mediaBillingReservationTTL)
		if err != nil {
			return nil, err
		}
	}
	finalizationOwnsReservation := false
	defer func() {
		if reserved && !finalizationOwnsReservation {
			s.cancelBillingReservation(eventID)
		}
	}()
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
	quotaRefreshGroup := s.providers.QuotaRefreshGroup(route.Provider, route.UpstreamModel)
	attemptPolicy := newRoutingAttemptPolicy(int(s.maxAttempts.Load()))
	excluded := make(map[uint64]bool)
	selection := preselectedSession
	var lease *selector.Lease
	defer func() {
		if !handedOff {
			lease.Release()
		}
	}()
	var credential accountdomain.Credential
	var response *provider.Response
	handoffError := ""
	var lastCredentialFailure *accountdomain.Credential
	var lastCredentialError error
	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if selection == nil {
			selection, err = s.selector.BeginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, key.AccountScope())
		}
		if err == nil {
			lease, err = selection.Acquire(ctx, excluded, false)
		}
		if err != nil {
			errorCode := "upstream_unavailable"
			var selectionFailure *selector.SelectionUnavailableError
			if errors.As(err, &selectionFailure) {
				errorCode = selectionFailure.Code()
			}
			writeFailureAudit(http.StatusServiceUnavailable, errorCode, lastCredentialFailure)
			return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, err)
		}
		excluded[lease.Credential.ID] = true
		credential, err = s.accounts.EnsureCredential(ctx, lease.Credential, false)
		if err != nil {
			s.logger.Error("image_credential_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", lease.Credential.ID, "error", err)
			failedCredential := lease.Credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = err
			lease.Release()
			continue
		}
		lease.MarkSelectorUpstreamStarted()
		attemptCtx := attemptmeta.WithAccount(ctx, credential.ID, string(route.Provider), route.UpstreamModel)
		generation = &imageGeneration{}
		response, err = execute(attemptCtx, route.Provider, credential, route.UpstreamModel, generation.observe)
		if err != nil {
			s.logger.Error("image_upstream_failed", "event_id", eventID, "request_id", requestID, "model", externalModel, "provider", route.Provider, "account_id", credential.ID, "error", err)
			if response != nil && response.Body != nil {
				_ = response.Body.Close()
			}
			var validation *inferencedomain.RequestValidationError
			if errors.As(err, &validation) {
				lease.SkipSelectorObservation()
				lease.Release()
				writeFailureAudit(http.StatusBadRequest, validation.Code, nil)
				return nil, err
			}
			facts, _ := generation.snapshot()
			if facts.OutputImages > 0 {
				handoffError = "image_generation_incomplete"
				if provider.IsMediaPostProcessingError(err) {
					handoffError = "media_postprocessing_failed"
				}
				response = jsonMediaResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{"code": handoffError, "type": "server_error", "message": "上游已生成图片，但未能完成图片响应处理"}})
				break
			}
			if errors.Is(err, inferencedomain.ErrAttemptBudget) {
				lease.SkipSelectorObservation()
				lease.Release()
				writeFailureAudit(http.StatusServiceUnavailable, "physical_attempt_limit", &credential)
				return nil, &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "physical_attempt_limit", PublicMessage: "上游尝试次数达到安全上限", Cause: err}
			}
			if isSSOCredentialRejected(err, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failedCredential := credential
				lastCredentialFailure = &failedCredential
				lastCredentialError = provider.ErrUnauthorized
				lease.Release()
				continue
			}
			// Network, resource and protocol failures do not establish an account
			// health restriction. Only explicit refusal paths below do so.
			lease.SkipSelectorObservation()
			lease.Release()
			errorCode := "upstream_unavailable"
			if provider.IsMediaPostProcessingError(err) {
				errorCode = "media_postprocessing_failed"
			}
			writeFailureAudit(http.StatusBadGateway, errorCode, &credential)
			return nil, err
		}
		if facts, _ := generation.snapshot(); facts.OutputImages > 0 {
			break
		}
		if response.StatusCode == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO {
			_, _ = readRetryableBody(response.Body)
			s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
			failedCredential := credential
			lastCredentialFailure = &failedCredential
			lastCredentialError = provider.ErrUnauthorized
			response = nil
			lease.Release()
			continue
		}
		if s.providers.RetryForbiddenAsEgress(credential.Provider) && response.StatusCode == http.StatusForbidden && !response.PolicyForbidden && attempt == 0 && attemptPolicy.hasNext(attempt) {
			_, _ = readRetryableBody(response.Body)
			delete(excluded, credential.ID)
			if selection != nil {
				selection.RetryAccount(credential.ID)
			}
			lease.Release()
			continue
		}
		if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && response.StatusCode == http.StatusTooManyRequests && lease.QuotaMode != "" {
			retryAfter := retryafter.Header(response.Header.Get("Retry-After"), time.Now().UTC())
			exhausted, reconcileErr := s.accounts.ReconcileWebRateLimit(ctx, credential.ID, lease.QuotaMode, retryAfter)
			s.selector.MarkQuotaStateChanged(credential.Provider, credential.ID)
			if reconcileErr != nil || !exhausted {
				s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			}
			if attemptPolicy.hasNext(attempt) {
				_, _ = readRetryableBody(response.Body)
				lease.Release()
				continue
			}
		}
		// 非 remote-window 的 402/429 与所有 5xx:与 voice/video 路径对齐——账号
		// 侧限流同样 MarkFailure 冷却(否则限流账号持续被选中挨打), 5xx 可重试。
		// 此前图片是三个媒体入口中唯一缺这层的, 上游限流时成功率显著更低。
		if response.StatusCode == http.StatusPaymentRequired || response.StatusCode == http.StatusTooManyRequests {
			retryAfter := retryafter.Header(response.Header.Get("Retry-After"), time.Now().UTC())
			s.selector.MarkFailure(ctx, credential, response.StatusCode, retryAfter)
			if attemptPolicy.hasNext(attempt) {
				_, _ = readRetryableBody(response.Body)
				lease.Release()
				continue
			}
		}
		if response.StatusCode >= http.StatusInternalServerError && attemptPolicy.hasNext(attempt) {
			_, _ = readRetryableBody(response.Body)
			lease.Release()
			continue
		}
		break
	}
	if response == nil {
		writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
		if lastCredentialError == nil {
			lastCredentialError = ErrNoAvailableAccount
		}
		return nil, fmt.Errorf("%w: %w", ErrNoAvailableAccount, lastCredentialError)
	}
	effectiveQuotaMode := lease.QuotaMode
	accountID := credential.ID
	handoff := &mediaHandoff{ctx: ctx, response: response, budget: requestBudget,
		release: func() {
			facts, _ := generation.snapshot()
			if facts.Completed {
				lease.CompleteSelectorObservation(true)
			} else if ctx.Err() != nil || provider.IsMediaPostProcessingError(err) {
				lease.SkipSelectorObservation()
			} else {
				lease.CompleteSelectorObservation(false)
			}
			lease.Release()
		},
		finish: func(stats DeliveryStats, admitted bool, errorCode string) {
			if handoffError != "" && (errorCode == "" || errorCode == "stream_closed") {
				errorCode = handoffError
			}
			budget := newFinalizationBudget(string(operation), string(route.Provider))
			record := auditBase
			record.AccountID, record.AccountName = &accountID, credential.Name
			applyMediaDelivery(&record, stats, admitted, response.StatusCode, errorCode)
			generated, outcome := generation.snapshot()
			record.GenerationOutcome = outcome
			if generated.UpstreamStatus != 0 {
				record.UpstreamStatusCode = generated.UpstreamStatus
			}
			record.PhysicalReceipt = s.finishPhysicalReceipt(ctx)
			record.DurationMS, record.CreatedAt = time.Since(startedAt).Milliseconds(), time.Now().UTC()
			applyAuditEgress(&record, egressTrace, route.Provider)
			record.MediaOutputImages = int64(generated.OutputImages)
			if generated.OutputImages > 0 {
				var pricing audit.PricingResult
				var priced bool
				switch operation {
				case audit.OperationImage:
					pricing, priced = audit.EstimateOfficialImageCost(pricingModel, pricingResolution, pricingQuality, generated.OutputImages)
				case audit.OperationImageEdit:
					pricing, priced = audit.EstimateOfficialImageEditCost(pricingModel, pricingResolution, pricingQuality, generated.OutputImages, inputImageCount)
				}
				if priced {
					record.EstimatedCostInUSDTicks, record.PricingModel, record.PricingVersion = pricing.CostInUSDTicks, pricing.Model, audit.OfficialPricingAsOf
				}
			}
			s.finishImageQuota(budget, route.Provider, eventID, accountID, effectiveQuotaMode, lease.QuotaSnapshotVersion, quotaRefreshGroup, generated.QuotaUnits)
			if err := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error { return s.audits.Create(stageCtx, record) }); err != nil {
				s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", requestID, "error", err)
			}
		},
	}
	finalizationOwnsReservation, handedOff = true, true
	result := handoff.result()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		result.CommitCompletion = func(Completion) error {
			_, outcome := generation.snapshot()
			if outcome != "completed" {
				return &UpstreamFailure{HTTPStatus: http.StatusBadGateway, Code: "image_generation_incomplete", PublicMessage: "上游没有返回完整的图片结果"}
			}
			return nil
		}
	}
	return result, nil
}

// finishImageQuota is shared by REST images and image generation encoded in
// text protocols. Its input is confirmed generation, independent of delivery.
func (s *Service) finishImageQuota(budget finalizationBudget, kind accountdomain.Provider, eventID string, accountID uint64, mode string, snapshotVersion uint64, quotaRefreshGroup string, units int) {
	quotaKind, _ := s.providers.QuotaKind(kind)
	refreshMode, decrementMode, availabilityMode := quotaFinalizationModes(mode, quotaRefreshGroup)
	if units > 0 && quotaKind == provider.QuotaRemoteWindow && refreshMode != "" {
		s.finishQuotaConsumption(budget, accountdomain.QuotaConsumption{EventID: "quota_" + eventID, AccountID: accountID, Mode: decrementMode, SnapshotVersion: snapshotVersion, Units: units})
		s.accounts.QueueQuotaRefresh(accountID, refreshMode)
		if availabilityMode != "" && availabilityMode != refreshMode {
			s.accounts.QueueQuotaRefresh(accountID, availabilityMode)
		}
	}
}
