package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	_ "github.com/chenyme/grok2api/backend/docs"
	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountsyncapp "github.com/chenyme/grok2api/backend/internal/application/accountsync"
	adminauthapp "github.com/chenyme/grok2api/backend/internal/application/adminauth"
	auditapp "github.com/chenyme/grok2api/backend/internal/application/audit"
	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	dashboardapp "github.com/chenyme/grok2api/backend/internal/application/dashboard"
	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
	updatecheckapp "github.com/chenyme/grok2api/backend/internal/application/updatecheck"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	accounthttp "github.com/chenyme/grok2api/backend/internal/transport/http/account"
	adminauthhttp "github.com/chenyme/grok2api/backend/internal/transport/http/adminauth"
	audithttp "github.com/chenyme/grok2api/backend/internal/transport/http/audit"
	clientkeyhttp "github.com/chenyme/grok2api/backend/internal/transport/http/clientkey"
	dashboardhttp "github.com/chenyme/grok2api/backend/internal/transport/http/dashboard"
	egresshttp "github.com/chenyme/grok2api/backend/internal/transport/http/egress"
	guardstatshttp "github.com/chenyme/grok2api/backend/internal/transport/http/guardstats"
	"github.com/chenyme/grok2api/backend/internal/transport/http/inference"
	mediahttp "github.com/chenyme/grok2api/backend/internal/transport/http/media"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	modelhttp "github.com/chenyme/grok2api/backend/internal/transport/http/model"
	settingshttp "github.com/chenyme/grok2api/backend/internal/transport/http/settings"
	systemhttp "github.com/chenyme/grok2api/backend/internal/transport/http/system"
	"github.com/gin-gonic/gin"
	swaggerFiles "github.com/swaggo/files"
	ginSwagger "github.com/swaggo/gin-swagger"
)

type Dependencies struct {
	Logger             *slog.Logger
	RequestTimeout     time.Duration
	MaxBodyBytes       int64
	TrustedProxies     []string
	ConcurrencyGate    *middleware.ConcurrencyGate
	SecureCookies      bool
	SwaggerEnabled     bool
	PublicAPIBaseURL   string
	FrontendStaticPath string
	// Readiness 返回可观测的分层就绪状态。Ready 仅为旧调用方保留。
	Readiness    func(context.Context) ReadinessSnapshot
	Ready        func(context.Context) bool
	TrafficReady func() bool
	AdminAuth    *adminauthapp.Service
	Accounts     *accountapp.Service
	AccountSync  *accountsyncapp.Service
	Models       *modelapp.Service
	ClientKeys   *clientkeyapp.Service
	Audits       *auditapp.Service
	Dashboard    *dashboardapp.Service
	Gateway      *gateway.Service
	Media        *mediaapp.Service
	Settings     *settingsapp.Service
	Egress       *egressapp.Service
	Updates      *updatecheckapp.Service
	AccountRisk  AccountRiskActions
}

// AccountRiskActions is the admin risk-check / patrol-run surface.
type AccountRiskActions interface {
	CheckAccount(ctx context.Context, id uint64) error
	RunDuePatrol(ctx context.Context) (int, error)
}

type ReadinessComponent struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
}

// ReadinessCredentialReport 表示可公开的启动凭据恢复统计。
type ReadinessCredentialReport struct {
	SchedulesBackfilled int `json:"schedulesBackfilled"`
	CriticalFound       int `json:"criticalFound"`
	Refreshed           int `json:"refreshed"`
	Failed              int `json:"failed"`
}

