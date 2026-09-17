package media

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/chenyme/grok2api/backend/internal/transport/http/response"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	service     *mediaapp.Service
	importer    *mediaapp.ImageInputImporter
	ingestSlots chan struct{}
}

func NewHandler(service *mediaapp.Service, importer *mediaapp.ImageInputImporter) *Handler {
	return &Handler{service: service, importer: importer, ingestSlots: make(chan struct{}, ingestConcurrency)}
}

// RegisterPublic 在已挂好就绪门的 /v1/media 分组上注册公开媒体端点:
// 使用不可猜测资源 ID 的图片/视频读取与视频上传接收。
// 上传 PUT 不使用客户端 API key：xAI 无法携带，票据本身即授权。
// 路径相对分组前缀给出；调用方决定该分组还叠加哪些中间件。
func (h *Handler) RegisterPublic(router *gin.RouterGroup) {
	router.GET("/images/:assetId", h.getImage)
	router.HEAD("/images/:assetId", h.getImage)
	router.GET("/videos/:assetId", h.getVideo)
	router.HEAD("/videos/:assetId", h.getVideo)
	router.PUT("/uploads/:token", h.putVideoUpload)
}

// RegisterAdmin 注册管理端媒体列表和统计端点。
func (h *Handler) RegisterAdmin(router *gin.RouterGroup) {
	router.GET("/media/images", h.listImages)
	router.DELETE("/media/images", h.deleteImages)
	router.GET("/media/images/stats", h.imageStats)
	router.POST("/media/inputs/import", h.importInputImageFromURL)
	router.POST("/media/inputs/upload", h.uploadInputAsset)
	router.GET("/media/videos", h.listVideos)
	router.DELETE("/media/videos", h.deleteVideos)
	router.GET("/media/videos/stats", h.videoStats)
}

type deleteImagesRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

type deleteVideosRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

func (h *Handler) getImage(c *gin.Context) {
	asset, body, err := h.service.OpenImage(c.Request.Context(), c.Param("assetId"))
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()
	etag := `"` + asset.SHA256 + `"`
	if strings.TrimSpace(c.GetHeader("If-None-Match")) == etag {
		c.Header("ETag", etag)
		c.Status(http.StatusNotModified)
		return
	}
	c.Header("Content-Type", asset.MIMEType)
	c.Header("Content-Length", strconv.FormatInt(asset.SizeBytes, 10))
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", etag)
	c.Header("X-Content-Type-Options", "nosniff")
	if c.Request.Method == http.MethodHead {
		c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)
	_, _ = io.Copy(c.Writer, body)
}

