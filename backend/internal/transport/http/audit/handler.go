package audit

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"
	"unicode/utf8"

	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	auditdomain "github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	// queries 是审计只读查询能力；写入、账本健康与生命周期不在 HTTP 合同内。
	queries auditapp.Queries
}

func NewHandler(queries auditapp.Queries) *Handler { return &Handler{queries: queries} }

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/request-audits", h.list)
	router.GET("/request-audits/summary", h.summary)
	router.GET("/request-audits/:id", h.get)
}

type auditResponse struct {
	Diagnostics         *auditdomain.ExecutionDiagnostics `json:"diagnostics,omitempty"`
	UpstreamStatusCode  int                               `json:"upstreamStatusCode"`
	ResponseID          string                            `json:"responseId"`
	ProviderStateCommit string                            `json:"providerStateCommit"`
	AdmissionOutcome    string                            `json:"admissionOutcome"`
	GenerationOutcome   string                            `json:"generationOutcome"`
	OwnershipCommit     string                            `json:"ownershipCommit"`
	DeliveryOutcome     string                            `json:"deliveryOutcome"`
	PhysicalReceipt     string                            `json:"physicalReceipt"`
	QualityReceipt      string                            `json:"qualityReceipt"`
	LedgerOutcome       string                            `json:"ledgerOutcome"`

	HistoryOutcome       string `json:"historyOutcome"`
	HistoryScopeHash     string `json:"historyScopeHash"`
	HistoryGeneration    int64  `json:"historyGeneration"`
	HistoryRestoredItems int    `json:"historyRestoredItems"`
	HistoryNormalizer    int    `json:"historyNormalizer"`
	HistoryCommit        string `json:"historyCommit"`

	ID                        uint64                    `json:"id,string"`
	RequestID                 string                    `json:"requestId"`
	ClientKeyID               uint64                    `json:"clientKeyId,string"`
	ClientKeyName             string                    `json:"clientKeyName,omitempty"`
	ClientIP                  string                    `json:"clientIp,omitempty"`
	ModelRouteID              uint64                    `json:"modelRouteId,string"`
	ModelPublicID             string                    `json:"modelPublicId,omitempty"`
	ModelUpstreamModel        string                    `json:"modelUpstreamModel,omitempty"`
	Provider                  string                    `json:"provider"`
	Operation                 string                    `json:"operation"`
	UsageSource               string                    `json:"usageSource"`
	ReasoningEffort           string                    `json:"reasoningEffort,omitempty"`
	AccountID                 *uint64                   `json:"accountId,string,omitempty"`
	AccountName               string                    `json:"accountName,omitempty"`
	EgressNodeID              *uint64                   `json:"egressNodeId,string,omitempty"`
	EgressNodeName            string                    `json:"egressNodeName,omitempty"`
	EgressScope               string                    `json:"egressScope,omitempty"`
	EgressMode                string                    `json:"egressMode,omitempty"`
	StatusCode                int                       `json:"statusCode"`
	Streaming                 bool                      `json:"streaming"`
	MediaInputImages          int64                     `json:"mediaInputImages"`
	MediaOutputImages         int64                     `json:"mediaOutputImages"`
	MediaOutputSeconds        int64                     `json:"mediaOutputSeconds"`
	AudioDurationMS           int64                     `json:"audioDurationMs"`
	InputTokens               int64                     `json:"inputTokens"`
	CachedInputTokens         int64                     `json:"cachedInputTokens"`
	CachedInputTokensReported *bool                     `json:"cachedInputTokensReported,omitempty"`
	OutputTokens              int64                     `json:"outputTokens"`
	ReasoningTokens           int64                     `json:"reasoningTokens"`
	TotalTokens               int64                     `json:"totalTokens"`
	CostInUSDTicks            int64                     `json:"costInUsdTicks"`
	EstimatedCostInUSDTicks   int64                     `json:"estimatedCostInUsdTicks"`
	PricingModel              string                    `json:"pricingModel,omitempty"`
	PricingVersion            string                    `json:"pricingVersion,omitempty"`
	Billing                   *billingBreakdownResponse `json:"billing,omitempty"`
	NumSourcesUsed            int64                     `json:"numSourcesUsed"`
	NumServerSideToolsUsed    int64                     `json:"numServerSideToolsUsed"`
	ContextInputTokens        int64                     `json:"contextInputTokens"`
	ContextOutputTokens       int64                     `json:"contextOutputTokens"`
	FirstTokenMS              *int64                    `json:"firstTokenMs,omitempty"`
	OutputTokensPerSecond     *float64                  `json:"outputTokensPerSecond,omitempty"`
	// DegradeClass 是对 2xx 流式成功行的降智观测档位；唯一档位
	// terminal_burst 表示"整包末尾爆发+零思考"——这类行 Token/s 是除以
	// ~0ms 的数学假象、速度列显示为空，此前在一切降智汇总里都隐形
	// （续聊链连续降智事故的直接表象）。纯 KPI，不参与执行。
	DegradeClass    string `json:"degradeClass,omitempty"`
	DeliveredEvents int64  `json:"deliveredEvents"`
	DeliveredBytes  int64  `json:"deliveredBytes"`
	DurationMS      int64  `json:"durationMs"`
	ErrorCode       string `json:"errorCode,omitempty"`
	QualityFailOpen bool   `json:"qualityFailOpen,omitempty"`
	// QualityExempt 守卫未介入的豁免原因(disabled 等);QualityRule 守卫介入
	// 时最终交付尝试的判决规则(thinking=观察到思考增量)。
	QualityExempt  string              `json:"qualityExempt,omitempty"`
	QualityRule    string              `json:"qualityRule,omitempty"`
	RequestMethod  string              `json:"requestMethod,omitempty"`
	RequestPath    string              `json:"requestPath,omitempty"`
	RequestHeaders map[string][]string `json:"requestHeaders,omitempty"`
	AttemptCount   int                 `json:"attemptCount"`
	CreatedAt      time.Time           `json:"createdAt"`
}

