package egress

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service *egressapp.Service
	logger  *slog.Logger
}

func NewHandler(service *egressapp.Service, logger ...*slog.Logger) *Handler {
	instance := &Handler{service: service}
	if len(logger) > 0 && logger[0] != nil {
		instance.logger = logger[0]
	}
	return instance
}

// log 返回已注入的 logger；未注入时回退 slog.Default()（测试/零值构造）。
func (h *Handler) log() *slog.Logger {
	if h.logger != nil {
		return h.logger
	}
	return slog.Default()
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/egress-pools", h.listPools)
	router.POST("/egress-pools", h.createPool)
	router.PUT("/egress-pools/:id", h.updatePool)
	router.DELETE("/egress-pools/:id", h.deletePool)
	router.PUT("/egress-pools/:id/members", h.setPoolMembers)
	router.GET("/egress-nodes", h.list)
	router.POST("/egress-nodes", h.create)
	router.PATCH("/egress-nodes/batch", h.updateMany)
	router.DELETE("/egress-nodes", h.deleteMany)
	router.GET("/egress-nodes/cleanup-preview", h.cleanupPreview)
	router.POST("/egress-nodes/batch-rotation", h.batchSetRotation)
	router.POST("/egress-nodes/cleanup", h.cleanup)
	router.POST("/egress-nodes/test", h.testNodes)
	router.POST("/egress-nodes/:id/test", h.testNode)
	router.POST("/egress-nodes/:id/proxy-url/reveal", h.proxyURL)
	router.POST("/egress-nodes/:id/rotation-url/reveal", h.rotationURL)
	router.POST("/egress-nodes/:id/rotate", h.rotateNode)
	router.PUT("/egress-nodes/:id", h.update)
	router.POST("/egress-nodes/:id/refresh-clearance", h.refreshClearance)
	router.DELETE("/egress-nodes/:id", h.delete)
	router.POST("/egress-imports", h.importText)
	router.GET("/egress-sources", h.listSources)
	router.POST("/egress-sources", h.createSource)
	router.POST("/egress-sources/:id/sync", h.syncSource)
	router.PUT("/egress-sources/:id", h.updateSource)
	router.DELETE("/egress-sources/:id", h.deleteSource)
	router.POST("/egress-sources/:id/url/reveal", h.sourceURL)
	router.POST("/egress-sources/:id/proxy-url/reveal", h.sourceProxyURL)
	router.GET("/egress-operations", h.operationsConfig)
	router.PUT("/egress-operations", h.updateOperationsConfig)
	router.GET("/egress-operations/routing-stats", h.routingStats)
	router.PUT("/egress-pools/:id/members/:nodeId/priority", h.setPoolMemberPriority)
	router.GET("/egress-pools/:id/stats", h.poolStats)
	router.DELETE("/egress-pools/:id/stats", h.resetPoolStats)
}

// routingStats reports process-local routing outcome counters for the admin
// UI. Counts reset on restart and are read-only.
func (h *Handler) routingStats(c *gin.Context) {
	response.Success(c, http.StatusOK, gin.H{"items": infraegress.RoutingStatsSnapshot()})
}

// setPoolMemberPriority 设置池内成员首选顺序（小者先）。
func (h *Handler) setPoolMemberPriority(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool id"})
		return
	}
	nodeID, err := strconv.ParseUint(c.Param("nodeId"), 10, 64)
	if err != nil || nodeID == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid node id"})
		return
	}
	var body struct {
		Priority *int64 `json:"priority"`
	}
	if err := c.ShouldBindJSON(&body); err != nil || body.Priority == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "priority is required"})
		return
	}
	if err := h.service.SetPoolMemberPriority(c.Request.Context(), id, nodeID, *body.Priority); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": true})
}

// poolStats 报告一个池内每个节点的进程内存调度统计（选中/失败），
// 供管理界面验证调度策略是否生效。重启归零，可用 DELETE 清零。
func (h *Handler) poolStats(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool id"})
		return
	}
	items, since := infraegress.PoolStatsSnapshot(id)
	response.Success(c, http.StatusOK, gin.H{"items": items, "since": since})
}

func (h *Handler) resetPoolStats(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid pool id"})
		return
	}
	infraegress.ResetPoolStats(id)
	response.Success(c, http.StatusOK, gin.H{"reset": true})
}