func (h *Handler) getVideo(c *gin.Context) {
	asset, body, err := h.service.OpenVideo(c.Request.Context(), c.Param("assetId"))
	if errors.Is(err, mediaapp.ErrAssetNotFound) {
		c.Status(http.StatusNotFound)
		return
	}
	if err != nil {
		c.Status(http.StatusInternalServerError)
		return
	}
	defer func() { _ = body.Close() }()
	seeker, ok := body.(io.ReadSeeker)
	if !ok {
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Header("Content-Type", asset.MIMEType)
	c.Header("Content-Disposition", mediafile.VideoContentDisposition(asset.ID, asset.MIMEType))
	c.Header("Cache-Control", "public, max-age=31536000, immutable")
	c.Header("ETag", `"`+asset.SHA256+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	http.ServeContent(c.Writer, c.Request, asset.ID, asset.CreatedAt, seeker)
}

// putVideoUpload 接收 XAI ZDR 视频 PUT。响应与错误不得回显完整票据。
func (h *Handler) putVideoUpload(c *gin.Context) {
	_, err := h.service.ReceiveVideoUpload(c.Request.Context(), c.Param("token"), c.GetHeader("Content-Type"), c.Request.Body)
	switch {
	case err == nil:
		c.Status(http.StatusNoContent)
	case errors.Is(err, mediaapp.ErrUploadTicketNotFound):
		c.Status(http.StatusNotFound)
	case errors.Is(err, mediaapp.ErrUploadTicketExpired):
		c.Status(http.StatusGone)
	case errors.Is(err, mediaapp.ErrUploadTicketConsumed):
		c.Status(http.StatusConflict)
	case errors.Is(err, mediaapp.ErrVideoUploadTooLarge):
		// 体积超限优先于通用无效上传，返回 413。
		c.Status(http.StatusRequestEntityTooLarge)
	case errors.Is(err, mediaapp.ErrInvalidVideoUpload):
		c.Status(http.StatusBadRequest)
	case errors.Is(err, mediaapp.ErrUploadTicketsUnavailable):
		c.Status(http.StatusServiceUnavailable)
	default:
		c.Status(http.StatusInternalServerError)
	}
}

func (h *Handler) listImages(c *gin.Context) {
	page, pageSize := httphelpers.Pagination(c)
	assets, total, err := h.service.AdminListImages(c.Request.Context(), page, pageSize, c.Query("search"))
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaListImagesFailed", "读取图片列表失败")
		return
	}
	items := make([]mediaAssetDTO, 0, len(assets))
	for _, a := range assets {
		items = append(items, mediaAssetDTO{
			ID: a.ID, Kind: a.Kind, MimeType: a.MIMEType, SizeBytes: a.SizeBytes,
			SHA256: a.SHA256, CreatedAt: a.CreatedAt.Format("2006-01-02T15:04:05Z"),
			URL: h.service.PublicImageURL(a.ID),
		})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) imageStats(c *gin.Context) {
	stats, err := h.service.AdminImageStats(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaImageStatsFailed", "读取图片统计失败")
		return
	}
	response.Success(c, http.StatusOK, imageStatsDTO{TotalImages: stats.TotalImages, TotalBytes: stats.TotalBytes})
}

func (h *Handler) deleteImages(c *gin.Context) {
	var request deleteImagesRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	deleted, err := h.service.AdminDeleteImages(c.Request.Context(), request.IDs)
	if errors.Is(err, mediaapp.ErrInvalidImageSelection) {
		response.Error(c, http.StatusBadRequest, "invalidImageSelection", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaDeleteImagesFailed", "删除图片失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}

func (h *Handler) listVideos(c *gin.Context) {
	page, pageSize := httphelpers.Pagination(c)
	jobs, total, err := h.service.AdminListVideoJobs(c.Request.Context(), page, pageSize, c.Query("search"), c.Query("status"), repository.SortQuery{Field: c.Query("sortBy"), Direction: repository.SortDirection(c.Query("sortOrder"))})
	if errors.Is(err, mediaapp.ErrInvalidFilter) {
		response.Error(c, http.StatusBadRequest, "invalidFilter", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaListVideosFailed", "读取视频任务列表失败")
		return
	}
	items := make([]mediaJobDTO, 0, len(jobs))
	for _, j := range jobs {
		var completedAt *string
		assetID := ""
		if j.CompletedAt != nil {
			formatted := j.CompletedAt.Format("2006-01-02T15:04:05Z")
			completedAt = &formatted
		}
		if j.Status == "completed" {
			assetID = j.ResultAssetID
		}
		items = append(items, mediaJobDTO{
			ID: j.ID, Model: j.Model, Prompt: j.Prompt, Status: string(j.Status),
			Progress: j.Progress, Seconds: j.Seconds, Size: j.Size, Quality: j.Quality,
			AccountName: j.AccountName, ClientKeyName: j.ClientKeyName,
			CreatedAt:   j.CreatedAt.Format("2006-01-02T15:04:05Z"),
			CompletedAt: completedAt, ErrorMessage: j.ErrorMessage, AssetID: assetID,
		})
	}
	response.Success(c, http.StatusOK, gin.H{"items": items, "page": page, "pageSize": pageSize, "total": total})
}

func (h *Handler) videoStats(c *gin.Context) {
	stats, err := h.service.AdminVideoStats(c.Request.Context())
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaVideoStatsFailed", "读取视频统计失败")
		return
	}
	response.Success(c, http.StatusOK, videoStatsDTO{
		TotalJobs: stats.TotalJobs, Completed: stats.Completed, Failed: stats.Failed,
		InProgress: stats.InProgress, Queued: stats.Queued,
	})
}

func (h *Handler) deleteVideos(c *gin.Context) {
	var request deleteVideosRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	deleted, err := h.service.AdminDeleteVideoJobs(c.Request.Context(), request.IDs)
	if errors.Is(err, mediaapp.ErrInvalidVideoSelection) || errors.Is(err, mediaapp.ErrActiveVideoSelection) || errors.Is(err, mediaapp.ErrVideoCompletionPending) {
		response.Error(c, http.StatusBadRequest, "invalidVideoSelection", err.Error())
		return
	}
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaDeleteVideosFailed", "删除视频任务失败")
		return
	}
	response.Success(c, http.StatusOK, gin.H{"deleted": deleted})
}
