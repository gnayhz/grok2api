// Package qualityhttp 是质量层管理面(B4 api 包的 HTTP/DTO 落点):
// 四入口——命中统计/路由守卫/IP 轮换/仲裁庭(G3;风险归因入口已
// 移除 G20,探针配置并入仲裁庭)。
package qualityhttp

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/enforcement"
	"github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

// Deps 是质量层管理面依赖(app 注入)。
type Deps struct {
	Queries     *management.Queries
	Court       *court.Service
	Enforcement *enforcement.Service
	Guard       *guard.Service
	// DialerDistribution 拨号选择分布(G10;nil=拨号策略未注入)。
	DialerDistribution func() map[string]map[uint64]uint64
	// Tunables 运行参数面(批7 面板可调承诺;nil=只读默认)。
	Tunables         TunablesStore
	RotationCapacity func() int
}

// Handler 暴露质量防护四入口。
type Handler struct {
	deps Deps
}

// NewHandler 构建质量层管理面。必填依赖缺席即组装错误:
// 立即崩溃并点名缺席字段——把"运行期偶发 nil panic 的 500"
// 前移为"启动期组装错误"(与中间件并发上限的 fail-fast 同例)。
// 可选依赖(DialerDistribution/Tunables)仍按 nil 容忍。
func NewHandler(deps Deps) *Handler {
	var missing []string
	if deps.Queries == nil {
		missing = append(missing, "Queries")
	}
	if deps.Court == nil {
		missing = append(missing, "Court")
	}
	if deps.Enforcement == nil {
		missing = append(missing, "Enforcement")
	}
	if deps.Guard == nil {
		missing = append(missing, "Guard")
	}
	if len(missing) > 0 {
		panic("qualityhttp: Deps 必填字段未注入: " + strings.Join(missing, ", "))
	}
	return &Handler{deps: deps}
}

// Register 挂载管理路由(管理鉴权由外层 adminProtected 提供)。
func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/quality/overview", h.getOverview)
	router.GET("/quality/court/cases", h.getCases)
	router.POST("/quality/court/review", h.postReview)
	router.POST("/quality/court/cases/:id/release", h.postReleaseCase)
	router.GET("/quality/guard", h.getGuard)
	router.PUT("/quality/guard", h.putGuard)
	router.DELETE("/quality/guard", h.resetGuard)
	router.GET("/quality/egress", h.getEgress)
	router.POST("/quality/egress/unban", h.postUnban)
	router.POST("/quality/egress/rotate", h.postRotate)
	router.GET("/quality/probes", h.getProbes)
	router.GET("/quality/evidence/matrix", h.getEvidenceMatrix)
	router.GET("/quality/proxy/distribution", h.getProxyDistribution)
	router.GET("/quality/settings", h.getSettings)
	router.PUT("/quality/settings", h.putSettings)
}

func (h *Handler) postReleaseCase(c *gin.Context) {
	id, err := strconv.ParseUint(c.Param("id"), 10, 64)
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err != nil || id == 0 || c.ShouldBindJSON(&req) != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", "case id and review reason required")
		return
	}
	if err := h.deps.Court.ReleaseAfterReview(c.Request.Context(), id, req.Reason); err != nil {
		response.Error(c, http.StatusBadRequest, "quality_review_failed", err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{"released": true})
}

// getProxyDistribution 池观测入口:拨号选择分布(G10 数据面)。
func (h *Handler) getProxyDistribution(c *gin.Context) {
	if h.deps.DialerDistribution == nil {
		response.Success(c, http.StatusOK, gin.H{"scopes": map[string]any{}})
		return
	}
	response.Success(c, http.StatusOK, gin.H{"scopes": h.deps.DialerDistribution()})
}

// getOverview 命中统计入口:案件/观测/台账汇总+证据局摘要。
func (h *Handler) getOverview(c *gin.Context) {
	value, err := h.deps.Queries.Overview(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_overview_failed", err.Error())
		return
	}
	check := gin.H{"outcome": value.GuardSelfCheck.Outcome}
	if value.GuardSelfCheck.Detail != "" {
		check["detail"] = value.GuardSelfCheck.Detail
	}
	payload := gin.H{
		"cases_total": value.CasesTotal, "cases_open": value.CasesOpen, "verdicts": value.Verdicts,
		"evidence": gin.H{"decidable": value.Evidence.Decidable, "degraded": value.Evidence.Degraded,
			"incidence": value.Evidence.Incidence, "nodes": value.Evidence.Nodes, "accounts": value.Evidence.Accounts},
		"observations_total": value.ObservationsTotal, "guard_self_check": check,
	}
	if value.ObservationDrops != nil {
		payload["observation_drops"] = *value.ObservationDrops
	}
	response.Success(c, http.StatusOK, payload)
}