func (h *Handler) cleanupPreview(c *gin.Context) {
	value, err := h.service.PreviewUnhealthyCleanup(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"nodes": value.Nodes, "subscriptionManaged": value.SubscriptionManaged,
	})
}

func (h *Handler) cleanup(c *gin.Context) {
	deleted, err := h.service.DeleteUnhealthy(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) refreshClearance(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.RefreshClearance(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"refreshed": true})
}

type nodeRequest struct {
	Name             string  `json:"name"`
	Enabled          bool    `json:"enabled"`
	ProxyPool        *bool   `json:"proxyPool"`
	ProxyURL         *string `json:"proxyURL"`
	ClearProxyURL    bool    `json:"clearProxyURL"`
	RotationURL      *string `json:"rotationURL"`
	ClearRotationURL bool    `json:"clearRotationURL"`
	RotationEnabled  *bool   `json:"rotationEnabled"`
}

type nodeResponse struct {
	ID                 uint64              `json:"id,string"`
	Name               string              `json:"name"`
	Enabled            bool                `json:"enabled"`
	ProxyConfigured    bool                `json:"proxyConfigured"`
	ProxyDisplay       string              `json:"proxyDisplay,omitempty"`
	ProxyFingerprint   string              `json:"proxyFingerprint,omitempty"`
	ProxyPool          bool                `json:"proxyPool"`
	SourceID           uint64              `json:"sourceId,omitempty,string"`
	SourceName         string              `json:"sourceName,omitempty"`
	Pools              []nodePoolRef       `json:"pools,omitempty"`
	AccountBoundProxy  bool                `json:"accountBoundProxy"`
	RotationConfigured bool                `json:"rotationConfigured"`
	RotationEnabled    bool                `json:"rotationEnabled"`
	LastRotatedAt      *time.Time          `json:"lastRotatedAt,omitempty"`
	RotationAttempts   int                 `json:"rotationAttempts"`
	LastRotationError  string              `json:"lastRotationError,omitempty"`
	DegradeCount       int                 `json:"degradeCount"`
	LastDegradedAt     *time.Time          `json:"lastDegradedAt,omitempty"`
	Health             float64             `json:"health"`
	FailureCount       int                 `json:"failureCount"`
	CooldownUntil      *time.Time          `json:"cooldownUntil,omitempty"`
	LastError          string              `json:"lastError,omitempty"`
	ProbeStatus        string              `json:"probeStatus"`
	LastProbedAt       *time.Time          `json:"lastProbedAt,omitempty"`
	ProbeLatencyMS     int                 `json:"probeLatencyMs"`
	ExitIP             string              `json:"exitIp,omitempty"`
	ProbeError         string              `json:"probeError,omitempty"`
	ProbeProvider      string              `json:"probeProvider,omitempty"`
	IPv4Probe          probeFamilyResponse `json:"ipv4Probe"`
	IPv6Probe          probeFamilyResponse `json:"ipv6Probe"`
}

type probeFamilyResponse struct {
	Status    string     `json:"status"`
	TestedAt  *time.Time `json:"testedAt,omitempty"`
	LatencyMS int        `json:"latencyMs"`
	ExitIP    string     `json:"exitIp,omitempty"`
	Error     string     `json:"error,omitempty"`
}

type batchNodeDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type batchNodeUpdateRequest struct {
	IDs     []string `json:"ids" binding:"required"`
	Enabled *bool    `json:"enabled" binding:"required"`
}

func (h *Handler) updateMany(c *gin.Context) {
	var request batchNodeUpdateRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	updated, err := h.service.UpdateManyEnabled(c.Request.Context(), ids, *request.Enabled)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": updated})
}