type billingBreakdownResponse struct {
	Source          string                     `json:"source"`
	Method          string                     `json:"method"`
	Model           string                     `json:"model,omitempty"`
	Version         string                     `json:"version,omitempty"`
	Tier            string                     `json:"tier,omitempty"`
	Components      []billingComponentResponse `json:"components"`
	TotalInUSDTicks int64                      `json:"totalInUsdTicks"`
}

type billingComponentResponse struct {
	Kind                string `json:"kind"`
	Unit                string `json:"unit"`
	Quantity            int64  `json:"quantity"`
	UnitPriceInUSDTicks int64  `json:"unitPriceInUsdTicks"`
	SubtotalInUSDTicks  int64  `json:"subtotalInUsdTicks"`
}

type auditAttemptResponse struct {
	ID                    uint64                    `json:"id,string"`
	Number                int                       `json:"number"`
	Source                string                    `json:"source"`
	Stage                 string                    `json:"stage"`
	AccountID             *uint64                   `json:"accountId,string,omitempty"`
	AccountName           string                    `json:"accountName,omitempty"`
	Method                string                    `json:"method,omitempty"`
	RequestPath           string                    `json:"requestPath,omitempty"`
	UpstreamURL           string                    `json:"upstreamUrl,omitempty"`
	StartedAt             time.Time                 `json:"startedAt"`
	DurationMS            int64                     `json:"durationMs"`
	UpstreamStatusCode    *int                      `json:"upstreamStatusCode,omitempty"`
	UpstreamStatus        string                    `json:"upstreamStatus,omitempty"`
	ResponseHeaders       map[string][]string       `json:"responseHeaders"`
	ResponseBody          string                    `json:"responseBody"`
	ResponseBodyEncoding  string                    `json:"responseBodyEncoding"`
	ResponseBodyTruncated bool                      `json:"responseBodyTruncated"`
	TransportError        string                    `json:"transportError,omitempty"`
	ErrorChain            []auditErrorFrameResponse `json:"errorChain"`
}

type auditErrorFrameResponse struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type auditGenerationUsageResponse struct {
	PhysicalID                string `json:"physicalId"`
	Ordinal                   uint64 `json:"ordinal"`
	AccountID                 string `json:"accountId"`
	AccountName               string `json:"accountName"`
	Model                     string `json:"model"`
	Selected                  bool   `json:"selected"`
	Outcome                   string `json:"outcome"`
	UsageSource               string `json:"usageSource"`
	InputTokens               int64  `json:"inputTokens"`
	CachedInputTokens         int64  `json:"cachedInputTokens"`
	CachedInputTokensReported *bool  `json:"cachedInputTokensReported,omitempty"`
	CacheCreationTokens       int64  `json:"cacheCreationTokens"`
	OutputTokens              int64  `json:"outputTokens"`
	ReasoningTokens           int64  `json:"reasoningTokens"`
	TotalTokens               int64  `json:"totalTokens"`
	ContextInputTokens        int64  `json:"contextInputTokens"`
	ContextOutputTokens       int64  `json:"contextOutputTokens"`
	NumSourcesUsed            int64  `json:"numSourcesUsed"`
	NumServerSideToolsUsed    int64  `json:"numServerSideToolsUsed"`
	CostInUSDTicks            int64  `json:"costInUsdTicks"`
	EstimatedCostInUSDTicks   int64  `json:"estimatedCostInUsdTicks"`
	PricingModel              string `json:"pricingModel"`
	PricingVersion            string `json:"pricingVersion"`
}