func (h *Handler) selfCheckPayload() gin.H {
	if err := h.deps.Guard.SelfCheck(); err != nil {
		return gin.H{"outcome": "error", "detail": err.Error()}
	}
	return gin.H{"outcome": "ok"}
}

// caseDTO 案件面板投影。
type caseDTO struct {
	ID       uint64    `json:"id"`
	Status   string    `json:"status"`
	Verdict  string    `json:"verdict"`
	OpenedAt time.Time `json:"opened_at"`
	// ClosedAt 可空指针必须 omitempty:Go 会把 nil 序列化成 null,而
	// 前端 isOptional 只接受缺省(undefined)——null 直接解码失败
	// (批8 契约测试抓出的羁押名单空列表第二根因)。
	ClosedAt *time.Time `json:"closed_at,omitempty"`
	Parties  []partyDTO `json:"parties"`
	Evidence gin.H      `json:"evidence,omitempty"`
	// Live 实时证据面(在审案件):证据进度/在飞探针/等待原因。
	Live *court.LiveCaseView `json:"live,omitempty"`
}

type partyDTO struct {
	Kind        string `json:"kind"`
	AccountID   uint64 `json:"account_id"`
	NodeID      uint64 `json:"node_id"`
	Epoch       uint64 `json:"epoch"`
	Role        string `json:"role"`
	Disposition string `json:"disposition"`
}

// getCases 仲裁庭入口:羁押名单(开案)+裁决流(近期结案)。
func (h *Handler) getCases(c *gin.Context) {
	values, err := h.deps.Queries.Cases(c.Request.Context())
	if err != nil {
		code := "quality_cases_failed"
		if errors.Is(err, management.ErrCaseParties) {
			code = "quality_case_parties_failed"
		}
		response.Error(c, http.StatusInternalServerError, code, err.Error())
		return
	}
	dtos := make([]caseDTO, 0, len(values))
	for _, value := range values {
		dto := caseDTO{ID: value.ID, Status: string(value.Status), Verdict: string(value.Verdict),
			OpenedAt: value.OpenedAt, ClosedAt: value.ClosedAt, Parties: make([]partyDTO, 0, len(value.Parties)),
			Evidence: value.Evidence, Live: value.Live}
		for _, party := range value.Parties {
			dto.Parties = append(dto.Parties, partyDTO{Kind: string(party.Kind), AccountID: party.AccountID,
				NodeID: party.NodeID, Epoch: party.Epoch, Role: string(party.Role), Disposition: string(party.Disposition)})
		}
		dtos = append(dtos, dto)
	}
	response.Success(c, http.StatusOK, gin.H{"items": dtos})
}

// postReview 复审(G19:手动触发再评估——错判纠正通道)。
func (h *Handler) postReview(c *gin.Context) {
	stats, err := h.deps.Court.ReviewNow(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_review_failed", err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{
		"opened": stats.Opened, "convicted": stats.Convicted,
		"dismissed": stats.Dismissed, "withdrawn": stats.Withdrawn, "retried": stats.Retried,
	})
}

// getGuard 路由守卫入口:配置+自检+生效可见性(G13/I4)。
// getEgress IP 轮换入口:节点质量面(epoch 档案+台账总数 G8)+
// 节点型标注(webhook 型才可主动/批量轮换,G18 入口可见性)。
func (h *Handler) getEgress(c *gin.Context) {
	archive, err := h.deps.Queries.Egress(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_egress_failed", err.Error())
		return
	}
	items := make([]gin.H, 0, len(archive))
	for _, view := range archive {
		items = append(items, gin.H{
			"node_id": view.NodeID, "current_epoch": view.CurrentEpoch,
			"current_ip": view.CurrentIP, "state": string(view.State),
			"degrade_total": view.DegradeTotal, "degrade_detail": view.DegradeDetail,
			"webhook": view.Webhook,
		})
	}
	response.Success(c, http.StatusOK, gin.H{"nodes": items})
}

type nodeIDRequest struct {
	NodeID uint64 `json:"node_id" binding:"required"`
	Reason string `json:"reason" binding:"max=500"`
}

// postUnban 人工解禁(G7)。
func (h *Handler) postUnban(c *gin.Context) {
	var req nodeIDRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if err := h.deps.Enforcement.ManualUnban(c.Request.Context(), req.NodeID, req.Reason); err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_unban_failed", err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{"unbanned": true, "node_id": req.NodeID})
}

type rotateRequest struct {
	NodeIDs []uint64 `json:"node_ids" binding:"required"`
}