func (h *Handler) deleteMany(c *gin.Context) {
	var request batchNodeDeleteRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	deleted, err := h.service.DeleteMany(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (value nodeRequest) input() egressapp.Input {
	return egressapp.Input{
		Name: value.Name, Enabled: value.Enabled, ProxyPool: value.ProxyPool,
		ProxyURL: value.ProxyURL, ClearProxyURL: value.ClearProxyURL,
		RotationURL: value.RotationURL, ClearRotationURL: value.ClearRotationURL, RotationEnabled: value.RotationEnabled,
	}
}

func (h *Handler) list(c *gin.Context) {
	sort := repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))}
	if legacyEgressListRequest(c) {
		values, err := h.service.ListAll(c.Request.Context(), sort)
		if h.writeListError(c, err) {
			return
		}
		items := make([]nodeResponse, 0, len(values))
		for _, value := range values {
			items = append(items, newNodeResponse(value))
		}
		pageSize := len(items)
		if pageSize == 0 {
			pageSize = repository.DefaultPageSize
		}
		response.Success(c, http.StatusOK, gin.H{"items": items, "page": 1, "pageSize": pageSize, "total": len(items)})
		return
	}
	page, pageSize := nodePagination(c)
	values, total, err := h.service.List(c.Request.Context(), page, pageSize, c.Query("search"), egressapp.ListFilter{
		Enabled: c.Query("enabled"), ProbeStatus: c.Query("probe"),
		Sort: sort,
	})
	if h.writeListError(c, err) {
		return
	}
	items := make([]nodeResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newNodeResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func legacyEgressListRequest(c *gin.Context) bool {
	if _, exists := c.GetQuery("page"); exists {
		return false
	}
	if _, exists := c.GetQuery("pageSize"); exists {
		return false
	}
	return c.Query("search") == "" && c.Query("enabled") == "" && c.Query("probe") == ""
}

func (h *Handler) writeListError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, egressapp.ErrInvalidFilter):
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
	case errors.Is(err, egressapp.ErrInvalidSort):
		response.Error(c, http.StatusBadRequest, "invalidSort", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressNodeListFailed", "读取代理节点失败")
	}
	return true
}

func nodePagination(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	return repository.NormalizePage(page, pageSize, repository.DefaultPageSize)
}

func (h *Handler) create(c *gin.Context) {
	var request nodeRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	value, err := h.service.Create(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newNodeResponse(value))
}

func (h *Handler) update(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request nodeRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	value, err := h.service.Update(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newNodeResponse(value))
}

func (h *Handler) proxyURL(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.ProxyURL(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"proxyURL": value})
}

func (h *Handler) rotationURL(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.RotationURL(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"rotationURL": value})
}

type nodePoolRef struct {
	ID   uint64 `json:"id,string"`
	Name string `json:"name"`
}

func newNodePoolRefs(values []egressdomain.NodePoolRef) []nodePoolRef {
	if len(values) == 0 {
		return nil
	}
	refs := make([]nodePoolRef, 0, len(values))
	for _, value := range values {
		refs = append(refs, nodePoolRef{ID: value.ID, Name: value.Name})
	}
	return refs
}

func newNodeResponse(value egressdomain.PublicNode) nodeResponse {
	return nodeResponse{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled,
		ProxyConfigured: value.ProxyConfigured, ProxyDisplay: value.ProxyDisplay, ProxyFingerprint: value.ProxyFingerprint,
		ProxyPool:          value.ProxyPool,
		AccountBoundProxy:  value.AccountBoundProxy,
		RotationConfigured: value.RotationConfigured, RotationEnabled: value.RotationEnabled, LastRotatedAt: value.LastRotatedAt,
		RotationAttempts: value.RotationAttempts, LastRotationError: value.LastRotationError,
		DegradeCount: value.DegradeCount, LastDegradedAt: value.LastDegradedAt,
		SourceID: value.SourceID, SourceName: value.SourceName,
		Pools:  newNodePoolRefs(value.Pools),
		Health: value.Health, FailureCount: value.FailureCount, CooldownUntil: value.CooldownUntil, LastError: value.LastError,
		ProbeStatus: string(value.ProbeStatus), LastProbedAt: value.LastProbedAt, ProbeLatencyMS: value.ProbeLatencyMS, ExitIP: value.ExitIP, ProbeError: value.ProbeError,
		ProbeProvider: string(value.ProbeProvider),
		IPv4Probe:     newProbeFamilyResponse(value.IPv4Probe), IPv6Probe: newProbeFamilyResponse(value.IPv6Probe),
	}
}

func newProbeFamilyResponse(value egressdomain.ProbeFamilyResult) probeFamilyResponse {
	status := value.Status
	if !status.IsValid() {
		status = egressdomain.ProbeStatusUnknown
	}
	var testedAt *time.Time
	if !value.TestedAt.IsZero() {
		canonical := value.TestedAt.UTC()
		testedAt = &canonical
	}
	return probeFamilyResponse{
		Status: string(status), TestedAt: testedAt, LatencyMS: value.LatencyMS, ExitIP: value.ExitIP, Error: value.Error,
	}
}