type auditDetailResponse struct {
	GenerationUsages []auditGenerationUsageResponse `json:"generationUsages"`
	Audit            auditResponse                  `json:"audit"`
	Attempts         []auditAttemptResponse         `json:"attempts"`
}

func (h *Handler) list(c *gin.Context) {
	if c.Query("pagination") == "cursor" {
		h.listCursor(c)
		return
	}
	// 兼容分页不接过滤参数——此前会静默忽略并返回全量（round 83 活体
	// 钓出：errorCode 过滤在旧分页下无声失效）。显式 400 优于静默。
	for _, name := range []string{"model", "status", "mode", "key", "account", "errorCode", "sortBy", "sortOrder"} {
		if c.Query(name) != "" {
			response.Error(c, http.StatusBadRequest, "filterRequiresCursor", "过滤/排序参数需要 pagination=cursor 分页")
			return
		}
	}
	page, pageSize := httphelpers.Pagination(c)
	values, total, err := h.queries.List(c.Request.Context(), page, pageSize)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditListFailed", "读取审计记录失败")
		return
	}
	items := make([]auditResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newAuditResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) listCursor(c *gin.Context) {
	pageSize := httphelpers.CursorPagination(c)
	result, err := h.queries.ListCursor(c.Request.Context(), c.Query("cursor"), pageSize, c.Query("search"), c.Query("period"), newListFilter(c))
	if errors.Is(err, auditapp.ErrInvalidCursor) {
		response.Error(c, http.StatusBadRequest, "invalidCursor", err.Error())
		return
	}
	if errors.Is(err, auditapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if errors.Is(err, auditapp.ErrInvalidPeriod) {
		response.Error(c, http.StatusBadRequest, "invalidAuditPeriod", "period 仅支持 24h、7d、30d、90d")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditListFailed", "读取审计记录失败")
		return
	}
	items := make([]auditResponse, 0, len(result.Items))
	for _, value := range result.Items {
		items = append(items, newAuditResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "pageSize": pageSize, "nextCursor": result.NextCursor, "hasMore": result.HasMore})
}

func (h *Handler) get(c *gin.Context) {
	id, ok := httphelpers.PathParamID(c, "id", "审计 ID 无效")
	if !ok {
		return
	}
	value, err := h.queries.Get(c.Request.Context(), id)
	if errors.Is(err, repository.ErrNotFound) {
		response.Error(c, http.StatusNotFound, "auditNotFound", "审计记录不存在")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditDetailFailed", "读取审计详情失败")
		return
	}
	attempts := make([]auditAttemptResponse, 0, len(value.Attempts))
	for _, attempt := range value.Attempts {
		body := string(attempt.ResponseBody)
		encoding := "utf8"
		if !utf8.Valid(attempt.ResponseBody) {
			body = base64.StdEncoding.EncodeToString(attempt.ResponseBody)
			encoding = "base64"
		}
		errorChain := make([]auditErrorFrameResponse, 0, len(attempt.ErrorChain))
		for _, frame := range attempt.ErrorChain {
			errorChain = append(errorChain, auditErrorFrameResponse{Type: frame.Type, Message: frame.Message})
		}
		attempts = append(attempts, auditAttemptResponse{
			ID: attempt.ID, Number: attempt.Number, Source: string(attempt.Source), Stage: attempt.Stage,
			AccountID: attempt.AccountID, AccountName: attempt.AccountName, Method: attempt.Method, RequestPath: attempt.RequestPath,
			UpstreamURL: attempt.UpstreamURL, StartedAt: attempt.StartedAt, DurationMS: attempt.DurationMS,
			UpstreamStatusCode: attempt.UpstreamStatusCode, UpstreamStatus: attempt.UpstreamStatus,
			ResponseHeaders: attempt.ResponseHeaders, ResponseBody: body, ResponseBodyEncoding: encoding,
			ResponseBodyTruncated: attempt.ResponseBodyTruncated,
			TransportError:        attempt.TransportError, ErrorChain: errorChain,
		})
	}
	generations := make([]auditGenerationUsageResponse, 0, len(value.GenerationUsages))
	for _, v := range value.GenerationUsages {
		generations = append(generations, auditGenerationUsageResponse{
			PhysicalID:                v.PhysicalID,
			Ordinal:                   v.Ordinal,
			AccountID:                 strconv.FormatUint(v.AccountID, 10),
			AccountName:               v.AccountName,
			Model:                     v.Model,
			Selected:                  v.Selected,
			Outcome:                   v.Outcome,
			UsageSource:               string(v.UsageSource),
			InputTokens:               v.InputTokens,
			CachedInputTokens:         v.CachedInputTokens,
			CachedInputTokensReported: v.CachedInputTokensReported,
			CacheCreationTokens:       v.CacheCreationTokens,
			OutputTokens:              v.OutputTokens,
			ReasoningTokens:           v.ReasoningTokens,
			TotalTokens:               v.TotalTokens,
			ContextInputTokens:        v.ContextInputTokens,
			ContextOutputTokens:       v.ContextOutputTokens,
			NumSourcesUsed:            v.NumSourcesUsed,
			NumServerSideToolsUsed:    v.NumServerSideToolsUsed,
			CostInUSDTicks:            v.CostInUSDTicks,
			EstimatedCostInUSDTicks:   v.EstimatedCostInUSDTicks,
			PricingModel:              v.PricingModel,
			PricingVersion:            v.PricingVersion,
		})
	}
	response.Success(c, http.StatusOK, auditDetailResponse{Audit: newAuditResponse(value), Attempts: attempts, GenerationUsages: generations})
}

type summaryResponse struct {
	Period      string               `json:"period"`
	GeneratedAt time.Time            `json:"generatedAt"`
	Range       summaryRangeResponse `json:"range"`
	Usage       summaryUsageResponse `json:"usage"`
	Pricing     pricingResponse      `json:"pricing"`
}

type summaryRangeResponse struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

type summaryUsageResponse struct {
	Requests                int64   `json:"requests"`
	SuccessfulRequests      int64   `json:"successfulRequests"`
	FailedRequests          int64   `json:"failedRequests"`
	InputTokens             int64   `json:"inputTokens"`
	CachedInputTokens       int64   `json:"cachedInputTokens"`
	OutputTokens            int64   `json:"outputTokens"`
	ReasoningTokens         int64   `json:"reasoningTokens"`
	TotalTokens             int64   `json:"totalTokens"`
	AverageDurationMS       float64 `json:"averageDurationMs"`
	SuccessRate             float64 `json:"successRate"`
	EstimatedCostInUSDTicks int64   `json:"estimatedCostInUsdTicks"`
}

type pricingResponse struct {
	Source           string `json:"source"`
	AsOf             string `json:"asOf"`
	PricedRequests   int64  `json:"pricedRequests"`
	UnpricedRequests int64  `json:"unpricedRequests"`
	PricedTokens     int64  `json:"pricedTokens"`
	UnpricedTokens   int64  `json:"unpricedTokens"`
}

func (h *Handler) summary(c *gin.Context) {
	load := h.queries.Summary
	if c.Query("refresh") == "1" {
		load = h.queries.SummaryFresh
	}
	result, err := load(c.Request.Context(), c.Query("search"), c.Query("period"), newListFilter(c))
	if errors.Is(err, auditapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if errors.Is(err, auditapp.ErrInvalidPeriod) {
		response.Error(c, http.StatusBadRequest, "invalidAuditPeriod", "period 仅支持 24h、7d、30d、90d")
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "auditSummaryFailed", "读取审计统计失败")
		return
	}
	response.Success(c, http.StatusOK, summaryResponse{
		Period: string(result.Period), GeneratedAt: result.GeneratedAt, Range: summaryRangeResponse{Start: result.Start, End: result.End},
		Usage: summaryUsageResponse{
			Requests: result.Usage.Requests, SuccessfulRequests: result.Usage.SuccessfulRequests, FailedRequests: result.Usage.FailedRequests,
			InputTokens: result.Usage.InputTokens, CachedInputTokens: result.Usage.CachedInputTokens, OutputTokens: result.Usage.OutputTokens,
			ReasoningTokens: result.Usage.ReasoningTokens, TotalTokens: result.Usage.TotalTokens, AverageDurationMS: result.Usage.AverageDurationMS,
			SuccessRate: result.Usage.SuccessRate, EstimatedCostInUSDTicks: result.Usage.EstimatedCostInUSDTicks,
		},
		Pricing: pricingResponse{
			Source: auditdomain.OfficialPricingSource, AsOf: auditdomain.OfficialPricingAsOf,
			PricedRequests: result.Usage.PricedRequests, UnpricedRequests: result.Usage.UnpricedRequests,
			PricedTokens: result.Usage.PricedTokens, UnpricedTokens: result.Usage.UnpricedTokens,
		},
	})
}

func newListFilter(c *gin.Context) auditapp.ListFilter {
	return auditapp.ListFilter{
		Model: c.Query("model"), Status: c.Query("status"), Mode: c.Query("mode"),
		Key: c.Query("key"), Account: c.Query("account"), ErrorCode: c.Query("errorCode"),
		Sort: repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))},
	}
}