// ReadinessStartupReport 表示可公开的启动恢复统计，不包含内部错误文本。
type ReadinessStartupReport struct {
	StartedAt                time.Time                 `json:"startedAt"`
	CompletedAt              *time.Time                `json:"completedAt,omitempty"`
	Credentials              ReadinessCredentialReport `json:"credentials"`
	CooldownsRestored        int                       `json:"cooldownsRestored"`
	QuotaRecoveriesRestored  int                       `json:"quotaRecoveriesRestored"`
	DueWebQuotasQueued       int                       `json:"dueWebQuotasQueued"`
	StatsigKeysWarmed        int                       `json:"statsigKeysWarmed"`
	StaleWebQuotasFound      int                       `json:"staleWebQuotasFound"`
	StaleWebQuotasSynced     int                       `json:"staleWebQuotasSynced"`
	StaleModelCatalogsFound  int                       `json:"staleModelCatalogsFound"`
	StaleModelCatalogsSynced int                       `json:"staleModelCatalogsSynced"`
	ErrorCount               int                       `json:"errorCount"`
}

// ReadinessSnapshot 表示公开就绪端点的稳定响应契约。
type ReadinessSnapshot struct {
	Ready      bool                          `json:"ready"`
	State      string                        `json:"state"`
	UpdatedAt  time.Time                     `json:"updatedAt"`
	Components map[string]ReadinessComponent `json:"components,omitempty"`
	Startup    *ReadinessStartupReport       `json:"startup,omitempty"`
}

