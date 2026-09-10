package media

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	mediaapp "github.com/chenyme/grok2api/backend/internal/application/media"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	"github.com/chenyme/grok2api/backend/internal/shared/response"
	"github.com/gin-gonic/gin"
)

const (
	ingestMaxImageBytes = mediadomain.MaxInputAssetBytes
	// Bound incoming upload buffers independently of inference traffic.
	ingestConcurrency = 4
)

type importImageRequest struct {
	URL string `json:"url" binding:"required,max=8192"`
}

// importInputImageFromURL 从管理员提供的 URL 抓取图片并登记到带 TTL 的隐藏输入区。
// 路由在 /api/admin/v1 下，已由 AdminAuth 保护；抓取带 SSRF 防护。
func (h *Handler) importInputImageFromURL(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	var request importImageRequest
	if bindErr := c.ShouldBindJSON(&request); bindErr != nil {
		response.Error(c, http.StatusBadRequest, "invalidRequest", "请求参数无效: "+bindErr.Error())
		return
	}
	asset, err := h.importer.Import(c.Request.Context(), request.URL)
	if err != nil {
		switch {
		case errors.Is(err, mediadomain.ErrInputImageURLInvalid):
			response.Error(c, http.StatusBadRequest, "invalidImageURL", err.Error())
		case errors.Is(err, mediadomain.ErrInputImageTooLarge):
			response.Error(c, http.StatusRequestEntityTooLarge, "imageTooLarge", "图片超过大小上限")
		case errors.Is(err, mediadomain.ErrInputImageURLBlocked):
			response.Error(c, http.StatusBadRequest, "imageURLBlocked", "该地址不允许访问")
		case errors.Is(err, mediadomain.ErrInputImageFetch):
			response.Error(c, http.StatusBadGateway, "imageFetchFailed", "下载图片失败")
		default:
			h.writeImageSaveError(c, err)
		}
		return
	}
	h.writeInputAsset(c, asset)
}

// uploadInputAsset 接收管理员上传的本地图片或视频（multipart 字段名 file），登记到临时输入区。
func (h *Handler) uploadInputAsset(c *gin.Context) {
	if !h.acquireIngest(c) {
		return
	}
	defer h.releaseIngest()
	fileHeader, err := c.FormFile("file")
	if err != nil {
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "文件超过请求大小上限")
			return
		}
		response.Error(c, http.StatusBadRequest, "invalidRequest", "缺少上传文件")
		return
	}
	if fileHeader.Size > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "文件超过大小上限")
		return
	}
	src, err := fileHeader.Open()
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaUploadReadFailed", "读取上传文件失败")
		return
	}
	defer func() { _ = src.Close() }()
	data, err := io.ReadAll(io.LimitReader(src, ingestMaxImageBytes+1))
	if err != nil {
		response.Error(c, http.StatusInternalServerError, "mediaUploadReadFailed", "读取上传文件失败")
		return
	}
	if int64(len(data)) > ingestMaxImageBytes {
		response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "文件超过大小上限")
		return
	}
	declaredMIME := strings.ToLower(strings.TrimSpace(strings.Split(fileHeader.Header.Get("Content-Type"), ";")[0]))
	detectedMIME := strings.ToLower(http.DetectContentType(data))
	if strings.HasPrefix(detectedMIME, "image/") {
		h.saveIngestedImage(c, data)
		return
	}
	if strings.HasPrefix(declaredMIME, "video/") || strings.HasPrefix(detectedMIME, "video/") {
		contentType := declaredMIME
		if contentType == "" {
			contentType = detectedMIME
		}
		asset, saveErr := h.service.SaveInputVideo(c.Request.Context(), contentType, bytes.NewReader(data))
		if saveErr != nil {
			switch {
			case errors.Is(saveErr, mediaapp.ErrVideoUploadTooLarge):
				response.Error(c, http.StatusRequestEntityTooLarge, "mediaTooLarge", "视频超过大小上限")
			case errors.Is(saveErr, mediaapp.ErrInvalidVideoUpload):
				response.Error(c, http.StatusBadRequest, "invalidVideo", "视频内容无效或格式不支持（mp4/webm/quicktime）")
			case errors.Is(saveErr, mediaapp.ErrMediaCapacity):
				response.Error(c, http.StatusInsufficientStorage, "mediaCapacityExceeded", "媒体临时存储容量不足")
			default:
				response.Error(c, http.StatusInternalServerError, "mediaSaveVideoFailed", "保存视频失败")
			}
			return
		}
		h.writeInputAsset(c, asset)
		return
	}
	response.Error(c, http.StatusBadRequest, "invalidMedia", "仅支持图片或视频文件")
}

// saveIngestedImage 收口两种临时输入路径：校验、落盘并登记 TTL，不进入图库。
func (h *Handler) saveIngestedImage(c *gin.Context, data []byte) {
	asset, err := h.service.SaveInputImage(c.Request.Context(), data)
	if err != nil {
		h.writeImageSaveError(c, err)
		return
	}
	h.writeInputAsset(c, asset)
}

func (h *Handler) writeImageSaveError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, mediaapp.ErrInvalidImage):
		response.Error(c, http.StatusBadRequest, "invalidImage", "图片内容无效或格式不支持（仅 jpeg/png/webp/gif）")
	case errors.Is(err, mediaapp.ErrMediaCapacity):
		response.Error(c, http.StatusInsufficientStorage, "mediaCapacityExceeded", "媒体临时存储容量不足")
	default:
		response.Error(c, http.StatusInternalServerError, "mediaSaveImageFailed", "保存图片失败")
	}
}

func (h *Handler) writeInputAsset(c *gin.Context, asset mediadomain.Asset) {
	expiresAt := ""
	if asset.ExpiresAt != nil {
		expiresAt = asset.ExpiresAt.Format(time.RFC3339)
	}
	response.Success(c, http.StatusCreated, gin.H{
		"fileId": asset.ID, "kind": asset.Kind, "mimeType": asset.MIMEType, "sizeBytes": asset.SizeBytes, "expiresAt": expiresAt,
	})
}

func (h *Handler) acquireIngest(c *gin.Context) bool {
	select {
	case h.ingestSlots <- struct{}{}:
		return true
	default:
		response.Error(c, http.StatusServiceUnavailable, "mediaIngestBusy", "媒体暂存并发已满，请稍后重试")
		return false
	}
}

func (h *Handler) releaseIngest() { <-h.ingestSlots }