func newAuditResponse(value auditdomain.Record) auditResponse {
	observation := value.ObserveStream()
	result := auditResponse{
		Diagnostics: value.Diagnostics,
		ID:          value.ID, RequestID: value.RequestID, ClientKeyID: value.ClientKeyID, ClientKeyName: value.ClientKeyName, ClientIP: value.ClientIP,
		ModelRouteID: value.ModelRouteID, ModelPublicID: value.ModelPublicID, ModelUpstreamModel: value.ModelUpstreamModel,
		Provider: value.Provider, Operation: string(value.Operation), UsageSource: string(value.UsageSource),
		ReasoningEffort: value.ReasoningEffort,
		AccountID:       value.AccountID, AccountName: value.AccountName,
		EgressNodeID: value.EgressNodeID, EgressNodeName: value.EgressNodeName, EgressScope: value.EgressScope, EgressMode: string(value.EgressMode),
		StatusCode: value.StatusCode, Streaming: value.Streaming,
		MediaInputImages: value.MediaInputImages, MediaOutputImages: value.MediaOutputImages, MediaOutputSeconds: value.MediaOutputSeconds, AudioDurationMS: value.AudioDurationMS,
		InputTokens: value.InputTokens, CachedInputTokens: value.CachedInputTokens, OutputTokens: value.OutputTokens,
		CachedInputTokensReported: value.CachedInputTokensReported,
		ReasoningTokens:           value.ReasoningTokens, TotalTokens: value.TotalTokens, CostInUSDTicks: value.CostInUSDTicks,
		EstimatedCostInUSDTicks: value.EstimatedCostInUSDTicks, PricingModel: value.PricingModel, PricingVersion: value.PricingVersion,
		Billing:        newBillingBreakdown(value),
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextInputTokens, ContextOutputTokens: value.ContextOutputTokens,
		FirstTokenMS: value.FirstTokenMS, OutputTokensPerSecond: observation.OutputTokensPerSecond, DegradeClass: observation.DegradeClass, DeliveredEvents: value.DeliveredEvents, HistoryOutcome: value.HistoryOutcome, HistoryScopeHash: value.HistoryScopeHash, HistoryGeneration: value.HistoryGeneration, HistoryRestoredItems: value.HistoryRestoredItems, HistoryNormalizer: value.HistoryNormalizer, HistoryCommit: value.HistoryCommit, UpstreamStatusCode: value.UpstreamStatusCode, ResponseID: value.ResponseID, ProviderStateCommit: value.ProviderStateCommit, AdmissionOutcome: value.AdmissionOutcome, GenerationOutcome: value.GenerationOutcome, OwnershipCommit: value.OwnershipCommit, DeliveryOutcome: value.DeliveryOutcome, PhysicalReceipt: value.PhysicalReceipt, QualityReceipt: value.QualityReceipt, LedgerOutcome: value.LedgerOutcome, DeliveredBytes: value.DeliveredBytes, DurationMS: value.DurationMS,
		ErrorCode: value.ErrorCode, QualityFailOpen: value.QualityFailOpen, QualityExempt: value.QualityExempt, QualityRule: value.QualityRule, RequestMethod: value.RequestMethod, RequestPath: value.RequestPath, RequestHeaders: value.RequestHeaders,
		AttemptCount: value.AttemptCount, CreatedAt: value.CreatedAt,
	}
	return result
}

func newBillingBreakdown(value auditdomain.Record) *billingBreakdownResponse {
	explanation, ok := value.ExplainBilling()
	if !ok {
		return nil
	}
	result := &billingBreakdownResponse{
		Source: explanation.Source, Method: explanation.Method, Model: explanation.Model,
		Version: explanation.Version, Tier: string(explanation.Tier), TotalInUSDTicks: explanation.TotalInUSDTicks,
		Components: make([]billingComponentResponse, 0, len(explanation.Components)),
	}
	for _, component := range explanation.Components {
		result.Components = append(result.Components, billingComponentResponse{
			Kind: string(component.Kind), Unit: string(component.Unit), Quantity: component.Quantity,
			UnitPriceInUSDTicks: component.UnitPriceInUSDTicks, SubtotalInUSDTicks: component.CostInUSDTicks,
		})
	}
	return result
}