func parseEgressNodeIDs(values []string) ([]uint64, error) {
	result := make([]uint64, 0, len(values))
	seen := make(map[uint64]struct{}, len(values))
	for _, value := range values {
		id, err := strconv.ParseUint(value, 10, 64)
		if err != nil || id == 0 {
			return nil, errors.New("invalid id")
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	if len(result) == 0 {
		return nil, errors.New("no ids")
	}
	return result, nil
}

func parseBoundedEgressNodeIDs(values []string, limit int) ([]uint64, error) {
	if len(values) == 0 || limit < 1 || len(values) > limit {
		return nil, errors.New("invalid id count")
	}
	return parseEgressNodeIDs(values)
}

func (h *Handler) delete(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

type sourceRequest struct {
	Name                   string  `json:"name"`
	Enabled                bool    `json:"enabled"`
	URL                    *string `json:"url"`
	ClearURL               bool    `json:"clearUrl"`
	ProxyURL               *string `json:"proxyURL"`
	ClearProxyURL          bool    `json:"clearProxyURL"`
	RefreshIntervalSeconds *int    `json:"refreshIntervalSeconds"`
}

type sourceResponse struct {
	ID                     uint64     `json:"id,string"`
	Name                   string     `json:"name"`
	Enabled                bool       `json:"enabled"`
	URLConfigured          bool       `json:"urlConfigured"`
	ProxyConfigured        bool       `json:"proxyConfigured"`
	RefreshIntervalSeconds int        `json:"refreshIntervalSeconds"`
	LastSyncedAt           *time.Time `json:"lastSyncedAt,omitempty"`
	NextSyncAt             *time.Time `json:"nextSyncAt,omitempty"`
	LastSyncImported       int        `json:"lastSyncImported"`
	LastSyncError          string     `json:"lastSyncError,omitempty"`
}

type importRequest struct {
	Name    string `json:"name"`
	Content string `json:"content"`
}

type probeBatchRequest struct {
	IDs []string `json:"ids"`
}

// routingTargetRequest is one routing decision: auto resets the level to
// follow the next level down; node/pool require their id.
type routingTargetRequest struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId"`
	PoolID string `json:"poolId"`
}

type routingTargetResponse struct {
	Mode   string `json:"mode"`
	NodeID string `json:"nodeId,omitempty"`
	PoolID string `json:"poolId,omitempty"`
}

func parseRoutingTargetRequest(value routingTargetRequest) (egressapp.RoutingTargetInput, error) {
	mode := egressdomain.RoutingTargetMode(strings.TrimSpace(value.Mode))
	if mode == "" {
		mode = egressdomain.RoutingTargetAuto
	}
	if !mode.IsValid() {
		return egressapp.RoutingTargetInput{}, fmt.Errorf("%w: 路由目标类型无效: %q", egressapp.ErrInvalidInput, value.Mode)
	}
	nodeID, err := parseOptionalID(value.NodeID, "路由目标节点")
	if err != nil {
		return egressapp.RoutingTargetInput{}, err
	}
	poolID, err := parseOptionalID(value.PoolID, "路由目标代理池")
	if err != nil {
		return egressapp.RoutingTargetInput{}, err
	}
	return egressapp.RoutingTargetInput{Mode: mode, NodeID: nodeID, PoolID: poolID}, nil
}

func parseOptionalID(raw string, label string) (uint64, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseUint(trimmed, 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%w: %s ID 无效", egressapp.ErrInvalidInput, label)
	}
	return parsed, nil
}

func newRoutingTargetResponse(value egressdomain.RoutingTarget) routingTargetResponse {
	item := routingTargetResponse{Mode: string(value.Mode.Normalized())}
	if value.NodeID != 0 {
		item.NodeID = strconv.FormatUint(value.NodeID, 10)
	}
	if value.PoolID != 0 {
		item.PoolID = strconv.FormatUint(value.PoolID, 10)
	}
	return item
}

type operationsConfigRequest struct {
	ProbeProvider        string                          `json:"probeProvider"`
	ProbeIntervalSeconds int                             `json:"probeIntervalSeconds"`
	DefaultTarget        *routingTargetRequest           `json:"defaultTarget"`
	ScopeTargets         map[string]routingTargetRequest `json:"scopeTargets"`
	ClassTargets         map[string]routingTargetRequest `json:"classTargets"`
}

type operationsConfigResponse struct {
	ProbeProvider        string                           `json:"probeProvider"`
	ProbeIntervalSeconds int                              `json:"probeIntervalSeconds"`
	DefaultTarget        routingTargetResponse            `json:"defaultTarget"`
	ScopeTargets         map[string]routingTargetResponse `json:"scopeTargets"`
	ClassTargets         map[string]routingTargetResponse `json:"classTargets"`
	UpdatedAt            time.Time                        `json:"updatedAt"`
}

func (value operationsConfigRequest) input() (egressapp.OperationsConfigInput, error) {
	result := egressapp.OperationsConfigInput{
		ProbeProvider: egressdomain.ProbeProvider(strings.TrimSpace(value.ProbeProvider)), ProbeIntervalSeconds: value.ProbeIntervalSeconds,
	}
	if value.DefaultTarget != nil {
		target, err := parseRoutingTargetRequest(*value.DefaultTarget)
		if err != nil {
			return egressapp.OperationsConfigInput{}, err
		}
		result.DefaultTarget = &target
	}
	if value.ScopeTargets != nil {
		result.ScopeTargets = make(map[egressdomain.Scope]egressapp.RoutingTargetInput, len(value.ScopeTargets))
		for rawScope, target := range value.ScopeTargets {
			parsed, err := parseRoutingTargetRequest(target)
			if err != nil {
				return egressapp.OperationsConfigInput{}, err
			}
			result.ScopeTargets[egressdomain.Scope(strings.TrimSpace(rawScope))] = parsed
		}
	}
	if value.ClassTargets != nil {
		result.ClassTargets = make(map[egressdomain.TrafficClass]egressapp.RoutingTargetInput, len(value.ClassTargets))
		for rawClass, target := range value.ClassTargets {
			parsed, err := parseRoutingTargetRequest(target)
			if err != nil {
				return egressapp.OperationsConfigInput{}, err
			}
			result.ClassTargets[egressdomain.TrafficClass(strings.TrimSpace(rawClass))] = parsed
		}
	}
	return result, nil
}

func newOperationsConfigResponse(value egressdomain.OperationsConfig) operationsConfigResponse {
	scopes := make(map[string]routingTargetResponse)
	for scope, target := range value.ScopeTargets {
		scopes[string(scope)] = newRoutingTargetResponse(target)
	}
	classes := make(map[string]routingTargetResponse)
	for class, target := range value.ClassTargets {
		classes[string(class)] = newRoutingTargetResponse(target)
	}
	return operationsConfigResponse{
		ProbeProvider: string(value.ProbeProvider.Normalized()), ProbeIntervalSeconds: value.ProbeIntervalSeconds,
		DefaultTarget: newRoutingTargetResponse(value.DefaultTarget), ScopeTargets: scopes, ClassTargets: classes,
		UpdatedAt: value.UpdatedAt,
	}
}

func (value sourceRequest) input() egressapp.SubscriptionSourceInput {
	return egressapp.SubscriptionSourceInput{
		Name: value.Name, Enabled: value.Enabled, URL: value.URL, ClearURL: value.ClearURL,
		ProxyURL: value.ProxyURL, ClearProxyURL: value.ClearProxyURL,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds,
	}
}

func newSourceResponse(value egressdomain.PublicSubscriptionSource) sourceResponse {
	return sourceResponse{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled, URLConfigured: value.URLConfigured,
		ProxyConfigured:        value.ProxyConfigured,
		RefreshIntervalSeconds: value.RefreshIntervalSeconds,
		LastSyncedAt:           value.LastSyncedAt, NextSyncAt: value.NextSyncAt, LastSyncImported: value.LastSyncImported, LastSyncError: value.LastSyncError,
	}
}

func (h *Handler) listSources(c *gin.Context) {
	if !legacyEgressSourceListRequest(c) {
		page, pageSize := nodePagination(c)
		values, total, err := h.service.ListSourcePage(c.Request.Context(), page, pageSize, c.Query("search"))
		if h.writeSourceListError(c, err) {
			return
		}
		items := make([]sourceResponse, 0, len(values))
		for _, value := range values {
			items = append(items, newSourceResponse(value))
		}
		response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
		return
	}
	values, err := h.service.ListSources(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]sourceResponse, 0, len(values))
	for _, value := range values {
		items = append(items, newSourceResponse(value))
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func legacyEgressSourceListRequest(c *gin.Context) bool {
	if _, exists := c.GetQuery("page"); exists {
		return false
	}
	if _, exists := c.GetQuery("pageSize"); exists {
		return false
	}
	return c.Query("search") == ""
}

func (h *Handler) writeSourceListError(c *gin.Context, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, egressapp.ErrInvalidFilter):
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
	default:
		response.Error(c, http.StatusInternalServerError, "egressSourceListFailed", "读取代理订阅来源失败")
	}
	return true
}

func (h *Handler) createSource(c *gin.Context) {
	var request sourceRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	value, err := h.service.CreateSource(c.Request.Context(), request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newSourceResponse(value))
}

func (h *Handler) updateSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request sourceRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	value, err := h.service.UpdateSource(c.Request.Context(), id, request.input())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newSourceResponse(value))
}

