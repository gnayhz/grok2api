package gateway

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	portphysical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type VoiceWebSocketInput struct {
	RequestID   string
	ClientKey   clientkey.Key
	PublicModel string
	// Path is one of /realtime or /stt.
	Path  string
	Query url.Values
}

type VoiceWebSocketSession struct {
	Conn          provider.VoiceWebSocketConn
	BeginDelivery func() error
	Finalize      func(VoiceWebSocketOutcome)
	RequestID     string
	PublicModel   string
	Provider      accountdomain.Provider
	AccountID     uint64
	AccountName   string
	Operation     audit.Operation
	Capability    modeldomain.Capability
}

type VoiceWebSocketOutcome struct {
	ErrorCode                       string
	UpstreamFailed                  bool
	ClientUpgraded                  bool
	DeliveredBytes, DeliveredEvents int64
}

// OpenVoiceWebSocket selects a Console account and dials the upstream voice websocket.
func (s *Service) OpenVoiceWebSocket(ctx context.Context, input VoiceWebSocketInput) (*VoiceWebSocketSession, error) {
	pathValue := strings.TrimSpace(input.Path)
	capability, operation, defaultModel, err := voiceWebSocketRoute(pathValue)
	if err != nil {
		return nil, err
	}
	publicModel := strings.TrimSpace(input.PublicModel)
	if publicModel == "" {
		publicModel = defaultModel
	}

	ctx, egressTrace := portphysical.WithTrace(ctx)
	startedAt := time.Now()
	eventID := s.newAuditEventID()

	routes, err := s.models.GetByPublicIDCandidates(ctx, publicModel)
	if err != nil {
		// 候选为空出口统一消歧（round 60，同 image/video）。
		return nil, s.distinguishMissingOrNoAccount(ctx, publicModel, err)
	}
	supportsVoiceWebSocket := func(providerValue accountdomain.Provider) bool {
		_, ok := s.providers.VoiceWebSocket(providerValue)
		return ok
	}
	route, preselectedSession, err := s.selectSchedulableMediaRoute(ctx, routes, input.ClientKey, capability, true, supportsVoiceWebSocket)
	if err != nil {
		// Preserve selection-failure audits when every eligible target is cooling
		// or exhausted. The regular request loop will reproduce the error.
		route, err = s.selectMediaRoute(routes, input.ClientKey, capability, supportsVoiceWebSocket)
		if err != nil {
			return nil, err
		}
		preselectedSession = nil
	}
	externalModel := modeldomain.ExternalPublicID(route.Provider, route.PublicID)
	auditBase := audit.Record{
		EventID: eventID, RequestID: input.RequestID, ClientKeyID: input.ClientKey.ID, ClientKeyName: input.ClientKey.Name,
		ClientIP:     requestmeta.ClientIP(ctx),
		ModelRouteID: route.ID, ModelPublicID: externalModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(route.Provider, route.UpstreamModel),
		Provider: string(route.Provider), Operation: operation, UsageSource: audit.UsageSourceNone, Streaming: true,
	}
	if err := s.checkLedgerReady(); err != nil {
		return nil, err
	}
	ctx = attemptmeta.WithRequest(ctx, eventID, 0, "", nil)
	ctx = s.startPhysicalTrace(ctx, string(route.Provider), string(operation))
	// Routing attempts bound account selection. The shared physical budget
	// includes cold DPoP preparation, its retry and every WS handshake.
	requestBudget := inferencedomain.NewAttemptBudget(portphysical.MaxPhysicalCalls)
	ctx = portphysical.WithPhysicalCallBudget(ctx, requestBudget)
	ctx, cancelExecution := context.WithCancel(ctx)
	handedOff := false
	defer func() {
		if !handedOff {
			cancelExecution()
			requestBudget.Close()
		}
	}()
	generation := &voiceGeneration{}
	quotaMode := s.providers.QuotaMode(route.Provider, route.UpstreamModel)
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
	var lastCredentialFailure *accountdomain.Credential
	var lastErr error

	writeFailureAudit := func(statusCode int, errorCode string, cred *accountdomain.Credential) {
		record := auditBase
		record.StatusCode = statusCode
		record.AdmissionOutcome, record.GenerationOutcome, record.DeliveryOutcome = "not_admitted", "not_started", "not_started"
		record.HistoryCommit, record.ProviderStateCommit, record.OwnershipCommit, record.QualityReceipt = "not_required", "not_required", "not_required", "not_required"
		record.PhysicalReceipt = s.finishPhysicalReceipt(ctx)
		record.ErrorCode = errorCode
		record.DurationMS = time.Since(startedAt).Milliseconds()
		record.CreatedAt = time.Now().UTC()
		if cred != nil {
			accountID := cred.ID
			record.AccountID = &accountID
			record.AccountName = cred.Name
		}
		applyAuditEgress(&record, egressTrace, route.Provider)
		persistCtx, cancel := context.WithTimeout(context.Background(), finalizationTimeout)
		defer cancel()
		if auditErr := s.audits.Create(persistCtx, record); auditErr != nil {
			s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", auditErr)
		}
	}

	adapter, ok := s.providers.VoiceWebSocket(route.Provider)
	if !ok {
		return nil, ErrNoAvailableAccount
	}
	prepared, err := adapter.PrepareVoiceWebSocket(provider.VoiceWebSocketRequest{Path: pathValue, Model: route.UpstreamModel, Query: input.Query, Observe: generation.observe})
	if err != nil {
		var validation *inferencedomain.RequestValidationError
		if errors.As(err, &validation) {
			writeFailureAudit(http.StatusBadRequest, validation.Code, nil)
		}
		return nil, err
	}

	for attempt := 0; attemptPolicy.allows(attempt); attempt++ {
		if selection == nil {
			selection, err = s.selector.BeginSelectionSessionForKey(ctx, route.Provider, route.ID, route.UpstreamModel, quotaMode, "", excluded, false, input.ClientKey.AccountScope())
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
			failed := lease.Credential
			lastCredentialFailure = &failed
			lastErr = err
			lease.Release()
			continue
		}
		lease.MarkSelectorUpstreamStarted()
		attemptCtx := attemptmeta.WithAccount(ctx, credential.ID, string(route.Provider), route.UpstreamModel)
		request := prepared
		request.Credential = credential
		conn, cleanup, dialErr := adapter.DialVoiceWebSocket(attemptCtx, request)

		if dialErr != nil {
			if cleanup != nil {
				cleanup()
			}
			if errors.Is(dialErr, inferencedomain.ErrAttemptBudget) {
				lease.SkipSelectorObservation()
				lease.Release()
				writeFailureAudit(http.StatusServiceUnavailable, "physical_attempt_limit", &credential)
				return nil, &UpstreamFailure{HTTPStatus: http.StatusServiceUnavailable, Code: "physical_attempt_limit", PublicMessage: "上游尝试次数达到安全上限", Cause: dialErr}
			}
			if status, ok := provider.ErrorHTTPStatus(dialErr); ok {
				failure := newHTTPUpstreamFailure(status, nil, credential.ID, credential.Name)
				failure.RequestScopedForbidden = provider.IsRequestScopedError(dialErr)
				failure.RetryAfter = provider.ErrorRetryAfter(dialErr)
				failure.Cause = dialErr
				lastErr = failure
				switch {
				case status == http.StatusUnauthorized && credential.AuthType == accountdomain.AuthTypeSSO:
					s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
					failed := credential
					lastCredentialFailure = &failed
					lease.Release()
					continue
				case status == http.StatusForbidden && s.providers.RetryForbiddenAsEgress(credential.Provider) && !failure.RequestScopedForbidden && attempt == 0 && attemptPolicy.hasNext(attempt):
					delete(excluded, credential.ID)
					if selection != nil {
						selection.RetryAccount(credential.ID)
					}
					lease.Release()
					continue
				case status == http.StatusPaymentRequired || status == http.StatusTooManyRequests:
					if quotaKind, _ := s.providers.QuotaKind(credential.Provider); quotaKind == provider.QuotaRemoteWindow && lease.QuotaMode != "" {
						state, reconcileErr := s.accounts.ReconcileRateLimit(ctx, credential.ID, lease.QuotaMode, failure.RetryAfter)
						s.applyRateLimitReconciliation(ctx, credential, status, failure.RetryAfter, state, reconcileErr)
					} else {
						s.selector.MarkFailure(ctx, credential, status, failure.RetryAfter)
					}
					if attemptPolicy.hasNext(attempt) {
						lease.Release()
						continue
					}
				case status >= http.StatusInternalServerError && attemptPolicy.hasNext(attempt):
					lease.Release()
					continue
				}
				lease.Release()
				writeFailureAudit(failure.HTTPStatus, failure.AuditCode(), &credential)
				return nil, failure
			}

			lastErr = dialErr
			if isSSOCredentialRejected(dialErr, credential) {
				s.markSSOCredentialRejected(ctx, credential, fmt.Sprintf("%s SSO credential rejected", credential.Provider))
				failed := credential
				lastCredentialFailure = &failed
				lease.Release()
				continue
			}
			failure := newTransportUpstreamFailure(dialErr, credential.ID, credential.Name)
			lastErr = failure
			if ctx.Err() == nil && isRetryableTransportFailure(credential.Provider, dialErr) && attempt == 0 && attemptPolicy.hasNext(attempt) {
				delete(excluded, credential.ID)
				if selection != nil {
					selection.RetryAccount(credential.ID)
				}
				lease.Release()
				continue
			}
			lease.Release()
			writeFailureAudit(failure.HTTPStatus, failure.AuditCode(), &credential)
			return nil, failure
		}

		// Keep the account lease until the websocket finishes so scheduling stays consistent.
		accountLeaseRef := lease
		accountCredential := credential
		var finalizeOnce sync.Once
		var lifecycleMu sync.Mutex
		claimed, terminal := false, false
		finalize := func(outcome VoiceWebSocketOutcome) {
			finalizeOnce.Do(func() {
				lifecycleMu.Lock()
				terminal = true
				lifecycleMu.Unlock()
				defer cancelExecution()
				defer requestBudget.Close()
				_ = conn.Close()
				if cleanup != nil {
					cleanup()
				}
				successful := strings.TrimSpace(outcome.ErrorCode) == ""
				generated := generation.snapshot()
				if !generated.Completed && !outcome.UpstreamFailed {
					accountLeaseRef.SkipSelectorObservation()
				} else {
					accountLeaseRef.CompleteSelectorObservation(generated.Completed)
				}
				accountLeaseRef.Release()

				budget := newFinalizationBudget(string(operation), string(route.Provider))
				accountID := accountCredential.ID
				if generated.Completed && accountLeaseRef.QuotaMode != "" {
					s.finishQuotaConsumption(budget, accountdomain.QuotaConsumption{EventID: "quota_" + eventID, AccountID: accountID, Mode: accountLeaseRef.QuotaMode, SnapshotVersion: accountLeaseRef.QuotaSnapshotVersion, Units: 1})
				}
				if generated.Completed && quotaMode != "" {
					if quotaKind, _ := s.providers.QuotaKind(route.Provider); quotaKind == provider.QuotaRemoteWindow {
						s.accounts.QueueQuotaRefresh(accountID, quotaMode)
					}
				}

				record := auditBase
				record.StatusCode, record.ErrorCode = voiceWebSocketAuditOutcome(outcome)
				record.UpstreamStatusCode = http.StatusSwitchingProtocols
				record.AdmissionOutcome = "not_admitted"
				if outcome.ClientUpgraded {
					record.AdmissionOutcome = "admitted"
				}
				record.GenerationOutcome = generated.Outcome
				record.DeliveryOutcome = "failed"
				if successful {
					record.DeliveryOutcome = "completed"
				}
				if outcome.ErrorCode == "client_stream_interrupted" || outcome.ErrorCode == "request_canceled" {
					record.DeliveryOutcome = "canceled"
				}
				record.DeliveredBytes, record.DeliveredEvents = outcome.DeliveredBytes, outcome.DeliveredEvents
				record.HistoryCommit, record.ProviderStateCommit, record.OwnershipCommit, record.QualityReceipt = "not_required", "not_required", "not_required", "not_required"
				record.PhysicalReceipt = s.finishPhysicalReceipt(ctx)
				record.DurationMS = time.Since(startedAt).Milliseconds()
				record.CreatedAt = time.Now().UTC()
				record.AccountID = &accountID
				record.AccountName = accountCredential.Name
				applyAuditEgress(&record, egressTrace, route.Provider)
				if generated.DurationReported && operation == audit.OperationSTT {
					record.UsageSource = audit.UsageSourceUpstream
					record.AudioDurationMS = int64(math.Round(min(generated.Duration*1000, float64(math.MaxInt64-1024))))
					if pricing, priced := audit.EstimateOfficialSTTCost(generated.Duration, true); priced {
						record.EstimatedCostInUSDTicks = pricing.CostInUSDTicks
						record.PricingModel = pricing.Model
						record.PricingVersion = audit.OfficialPricingAsOf
					}
				}
				if auditErr := budget.run("audit", finalizationAuditBudget, func(stageCtx context.Context) error {
					return s.audits.Create(stageCtx, record)
				}); auditErr != nil {
					s.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", input.RequestID, "error", auditErr)
				}
			})
		}
		stopCancel := context.AfterFunc(ctx, func() {
			lifecycleMu.Lock()
			owned, ended := claimed, terminal
			if !owned {
				terminal = true
			}
			lifecycleMu.Unlock()
			if ended {
				return
			}
			_ = conn.Close()
			if cleanup != nil {
				cleanup()
			}
			accountLeaseRef.SkipSelectorObservation()
			accountLeaseRef.Release()
			if !owned {
				finalize(VoiceWebSocketOutcome{ErrorCode: "request_canceled"})
			}
		})
		begin := func() error {
			lifecycleMu.Lock()
			defer lifecycleMu.Unlock()
			if terminal {
				return context.Canceled
			}
			claimed = true
			return nil
		}
		handedOff = true
		return &VoiceWebSocketSession{
			Conn: conn, BeginDelivery: begin, Finalize: func(outcome VoiceWebSocketOutcome) { stopCancel(); finalize(outcome) }, RequestID: input.RequestID, PublicModel: externalModel,
			Provider: route.Provider, AccountID: credential.ID, AccountName: credential.Name,
			Operation: operation, Capability: capability,
		}, nil
	}
	if lastErr != nil {
		writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
		return nil, lastErr
	}
	writeFailureAudit(http.StatusServiceUnavailable, "upstream_unavailable", lastCredentialFailure)
	return nil, ErrNoAvailableAccount
}

func voiceWebSocketAuditOutcome(outcome VoiceWebSocketOutcome) (int, string) {
	errorCode := strings.TrimSpace(outcome.ErrorCode)
	if errorCode == "" {
		// Audits store the logical request outcome. The transport handshake remains
		// HTTP 101, while successful request accounting uses the common 2xx predicate.
		return http.StatusOK, ""
	}
	return http.StatusBadGateway, errorCode
}

func voiceWebSocketRoute(pathValue string) (modeldomain.Capability, audit.Operation, string, error) {
	switch strings.TrimSpace(pathValue) {
	case "/realtime":
		return modeldomain.CapabilityRealtime, audit.OperationRealtime, "grok-voice-latest", nil
	case "/stt":
		return modeldomain.CapabilitySTT, audit.OperationSTT, "grok-stt", nil
	default:
		return "", "", "", errors.New("不支持的 voice websocket path")
	}
}