// postRotate 批量轮换(G18;限速内触发,超限如实报告)。
func (h *Handler) postRotate(c *gin.Context) {
	var req rotateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	triggered, rateLimited, err := h.deps.Enforcement.RotateNodes(c.Request.Context(), req.NodeIDs)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_rotate_failed", err.Error())
		return
	}
	response.Success(c, http.StatusOK, gin.H{"triggered": triggered, "rate_limited": rateLimited})
}

// getEvidenceMatrix 证据矩阵(仲裁庭核心视图):行=账号,列=出口
// (节点+epoch),格=窗口内观测构成(clean/degraded 计数)——"哪个
// 账号在哪个出口看到了什么"一眼可读,裁决依据不再是黑盒计数。
func (h *Handler) getEvidenceMatrix(c *gin.Context) {
	value, err := h.deps.Queries.Matrix(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_matrix_failed", err.Error())
		return
	}

	type accountRow struct {
		ID    uint64 `json:"id"`
		State string `json:"state"`
		Last  string `json:"last_at"`
	}
	type exitCol struct {
		Node  uint64 `json:"node"`
		Epoch uint64 `json:"epoch"`
		State string `json:"state"`
		Last  string `json:"last_at"`
	}
	type cell struct {
		AccountID uint64 `json:"a"`
		Node      uint64 `json:"n"`
		Epoch     uint64 `json:"e"`
		Clean     int    `json:"c"`
		Degraded  int    `json:"d"`
	}

	accounts := make([]accountRow, 0, len(value.Accounts))
	for _, row := range value.Accounts {
		accounts = append(accounts, accountRow{ID: row.ID, State: string(row.State), Last: row.LastAt.Format(time.RFC3339)})
	}
	exits := make([]exitCol, 0, len(value.Exits))
	for _, col := range value.Exits {
		exits = append(exits, exitCol{Node: col.Key.NodeID, Epoch: col.Key.Epoch, State: string(col.State), Last: col.LastAt.Format(time.RFC3339)})
	}
	cells := make([]cell, 0, len(value.Cells))
	for _, entry := range value.Cells {
		cells = append(cells, cell{AccountID: entry.AccountID, Node: entry.Exit.NodeID, Epoch: entry.Exit.Epoch, Clean: entry.Clean, Degraded: entry.Degraded})
	}
	response.Success(c, http.StatusOK, gin.H{"accounts": accounts, "exits": exits, "cells": cells})
}

// getProbes 仲裁庭探针配置面:任务队列观测。case_id 存在时返回该案件
// 的完整任务历史(不受全局最近窗口截断——案件详情的调查过程必须永远
// 可读,否则已结案件会退化成"无探针记录"的黑盒结论)。
func (h *Handler) getProbes(c *gin.Context) {
	limit, _ := strconv.Atoi(c.Query("limit"))
	var caseID uint64
	if raw := c.Query("case_id"); raw != "" {
		parsed, err := strconv.ParseUint(raw, 10, 64)
		if err != nil || parsed == 0 {
			response.Error(c, http.StatusBadRequest, "invalid_request", "case_id 必须是正整数")
			return
		}
		caseID = parsed
	}
	tasks, err := h.deps.Queries.Probes(c.Request.Context(), caseID, limit)
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "quality_probes_failed", err.Error())
		return
	}
	items := make([]gin.H, 0, len(tasks))
	for _, task := range tasks {
		item := gin.H{
			"id": task.ID, "case_id": task.CaseID, "direction": string(task.Direction),
			"experiment": task.Experiment,
			"defendant":  task.Defendant, "node_id": task.NodeID, "epoch": task.Epoch,
			// node_id/epoch are the comparison target for account
			// differentials; baseline_* is the original degraded path.
			"baseline_node_id": task.BaselineNodeID, "baseline_epoch": task.BaselineEpoch,
			"verified_ip_change": task.VerifiedIPChange,
			"control_account_id": task.ControlAccountID, "control_node_id": task.ControlNodeID, "control_epoch": task.ControlEpoch,
			"failure_kind": task.FailureKind, "path_key": task.PathKey, "control_outcome": task.ControlOutcome,
			"control_detail": task.ControlDetail, "control_path_key": task.ControlPathKey, "control_verified": task.ControlVerified,
			"juror": task.Juror, "state": string(task.State), "result": string(task.Result),
			"detail": task.Detail, "created_at": task.CreatedAt,
		}
		// finished_at 可空指针必须缺席而非 null(前端 isOptional 只认
		// undefined——批8 契约测试抓出)。
		if task.FinishedAt != nil {
			item["finished_at"] = task.FinishedAt
		}
		items = append(items, item)
	}
	response.Success(c, http.StatusOK, gin.H{"items": items})
}