func (h *Handler) sourceURL(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.SourceURL(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"url": value})
}

func (h *Handler) sourceProxyURL(c *gin.Context) {
	c.Header("Cache-Control", "private, no-store")
	c.Header("Pragma", "no-cache")
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.SourceProxyURL(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"proxyURL": value})
}

func (h *Handler) deleteSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.DeleteSource(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

func (h *Handler) syncSource(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.SyncSource(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

func (h *Handler) importText(c *gin.Context) {
	var request importRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	value, err := h.service.ImportText(c.Request.Context(), egressapp.ImportInput{
		Name: request.Name, Content: request.Content,
	})
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, gin.H{"imported": value.Imported, "skipped": value.Skipped})
}

// batchSetRotation 用模板批量设置选中节点的换 IP Webhook；空模板=清除。
func (h *Handler) batchSetRotation(c *gin.Context) {
	var request batchRotationRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseBoundedEgressNodeIDs(request.IDs, 5000)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "代理节点 ID 无效")
		return
	}
	result, err := h.service.BatchSetNodeRotation(c.Request.Context(), ids, strings.TrimSpace(request.Template))
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": result.Updated, "skipped": result.Skipped})
}

type batchRotationRequest struct {
	IDs      []string `json:"ids" binding:"required"`
	Template string   `json:"template"`
}