// New 创建完整 HTTP 路由并明确区分公共、管理员和客户端鉴权边界。
func New(deps Dependencies) *gin.Engine {
	if deps.ConcurrencyGate == nil {
		panic("httpserver: ConcurrencyGate 不能为空")
	}
	gin.SetMode(gin.ReleaseMode)
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	router := gin.New()
	if err := router.SetTrustedProxies(deps.TrustedProxies); err != nil {
		panic("httpserver: trustedProxies 配置无效: " + err.Error())
	}
	// 访问日志走专用异步 logger(有界队列+批量刷写):同步 JSON stdout 写
	// 的全局互斥锁与每请求一次 write 系统调用是高 QPS 下的入口串行点。
	// deps.Logger 仍供业务日志使用(同步、不丢关键错误)。
	router.Use(gin.Recovery(), middleware.RequestID(), middleware.ClientIP(), middleware.SecurityHeaders(), middleware.MaxBodyBytes(deps.MaxBodyBytes), middleware.Timeout(deps.RequestTimeout), middleware.Gzip(), middleware.AccessLog(middleware.AsyncAccessLogger()))
	// 错误方法此前落到 gin 默认 NoRoute（404 裸文本）：API 消费方无法区分
	// 「路径不存在」与「方法不对」。405 + 统一信封让两类错误可判别。
	router.HandleMethodNotAllowed = true
	router.NoMethod(func(c *gin.Context) { writeRouteError(c, http.StatusMethodNotAllowed) })
	router.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })
	router.GET("/readyz", func(c *gin.Context) {
		if deps.Readiness != nil {
			snapshot := deps.Readiness(c.Request.Context())
			status := http.StatusServiceUnavailable
			if snapshot.Ready {
				status = http.StatusOK
			}
			c.JSON(status, snapshot)
			return
		}
		if deps.Ready != nil && deps.Ready(c.Request.Context()) {
			c.JSON(http.StatusOK, gin.H{"ready": true, "state": "ready"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"ready": false, "state": "not_ready"})
	})
	if deps.SwaggerEnabled {
		router.GET("/swagger/*any", ginSwagger.WrapHandler(swaggerFiles.Handler))
	}
	mediaHandler := mediahttp.NewHandler(deps.Media)
	mediaHandler.RegisterPublic(router)

	adminRoot := router.Group("/api/admin/v1")
	authHandler := adminauthhttp.NewHandler(deps.AdminAuth, deps.SecureCookies)
	authHandler.RegisterPublic(adminRoot)
	adminProtected := adminRoot.Group("")
	adminProtected.Use(middleware.AdminAuth(deps.AdminAuth))
	authHandler.RegisterAuthenticated(adminProtected)
	accountHandler := accounthttp.NewHandler(deps.Accounts, deps.AccountSync, deps.Logger)
	accountHandler.SetRiskChecker(deps.AccountRisk)
	accountHandler.Register(adminProtected)
	modelhttp.NewHandler(deps.Models).Register(adminProtected)
	clientkeyhttp.NewHandler(deps.ClientKeys).Register(adminProtected)
	auditHandler := audithttp.NewHandler(deps.Audits)
	auditHandler.Register(adminProtected)
	dashboardhttp.NewHandler(deps.Dashboard).Register(adminProtected)
	mediaHandler.RegisterAdmin(adminProtected)
	settingsHandler := settingshttp.NewHandler(deps.Settings)
	settingsHandler.SetPatrolRunner(deps.AccountRisk)
	settingsHandler.Register(adminProtected)
	egressHandler := egresshttp.NewHandler(deps.Egress, deps.Logger)
	egressHandler.Register(adminProtected)
	guardstatshttp.NewHandler().Register(adminProtected)
	systemhttp.NewHandler(func() string {
		if deps.Settings != nil {
			return deps.Settings.PublicAPIBaseURL()
		}
		return deps.PublicAPIBaseURL
	}, deps.Updates).Register(adminProtected)

	v1 := router.Group("/v1")
	// 就绪门最前:启动恢复期间对所有流量(含未鉴权)返回 503, 语义与既有
	// 流量拒绝测试一致; 也是纯内存标记检查, 成本为零。
	if deps.TrafficReady != nil {
		v1.Use(func(c *gin.Context) {
			if deps.TrafficReady() {
				c.Next()
				return
			}
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
				"code": "service_reconciling", "message": "服务正在完成启动恢复，请稍后重试", "param": nil, "type": "server_error",
			}})
		})
	}
	// 鉴权先于并发闸门:闸门在鉴权前会为每个伪造 key 占住一个全局并发槽,
	// 无凭据流量即可把 1024 个槽耗尽, 令所有合法推理请求 503。先 401 伪请求,
	// 闸门槽位只留给已通过鉴权的流量。per-key 的 RPM/并发租约仍在闸门之后。
	v1.Use(middleware.ClientAuth(deps.ClientKeys))
	v1.Use(deps.ConcurrencyGate.Middleware())
	v1.Use(middleware.ObserveBodyMemory())
	inferenceHandler := inference.NewHandler(deps.Gateway, deps.Models, deps.MaxBodyBytes, deps.PublicAPIBaseURL)
	if deps.Settings != nil {
		inferenceHandler.SetPublicAPIBaseURLResolver(deps.Settings.PublicAPIBaseURL)
	}
	inferenceHandler.Register(v1)
	registerFrontend(router, deps.FrontendStaticPath)
	return router
}

// writeRouteError 以调用方所属面（OpenAI 兼容 /v1 或管理 API）的信封返回
// 路由级 404/405。/v1 与 middleware.writeOpenAIError 同口径；管理面复用
// response.Error（含 requestId，便于日志关联）。非后端路径不经过此函数。
func writeRouteError(c *gin.Context, status int) {
	if strings.HasPrefix(path.Clean("/"+c.Request.URL.Path), "/v1/") || c.Request.URL.Path == "/v1" {
		errorType := "invalid_request_error"
		if status >= 500 {
			errorType = "server_error"
		}
		code := "not_found"
		message := "未知请求路径: " + c.Request.URL.Path
		if status == http.StatusMethodNotAllowed {
			code = "method_not_allowed"
			message = "请求方法不被允许: " + c.Request.Method + " " + c.Request.URL.Path
		}
		c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": errorType, "code": code, "param": nil}})
		return
	}
	code, message := "notFound", "请求路径不存在: "+c.Request.URL.Path
	if status == http.StatusMethodNotAllowed {
		code, message = "methodNotAllowed", "请求方法不被允许: "+c.Request.Method+" "+c.Request.URL.Path
	}
	response.Error(c, status, code, message)
}
