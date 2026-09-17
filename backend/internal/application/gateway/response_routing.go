package gateway

import (
	"errors"
	"net/http"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	historyapp "github.com/chenyme/grok2api/backend/internal/application/history"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/requestmeta"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

func (r *responseExecution) prepareRoute() error {
	routes, aliasEffort, err := r.service.resolvePublicModelRoutes(r.ctx, r.input.PublicModel, r.input.ClientKey.AllowModelAliases)
	if err != nil {
		// 无账号（路由在、Provider 当前无可用账号）是可重试的 503 语义，
		// 不能与「模型不存在」（404）一起被扁平化——否则客户端会把暂时
		// 性不可服务当成永久性配置错误放弃重试。
		if errors.Is(err, ErrNoAvailableAccount) {
			return err
		}
		if errors.Is(err, repository.ErrNotFound) {
			return ErrModelNotFound
		}
		// DB 瞬态故障(超时/busy)原样上抛→503:此前与"模型不存在"一起被扁平成
		// 404, SDK 会把暂时性不可服务当成永久性配置错误放弃重试。
		return err
	}
	// Select an initial route only to preserve the existing stateful/stateless
	// previous_response_id boundary. The actual target is chosen from the eligible
	// pool below after ownership and account availability are known.
	initialRoute, routeErr := r.service.selectConversationRoute(routes, r.input.ClientKey, r.operation, r.path, false, nil)

	if r.input.PreviousResponseID != "" && routeErr == nil {
		if r.service.providers.SupportsStoredResponseModel(initialRoute.Provider, initialRoute.UpstreamModel) {
			value, ownershipErr := r.service.responses.Lookup(r.ctx, r.input.PreviousResponseID, r.input.ClientKey.ID, time.Now().UTC())
			if ownershipErr != nil {
				return ownershipErr
			}
			r.ownership = &value
		} else if initialRoute.Provider == accountdomain.ProviderConsole {
			// Console does not retain Response state, so replay the history statelessly here;
			// Provider normalization removes stale Response IDs.
			r.input.PreviousResponseID = ""
		} else {
			return ErrResponseStateUnsupported
		}
	}
	// 消息锚点记忆器:路由排序、预选候选、选中身份三处共享一次全量解析
	//(128KB body 的锚点提取是毫秒级;别名模型重写只改 model/effort 字段,
	// 不触碰 instructions/system/messages,重写前后锚点等价)。
	identityRequest := historyapp.NewIdentityRequest(r.input.ClientKey.ID, r.input.SessionSignals, r.input.PromptCacheKey, r.input.RequestID, r.eventID, r.input.Body)
	eligibleRoutes, fallbackRoute, routeErr := r.service.eligibleConversationRoutes(routes, r.input.ClientKey, r.operation, r.path, r.ownership != nil, r.ownership)
	r.route = fallbackRoute
	orderedRoutes := eligibleRoutes
	if routeErr == nil {
		orderedRoutes = orderConversationRouteTargets(eligibleRoutes, identityRequest.RouteSeed())
		r.route = orderedRoutes[0]
	}
	accountScope := r.input.ClientKey.AccountScope()

	// Skip targets whose account pool is already known to be unavailable. This
	// gives same-name targets failover before any physical upstream request while
	// preserving pinned Responses.
	if routeErr == nil && r.ownership == nil {
		for _, candidate := range orderedRoutes {
			affinityKey := ""
			if candidate.Provider == accountdomain.ProviderBuild {
				identity := r.service.identities.Resolve(identityRequest, historyapp.IdentityTarget{
					Provider: string(candidate.Provider), Model: candidate.UpstreamModel,
					IsolatedWithoutSession: modeldomain.IsGrokComposerModel(candidate.UpstreamModel),
				}, historyapp.Identity{})
				affinityKey = identity.AffinityKey
			}
			candidateSession, selectionErr := r.service.selector.BeginSelectionSessionForKey(
				r.ctx,
				candidate.Provider,
				candidate.ID,
				candidate.UpstreamModel,
				r.service.providers.QuotaMode(candidate.Provider, candidate.UpstreamModel),
				affinityKey,
				nil,
				true,
				accountScope,
			)
			if selectionErr != nil {
				continue
			}
			r.route = candidate
			r.selection = candidateSession
			break
		}
	}
	publicModel := modeldomain.ExternalPublicID(r.route.Provider, r.route.PublicID)
	r.input.PublicModel = publicModel
	if aliasEffort != "" {
		r.input.Body, err = rewriteAliasedModel(r.input.Body, publicModel, aliasEffort, r.operation)
		if err != nil {
			return err
		}
	}
	if routeErr != nil && !errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		return routeErr
	}
	r.timing = newGenerationTiming(publicModel, r.route.Provider)

	r.usageSource = audit.UsageSourceUpstream
	if usageKind, _ := r.service.providers.UsageKind(r.route.Provider); usageKind == provider.UsageEstimated {
		r.usageSource = audit.UsageSourceEstimated
	}
	mediaSummary, _ := summarizeResponseMedia(r.input.Body)
	logResponseMediaSummary(r.service.logger, r.input.RequestID, mediaSummary)
	r.auditBase = audit.Record{
		EventID: r.eventID, RequestID: r.input.RequestID, ClientKeyID: r.input.ClientKey.ID, ClientKeyName: r.input.ClientKey.Name,
		ClientIP:     requestmeta.ClientIP(r.ctx),
		ModelRouteID: r.route.ID, ModelPublicID: publicModel, ModelUpstreamModel: modeldomain.DisplayUpstreamModel(r.route.Provider, r.route.UpstreamModel),
		Provider: string(r.route.Provider), Operation: r.auditOperation, UsageSource: audit.UsageSourceNone, Streaming: r.input.Streaming,
		MediaInputImages: mediaSummary.InputImages,
	}
	if errors.Is(routeErr, clientkeyapp.ErrModelNotAllowed) {
		record := r.auditBase
		record.StatusCode = http.StatusForbidden
		record.DurationMS = time.Since(r.startedAt).Milliseconds()
		record.ErrorCode = "model_not_allowed"
		record.CreatedAt = time.Now().UTC()
		applyAuditEgress(&record, r.egressTrace, r.route.Provider)
		if err := r.service.audits.Create(r.ctx, record); err != nil {
			r.service.logger.Error("request_usage_write_failed", "event_id", record.EventID, "request_id", r.input.RequestID, "error", err)
		}
		return clientkeyapp.ErrModelNotAllowed
	}
	r.affinityKey = ""
	r.ownershipPromptCacheKey = ""
	r.reasoningReplayKey = ""
	r.priorReasoningReplayKey = ""
	if r.route.Provider == accountdomain.ProviderBuild {
		// Derive a stable identity from explicit session signals, message anchors,
		// and model. Composer replaces message-only fallback identities with an
		// isolated request identity that remains stable across retries.
		inherited := historyapp.Identity{}
		if r.ownership != nil && r.ownership.PromptCacheKey != "" {
			inherited.UpstreamID = r.ownership.PromptCacheKey
			inherited.ReplayKey = r.ownership.ReasoningReplayKey
		}
		identity := r.service.identities.Resolve(identityRequest, historyapp.IdentityTarget{
			Provider: string(r.route.Provider), Model: r.route.UpstreamModel,
			IsolatedWithoutSession: modeldomain.IsGrokComposerModel(r.route.UpstreamModel),
		}, inherited)
		r.input.PromptCacheKey = identity.UpstreamID
		r.affinityKey = identity.AffinityKey
		r.ownershipPromptCacheKey = identity.UpstreamID
		r.reasoningReplayKey = identity.ReplayKey
		r.priorReasoningReplayKey = identity.PriorReplayKey
		if identity.UpstreamID == "" {
			r.service.logger.Debug("prompt_cache_session_empty", "request_id", r.input.RequestID, "model", r.route.UpstreamModel, "provider", r.route.Provider)
		} else if identity.Soft {
			r.service.logger.Debug("prompt_cache_session_soft", "request_id", r.input.RequestID, "model", r.route.UpstreamModel)
		} else if identity.Isolated {
			r.service.logger.Debug("prompt_cache_session_isolated", "request_id", r.input.RequestID, "model", r.route.UpstreamModel)
		}
	}
	_, ok := r.service.providers.Responses(r.route.Provider)
	if !ok {
		return ErrNoAvailableAccount
	}
	return nil
}