type poolRequest struct {
	Name           string `json:"name" binding:"required"`
	Enabled        *bool  `json:"enabled"`
	Strategy       string `json:"strategy"`
	FallbackMode   string `json:"fallbackMode"`
	FallbackPoolID string `json:"fallbackPoolId"`
}

type poolResponse struct {
	ID                   uint64    `json:"id,string"`
	Name                 string    `json:"name"`
	Enabled              bool      `json:"enabled"`
	Strategy             string    `json:"strategy"`
	FallbackMode         string    `json:"fallbackMode"`
	FallbackPoolID       uint64    `json:"fallbackPoolId,omitempty,string"`
	FallbackPoolName     string    `json:"fallbackPoolName,omitempty"`
	MemberCount          int       `json:"memberCount"`
	HealthyCount         int       `json:"healthyCount"`
	QuarantinedCount     int       `json:"quarantinedCount"`
	MemberIDs            []string  `json:"memberIds"`
	PreferredNodeID      uint64    `json:"preferredNodeId,omitempty,string"`
	RotationCursorNodeID uint64    `json:"rotationCursorNodeId,omitempty,string"`
	LastSelectedNodeID   uint64    `json:"lastSelectedNodeId,omitempty,string"`
	CreatedAt            time.Time `json:"createdAt"`
	UpdatedAt            time.Time `json:"updatedAt"`
}

func newPoolResponse(value egressdomain.PublicPool) poolResponse {
	return poolResponse{
		ID: value.ID, Name: value.Name, Enabled: value.Enabled,
		Strategy:     string(value.Strategy.Normalized()),
		FallbackMode: string(value.FallbackMode.Normalized()), FallbackPoolID: value.FallbackPoolID, FallbackPoolName: value.FallbackPoolName,
		MemberCount: value.MemberCount, HealthyCount: value.HealthyCount, QuarantinedCount: value.QuarantinedCount,
		MemberIDs: poolMemberIDs(value.MemberIDs), PreferredNodeID: value.PreferredNodeID, RotationCursorNodeID: value.RotationCursorNodeID,
		CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt,
	}
}

func poolMemberIDs(values []uint64) []string {
	if len(values) == 0 {
		return []string{}
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, strconv.FormatUint(value, 10))
	}
	return result
}

func (value poolRequest) input() (egressapp.PoolInput, error) {
	fallbackPoolID := uint64(0)
	if trimmed := strings.TrimSpace(value.FallbackPoolID); trimmed != "" {
		parsed, err := strconv.ParseUint(trimmed, 10, 64)
		if err != nil || parsed == 0 {
			return egressapp.PoolInput{}, fmt.Errorf("%w: 回退代理池 ID 无效", egressapp.ErrInvalidInput)
		}
		fallbackPoolID = parsed
	}
	return egressapp.PoolInput{
		Name: value.Name, Enabled: value.Enabled,
		Strategy:     egressdomain.PoolStrategy(strings.TrimSpace(value.Strategy)),
		FallbackMode: egressdomain.PoolFallbackMode(strings.TrimSpace(value.FallbackMode)), FallbackPoolID: fallbackPoolID,
	}, nil
}

type poolMembersRequest struct {
	// NodeIDs 是新字段名;IDs 兼容旧客户端。指针用于区分"未提供"与"显式
	// 清空":两个键都缺位的载荷(例如误用响应字段 memberIds)曾把整个成员
	// 关系静默清空并返回 updated=true。
	NodeIDs *[]string `json:"nodeIds"`
	IDs     *[]string `json:"ids"`
}

// setPoolMembers replaces the full membership of one pool. Pool-side
// selection is the only membership write path (a node may join many pools).
func (h *Handler) setPoolMembers(c *gin.Context) {
	poolID, ok := pathID(c)
	if !ok {
		return
	}
	var request poolMembersRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	rawIDs := request.NodeIDs
	if rawIDs == nil {
		rawIDs = request.IDs
	}
	if rawIDs == nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求必须携带 nodeIds 或 ids(空数组表示清空全部成员)")
		return
	}
	if len(*rawIDs) > 5000 {
		response.Error(c, http.StatusBadRequest, "invalidId", "节点数量最多 5000")
		return
	}
	ids := make([]uint64, 0, len(*rawIDs))
	seen := make(map[uint64]struct{}, len(*rawIDs))
	for _, raw := range *rawIDs {
		parsed, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil || parsed == 0 {
			response.Error(c, http.StatusBadRequest, "invalidId", "节点 ID 无效")
			return
		}
		if _, exists := seen[parsed]; exists {
			continue
		}
		seen[parsed] = struct{}{}
		ids = append(ids, parsed)
	}
	if err := h.service.SetPoolMembers(c.Request.Context(), poolID, ids); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"updated": true})
}

func (h *Handler) listPools(c *gin.Context) {
	values, err := h.service.ListPools(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	items := make([]poolResponse, 0, len(values))
	for _, value := range values {
		item := newPoolResponse(value)
		// 最近使用:池统计里 lastSelectedAt 最新的节点,任何策略都适用。
		if stats, _ := infraegress.PoolStatsSnapshot(value.ID); len(stats) > 0 {
			var latest *infraegress.PoolNodeStat
			for index := range stats {
				if stats[index].LastSelectedAt.IsZero() {
					continue
				}
				if latest == nil || stats[index].LastSelectedAt.After(latest.LastSelectedAt) {
					latest = &stats[index]
				}
			}
			if latest != nil {
				item.LastSelectedNodeID = latest.NodeID
			}
		}
		items = append(items, item)
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}

func (h *Handler) createPool(c *gin.Context) {
	var request poolRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	input, inputErr := request.input()
	if inputErr != nil {
		h.writeError(c, inputErr)
		return
	}
	value, err := h.service.CreatePool(c.Request.Context(), input)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusCreated, newPoolResponse(value))
}

func (h *Handler) updatePool(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var request poolRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	input, inputErr := request.input()
	if inputErr != nil {
		h.writeError(c, inputErr)
		return
	}
	value, err := h.service.UpdatePool(c.Request.Context(), id, input)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newPoolResponse(value))
}

func (h *Handler) deletePool(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.DeletePool(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": true})
}

// rotateNode 触发一次手动出口 IP 轮换（排入自动轮换队列）。
func (h *Handler) rotateNode(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	if err := h.service.RotateNode(c.Request.Context(), id); err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusAccepted, gin.H{"queued": true})
}

func (h *Handler) testNode(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	value, err := h.service.TestNode(c.Request.Context(), id)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"status": value.Status, "testedAt": value.TestedAt, "latencyMs": value.LatencyMS, "exitIp": value.ExitIP, "error": value.Error,
		"probeProvider": value.Provider,
		"ipv4":          newProbeFamilyResponse(value.IPv4), "ipv6": newProbeFamilyResponse(value.IPv6),
	})
}

func (h *Handler) testNodes(c *gin.Context) {
	var request probeBatchRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	ids, err := parseOptionalNodeIDs(request.IDs)
	if err != nil {
		response.Error(c, http.StatusBadRequest, "invalidId", "节点 ID 无效")
		return
	}
	value, err := h.service.TestNodes(c.Request.Context(), ids)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, gin.H{"requested": value.Requested, "healthy": value.Healthy, "unhealthy": value.Unhealthy})
}

func (h *Handler) operationsConfig(c *gin.Context) {
	value, err := h.service.OperationsConfig(c.Request.Context())
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func (h *Handler) updateOperationsConfig(c *gin.Context) {
	var request operationsConfigRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	input, err := request.input()
	if err != nil {
		h.writeError(c, err)
		return
	}
	value, err := h.service.UpdateOperationsConfig(c.Request.Context(), input)
	if err != nil {
		h.writeError(c, err)
		return
	}
	response.Success(c, http.StatusOK, newOperationsConfigResponse(value))
}

func parseOptionalNodeIDs(values []string) ([]uint64, error) {
	if len(values) == 0 {
		return nil, nil
	}
	return parseEgressNodeIDs(values)
}

func (h *Handler) writeError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, egressapp.ErrInvalidInput):
		response.Error(c, http.StatusBadRequest, "invalidEgressNode", err.Error())
	case errors.Is(err, egressapp.ErrNotFound):
		response.Error(c, http.StatusNotFound, "egressNodeNotFound", err.Error())
	case errors.Is(err, egressapp.ErrProbeStale):
		response.Error(c, http.StatusConflict, "egressProbeStale", err.Error())
	case errors.Is(err, repository.ErrConflict):
		response.Error(c, http.StatusConflict, "egressConflict", "名称已存在")
	case errors.Is(err, egressapp.ErrOperationsUnavailable):
		response.Error(c, http.StatusServiceUnavailable, "egressOperationsUnavailable", "代理运营功能暂不可用")
	case errors.Is(err, egressapp.ErrSubscriptionSync):
		response.Error(c, http.StatusBadGateway, "egressSubscriptionSyncFailed", "代理订阅同步失败")
	case errors.Is(err, egressapp.ErrClearanceUnavailable):
		response.Error(c, http.StatusConflict, "clearanceRefreshUnavailable", err.Error())
	case strings.Contains(err.Error(), "FlareSolverr") || strings.Contains(err.Error(), "Clearance"):
		// 固定文案:底层错误可能含 FlareSolverr URL/超时细节/内部地址, 细节
		// 在服务端日志留档（round 96 修复：此前承诺"详情见服务端日志"但
		// RefreshClearance 全链路无任何日志输出，承诺落空）, 不透传给管理端响应体。
		h.log().Warn("clearance_refresh_failed", "error", err, "request_id", c.Value(middleware.RequestIDKey))
		response.Error(c, http.StatusBadGateway, "clearanceRefreshFailed", "出口会话 Clearance 刷新失败，详情见服务端日志")
	default:
		// 服务层返回的语义化错误（如「出口轮换未启用」「节点未配置换 IP webhook」）
		// 此前被通用文案吞掉且不留痕——运维无从定位。保留对外通用文案，真实原因落日志。
		h.log().Error("egress_node_operation_failed", "error", err, "request_id", c.Value(middleware.RequestIDKey))
		response.Error(c, http.StatusInternalServerError, "egressNodeOperationFailed", "代理节点操作失败")
	}
}

func pathID(c *gin.Context) (uint64, bool) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	if err != nil || id == 0 {
		response.Error(c, http.StatusBadRequest, "invalidId", "ID 无效")
		return 0, false
	}
	return id, true
}
