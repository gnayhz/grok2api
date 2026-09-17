package inference

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	clientkeydomain "github.com/chenyme/grok2api/backend/internal/domain/clientkey"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/pkg/mediafile"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/retryafter"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

type Handler struct {
	gateway          *gateway.Service
	models           *modelapp.Service
	maxBodyBytes     int64
	publicAPIBaseURL string
	publicBaseURL    func() string
}

const (
	responseCopyBufferBytes        = 32 << 10
	maxJSONMetadataInspectionBytes = 8 << 20
	maxStreamEventInspectionBytes  = 8 << 20
	// maxParsedSSEJSONBytes 是热路径上完整 json.Unmarshal 的上限。
	// grok-4.6 xhigh 推理结束会推数 MiB encrypted_content；把整行解成
	// map/结构体会在推理阶段把 CPU 和内存打满，而兼容补字段/用量抽取
	// 都不需要密文。未完成行仍按 maxStreamEventInspectionBytes 透传。
	maxParsedSSEJSONBytes           = 64 << 10
	maxStreamFailureDiagnosticBytes = 64 << 10
	maxCredentialErrorInspectBytes  = 64 << 10
	maxJSONResponseTransferBytes    = 128 << 20
	maxStreamResponseTransferBytes  = 256 << 20
	maxMediaResponseTransferBytes   = int64(2) << 30
)

var (
	errResponseTransferLimit    = errors.New("响应超过代理安全上限")
	errUpstreamStreamIncomplete = errors.New("上游流在终止事件前结束")
	errUpstreamStreamFailed     = errors.New("上游流返回失败终止事件")
	errUpstreamStreamRead       = errors.New("读取上游流失败")
	errClientStreamWrite        = errors.New("写入客户端流失败")
)

type streamProtocol uint8

const (
	streamProtocolResponses streamProtocol = iota
	streamProtocolChat
	streamProtocolAnthropic
	streamProtocolImage
)

const mediaTransferErrorTrailer = "X-Grok2API-Transfer-Error"

func NewHandler(gatewayService *gateway.Service, models *modelapp.Service, maxBodyBytes int64, publicAPIBaseURL ...string) *Handler {
	baseURL := ""
	if len(publicAPIBaseURL) > 0 {
		baseURL = strings.TrimRight(strings.TrimSpace(publicAPIBaseURL[0]), "/")
	}
	return &Handler{gateway: gatewayService, models: models, maxBodyBytes: maxBodyBytes, publicAPIBaseURL: baseURL}
}

// 请求体限额由入口中间件统一执行(server.go 全局 MaxBytesReader,同限额);
// 各 handler 此前再包一层内层 MaxBytesReader 是死重——内层永远先触发且不
// 增加保护,只多一层 Read 间接与每请求一次分配,已全部移除。
// SetPublicAPIBaseURLResolver makes video content URLs follow hot-updated runtime settings.
// Set it before Register; request handling only reads the resolver.
func (h *Handler) SetPublicAPIBaseURLResolver(resolve func() string) *Handler {
	h.publicBaseURL = resolve
	return h
}

func (h *Handler) Register(router *gin.RouterGroup) {
	router.GET("/models", h.listModels)
	router.POST("/responses", h.createResponse)
	router.POST("/chat/completions", h.createChatCompletion)
	router.POST("/messages", h.createMessage)
	router.POST("/images/generations", h.generateImage)
	router.POST("/images/edits", h.editImage)
	router.POST("/videos/generations", h.generateVideo)
	router.POST("/videos/edits", h.editVideo)
	router.POST("/videos/extensions", h.extendVideo)
	router.GET("/videos/:requestId", h.getVideo)
	router.GET("/videos/:requestId/content", h.getVideoContent)
	router.POST("/tts", h.synthesizeSpeech)
	router.GET("/tts/voices", h.listTTSVoices)
	router.GET("/tts/voices/:voiceId", h.getTTSVoice)
	router.POST("/stt", h.transcribeSpeech)
	router.GET("/stt", h.proxySTTWebSocket)
	// OpenAI-compatible audio aliases for common client SDKs.
	router.POST("/audio/speech", h.synthesizeOpenAISpeech)
	router.POST("/audio/tasks", h.synthesizeOpenAIAudioTask)
	router.POST("/audio/transcriptions", h.transcribeOpenAIAudio)
	router.GET("/realtime", h.proxyRealtimeWebSocket)
	router.POST("/responses/compact", h.compactResponse)
	router.GET("/responses/:responseId", h.getResponse)
	router.DELETE("/responses/:responseId", h.deleteResponse)
}

type responsesRequest struct {
	Model              string `json:"model"`
	Stream             bool   `json:"stream"`
	PromptCacheKey     string `json:"prompt_cache_key"`
	PreviousResponseID string `json:"previous_response_id"`
	Store              *bool  `json:"store"`
}

type chatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       json.RawMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	PromptCacheKey string          `json:"prompt_cache_key"`
}

type messagesRequest struct {
	Model          string          `json:"model"`
	MaxTokens      *int            `json:"max_tokens"`
	Messages       json.RawMessage `json:"messages"`
	Stream         bool            `json:"stream"`
	PromptCacheKey string          `json:"prompt_cache_key"`
}

type imageGenerationRequest struct {
	Model          string          `json:"model"`
	Prompt         string          `json:"prompt"`
	Count          *int            `json:"n"`
	PartialImages  *int            `json:"partial_images"`
	Size           string          `json:"size"`
	AspectRatio    string          `json:"aspect_ratio"`
	Resolution     string          `json:"resolution"`
	Quality        string          `json:"quality"`
	ResponseFormat string          `json:"response_format"`
	StorageOptions json.RawMessage `json:"storage_options"`
	Stream         bool            `json:"stream"`
}

type imageEditJSONImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type imageEditJSONRequest struct {
	Model          string               `json:"model"`
	Prompt         string               `json:"prompt"`
	Image          *imageEditJSONImage  `json:"image"`
	Images         []imageEditJSONImage `json:"images"`
	Count          *int                 `json:"n"`
	Size           string               `json:"size"`
	AspectRatio    string               `json:"aspect_ratio"`
	Resolution     string               `json:"resolution"`
	Quality        string               `json:"quality"`
	ResponseFormat string               `json:"response_format"`
	StorageOptions json.RawMessage      `json:"storage_options"`
	Stream         bool                 `json:"stream"`
	PartialImages  *int                 `json:"partial_images"`
}

type videoGenerationImage struct {
	URL    string `json:"url"`
	FileID string `json:"file_id"`
}

type videoGenerationAudio struct {
	VoiceID string `json:"voice_id"`
}

type videoGenerationRequest struct {
	Model           string                 `json:"model"`
	Prompt          string                 `json:"prompt"`
	User            *string                `json:"user"`
	Duration        json.RawMessage        `json:"duration"`
	AspectRatio     string                 `json:"aspect_ratio"`
	Resolution      string                 `json:"resolution"`
	Image           *videoGenerationImage  `json:"image"`
	ReferenceImages []videoGenerationImage `json:"reference_images"`
	ReferenceAudios []videoGenerationAudio `json:"reference_audios"`
	Video           *videoGenerationImage  `json:"video"`
	Output          json.RawMessage        `json:"output"`
	StorageOptions  json.RawMessage        `json:"storage_options"`
}

type modelListItem struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
}

func (h *Handler) listModels(c *gin.Context) {
	var key *clientkeydomain.Key
	if value, exists := c.Get(middleware.ClientKey); exists {
		if subject, ok := value.(clientkeydomain.Key); ok {
			key = &subject
		}
	}
	products, err := h.models.ListPublic(c.Request.Context(), key)
	if err != nil {
		writeOpenAIError(c, http.StatusInternalServerError, "model_list_failed", "读取模型列表失败")
		return
	}
	if strings.TrimSpace(c.Query("client_version")) != "" {
		writeCodexModelCatalog(c, newCodexModelCatalog(products))
		return
	}
	c.JSON(http.StatusOK, gin.H{"object": "list", "data": newModelListItems(products)})
}

func newModelListItems(products []modeldomain.PublicModel) []modelListItem {
	items := make([]modelListItem, 0, len(products))
	for _, product := range products {
		items = append(items, modelListItem{ID: product.ID, Object: "model", Created: product.CreatedAt.Unix(), OwnedBy: "grok2api"})
	}
	return items
}

func (h *Handler) createResponse(c *gin.Context) {
	h.handleCreate(c, false)
}

func (h *Handler) compactResponse(c *gin.Context) {
	h.handleCreate(c, true)
}

func (h *Handler) createChatCompletion(c *gin.Context) {
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Chat Completions only supports application/json")
		return
	}
	body, err := readRequestBody(c, h.maxBodyBytes)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request chatCompletionRequest
	// JSON 语法错误与缺字段分开提示: 合并会让非法 JSON 得到「缺少有效
	// model」的误导信息(round 64 活体复现), 与 OpenAI 的 parse-error 语义对齐。
	if unmarshalErr := json.Unmarshal(body, &request); unmarshalErr != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "请求体不是有效的 JSON: "+unmarshalErr.Error())
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Chat Completions 请求缺少有效 model")
		return
	}
	// 空 messages 属客户端错误：不发往上游（否则返回误导性的 upstream_server_error）
	if len(request.Messages) == 0 || string(bytes.TrimSpace(request.Messages)) == "[]" || string(bytes.TrimSpace(request.Messages)) == "null" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Chat Completions 请求缺少有效消息")
		return
	}
	clientKey, requestIDValue, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.CreateChatCompletion(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		SessionSignals: extractClientSignals(c.Request.Header, body),
		GrokTurnIndex:  c.GetHeader("x-grok-turn-idx"),
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolChat)
}

func (h *Handler) createMessage(c *gin.Context) {
	if !isJSONRequest(c) {
		writeAnthropicError(c, http.StatusUnsupportedMediaType, "invalid_request_error", "Messages only supports application/json")
		return
	}
	if strings.TrimSpace(c.GetHeader("anthropic-version")) == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "anthropic-version header is required")
		return
	}
	body, err := readRequestBody(c, h.maxBodyBytes)
	if err != nil {
		writeAnthropicError(c, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body exceeds the configured limit")
		return
	}
	var request messagesRequest
	if unmarshalErr := json.Unmarshal(body, &request); unmarshalErr != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "request body is not valid JSON: "+unmarshalErr.Error())
		return
	}
	if strings.TrimSpace(request.Model) == "" || request.MaxTokens == nil || *request.MaxTokens <= 0 || len(bytes.TrimSpace(request.Messages)) == 0 {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model, max_tokens, and messages are required")
		return
	}
	// 提取逻辑共用 clientIdentity；Messages 面的 401 必须保持 Anthropic
	// 信封与英文文案（与 OpenAI 面的 requestIdentity 不同）。
	clientKey, _, ok := clientIdentity(c)
	if !ok {
		writeAnthropicError(c, http.StatusUnauthorized, "authentication_error", "invalid API key")
		return
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	result, err := h.gateway.CreateMessage(c.Request.Context(), gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		SessionSignals: extractClientSignals(c.Request.Header, body),
		GrokTurnIndex:  c.GetHeader("x-grok-turn-idx"),
	})
	if err != nil {
		writeGatewayAnthropicError(c, err)
		return
	}
	h.writeAnthropicResult(c, result, request.Stream)
}

// validateImageStorageOptions 是生成/编辑共用的 storage_options 拒绝。
func validateImageStorageOptions(c *gin.Context, storageOptions []byte) bool {
	if value := bytes.TrimSpace(storageOptions); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return false
	}
	return true
}

// validateImageCount 校验生成/编辑共用的 n 范围;nil 视为 1。
func validateImageCount(c *gin.Context, count *int) (int, bool) {
	value := 1
	if count != nil {
		if *count < 1 || *count > 10 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "n 必须在 1 到 10 之间")
			return 0, false
		}
		value = *count
	}
	return value, true
}

// validatePartialImagesAndQuality 校验生成/编辑共用的 partial_images 与
// quality 口径。编辑入口另在本地校验 aspect_ratio/size/resolution;生成
// 入口按兼容合同把这些字段原样交给 gateway,不在传输层收紧(两处口径
// 差异是有意的,勿在未评估 API 兼容前对齐)。
func validatePartialImagesAndQuality(c *gin.Context, partialImages *int, stream bool, quality string) (int, string, bool) {
	partial := 0
	if partialImages != nil {
		if *partialImages < 0 || *partialImages > 3 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 必须在 0 到 3 之间")
			return 0, "", false
		}
		partial = *partialImages
		if partial > 0 && !stream {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "partial_images 仅可在 stream=true 时使用")
			return 0, "", false
		}
	}
	normalized := strings.ToLower(strings.TrimSpace(quality))
	if normalized != "" && normalized != "low" && normalized != "medium" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "quality 必须是 low 或 medium")
		return 0, "", false
	}
	return partial, normalized, true
}

func (h *Handler) generateImage(c *gin.Context) {
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片生成仅支持 application/json")
		return
	}
	var request imageGenerationRequest
	if decodeSingleJSON(c.Request.Body, &request, false) != nil || strings.TrimSpace(request.Model) == "" || strings.TrimSpace(request.Prompt) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片请求缺少有效 model 或 prompt")
		return
	}
	if !validateImageStorageOptions(c, request.StorageOptions) {
		return
	}
	count, ok := validateImageCount(c, request.Count)
	if !ok {
		return
	}
	if request.Stream && count != 1 {
		writeImageGenerationUserError(c, "unsupported_parameter", "input", "Streaming is only supported with n=1.")
		return
	}
	partialImages, quality, ok := validatePartialImagesAndQuality(c, request.PartialImages, request.Stream, request.Quality)
	if !ok {
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.GenerateImage(c.Request.Context(), gateway.ImageGenerationInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: request.Model, Prompt: request.Prompt,
		Count: count, Size: request.Size, AspectRatio: request.AspectRatio,
		Resolution: request.Resolution, Quality: quality, ResponseFormat: request.ResponseFormat,
		Streaming: request.Stream, PartialImages: partialImages,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func (h *Handler) editImage(c *gin.Context) {
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "图片编辑仅支持 application/json")
		return
	}
	var request imageEditJSONRequest
	if err := decodeSingleJSON(c.Request.Body, &request, false); err != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑 JSON 请求无效")
		return
	}
	if !validateImageStorageOptions(c, request.StorageOptions) {
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	count, ok := validateImageCount(c, request.Count)
	if !ok {
		return
	}
	inputs := append([]imageEditJSONImage(nil), request.Images...)
	if request.Image != nil {
		inputs = append([]imageEditJSONImage{*request.Image}, inputs...)
	}
	if len(inputs) == 0 || len(inputs) > 8 {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "image 或 images 数量必须在 1 到 8 之间")
		return
	}
	imageURLs := make([]string, 0, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.FileID) != "" {
			writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前暂不支持 image.file_id，请使用 image.url")
			return
		}
		if value := strings.TrimSpace(input.URL); value != "" {
			imageURLs = append(imageURLs, value)
		}
	}
	if len(imageURLs) != len(inputs) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "每个 image 都必须提供有效 url")
		return
	}
	if model == "" || prompt == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "图片编辑缺少有效 model 或 prompt")
		return
	}
	partialImages, quality, ok := validatePartialImagesAndQuality(c, request.PartialImages, request.Stream, request.Quality)
	if !ok {
		return
	}
	aspectRatio := strings.ToLower(strings.TrimSpace(request.AspectRatio))
	size := strings.ToLower(strings.TrimSpace(request.Size))
	if aspectRatio != "" && !validImageAspectRatio(aspectRatio) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "aspect_ratio 不受支持")
		return
	}
	if size != "" && !validImageEditSize(size) {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "size 必须是 auto、1024x1024、1024x1536 或 1536x1024")
		return
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" && resolution != "2k" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_parameter", "resolution 必须是 1k 或 2k")
		return
	}
	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	result, err := h.gateway.EditImage(c.Request.Context(), gateway.ImageEditInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model, Prompt: prompt,
		ImageURLs: imageURLs, Count: count, Size: size, AspectRatio: aspectRatio,
		Resolution: resolution, Quality: quality, ResponseFormat: request.ResponseFormat,
		Streaming: request.Stream, PartialImages: partialImages,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResult(c, result, request.Stream, streamProtocolImage)
}

func (h *Handler) writeResult(c *gin.Context, result *gateway.Result, stream bool, protocol streamProtocol) {
	h.writeProtocolResult(c, result, stream, false, protocol, "")
}

// clientIdentity 只提取中间件写入的客户端 Key 与请求 ID，不写响应。
func clientIdentity(c *gin.Context) (clientkeydomain.Key, string, bool) {
	clientValue, exists := c.Get(middleware.ClientKey)
	clientKey, ok := clientValue.(clientkeydomain.Key)
	if !exists || !ok {
		return clientkeydomain.Key{}, "", false
	}
	requestID, _ := c.Get(middleware.RequestIDKey)
	requestIDValue, _ := requestID.(string)
	return clientKey, requestIDValue, true
}

// requestIdentity 提取客户端身份；缺失时写出 OpenAI 面 401 信封。
func requestIdentity(c *gin.Context) (clientkeydomain.Key, string, bool) {
	clientKey, requestIDValue, ok := clientIdentity(c)
	if !ok {
		writeOpenAIError(c, http.StatusUnauthorized, "invalid_api_key", "客户端 API Key 无效")
		return clientkeydomain.Key{}, "", false
	}
	return clientKey, requestIDValue, true
}

func (h *Handler) generateVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationGenerate, "视频生成")
}

func (h *Handler) editVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationEdit, "视频编辑")
}

func (h *Handler) extendVideo(c *gin.Context) {
	h.handleVideoCreate(c, gatewayVideoOperationExtend, "视频延长")
}

const (
	gatewayVideoOperationGenerate = "generate"
	gatewayVideoOperationEdit     = "edit"
	gatewayVideoOperationExtend   = "extend"
)

func (h *Handler) handleVideoCreate(c *gin.Context, operation, label string) {
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", label+"仅支持 application/json")
		return
	}
	var request videoGenerationRequest
	if err := decodeSingleJSON(c.Request.Body, &request, true); err != nil {
		// 保留 unknown field 细节:视频端点用 DisallowUnknownFields 显式拒绝
		// 不支持参数, 客户端依赖错误里的字段名(handler_test 锁定该契约)。
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+" JSON 请求无效: "+err.Error())
		return
	}
	if hasJSONValue(request.Output) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 output.upload_url")
		return
	}
	if hasJSONValue(request.StorageOptions) {
		writeOpenAIError(c, http.StatusBadRequest, "unsupported_parameter", "当前兼容层暂不支持 storage_options")
		return
	}
	model := strings.TrimSpace(request.Model)
	prompt := strings.TrimSpace(request.Prompt)
	if model == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"缺少有效 model")
		return
	}
	parseVideoImage := func(input videoGenerationImage, field string) (string, bool) {
		urlValue := strings.TrimSpace(input.URL)
		fileID := strings.TrimSpace(input.FileID)
		if (urlValue == "") == (fileID == "") {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", field+" 必须且只能提供 url 或 file_id")
			return "", false
		}
		if fileID != "" {
			if !mediadomain.IsInputAssetID(fileID) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", field+".file_id 无效")
				return "", false
			}
			return gateway.VideoInputFileReference(fileID), true
		}
		return urlValue, true
	}

	duration := 0
	aspectRatio := ""
	resolution := ""
	imageURL := ""
	referenceURLs := []string{}
	referenceAudios := []string{}
	videoURL := ""

	if operation == gatewayVideoOperationGenerate {
		var err error
		duration, err = parseVideoDuration(request.Duration)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		aspectRatio = strings.TrimSpace(request.AspectRatio)
		if aspectRatio == "" {
			aspectRatio = "16:9"
		}
		if !validVideoAspectRatio(aspectRatio) {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "aspect_ratio 必须是 1:1、16:9、9:16、4:3、3:4、3:2 或 2:3")
			return
		}
		resolution = strings.ToLower(strings.TrimSpace(request.Resolution))
		if resolution == "" {
			resolution = "720p"
		}
		if resolution != "480p" && resolution != "720p" && resolution != "1080p" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "resolution 必须是 480p、720p 或 1080p")
			return
		}
		if request.Image != nil {
			value, ok := parseVideoImage(*request.Image, "image")
			if !ok {
				return
			}
			imageURL = value
		}
		referenceURLs = make([]string, 0, len(request.ReferenceImages))
		for _, input := range request.ReferenceImages {
			value, ok := parseVideoImage(input, "reference_images")
			if !ok {
				return
			}
			referenceURLs = append(referenceURLs, value)
		}
		referenceAudios = make([]string, 0, len(request.ReferenceAudios))
		for i, input := range request.ReferenceAudios {
			voiceID := strings.TrimSpace(input.VoiceID)
			if voiceID == "" {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", fmt.Sprintf("reference_audios[%d].voice_id 不能为空", i))
				return
			}
			referenceAudios = append(referenceAudios, voiceID)
		}
		// 组合约束(互斥/必填/上限/分辨率)与 gateway 用例共用
		// domain/media 的唯一规则,错误文本即对外消息。
		if err := mediadomain.ValidateVideoGenerationInput(imageURL != "", len(referenceURLs), len(referenceAudios), prompt != "", resolution); err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if request.Video != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频生成不支持 video 输入")
			return
		}
	} else {
		if prompt == "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"必须提供 prompt")
			return
		}
		if request.Video == nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"必须提供 video")
			return
		}
		value, ok := parseVideoImage(*request.Video, "video")
		if !ok {
			return
		}
		videoURL = value
		if request.Image != nil || len(request.ReferenceImages) > 0 || len(request.ReferenceAudios) > 0 {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"不支持 image、reference_images 或 reference_audios")
			return
		}
		if strings.TrimSpace(request.AspectRatio) != "" || strings.TrimSpace(request.Resolution) != "" {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", label+"不支持 aspect_ratio 或 resolution")
			return
		}
		if operation == gatewayVideoOperationEdit {
			if hasJSONValue(request.Duration) {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频编辑不支持 duration")
				return
			}
		} else {
			// extend: duration optional, default 6, range 2-10
			if hasJSONValue(request.Duration) {
				var err error
				duration, err = parseVideoDuration(request.Duration)
				if err != nil {
					writeOpenAIError(c, http.StatusBadRequest, "invalid_request", err.Error())
					return
				}
			} else {
				duration = 6
			}
			if duration < 2 || duration > 10 {
				writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "视频延长 duration 必须在 2 到 10 秒之间")
				return
			}
		}
	}

	clientKey, requestID, ok := requestIdentity(c)
	if !ok {
		return
	}
	var op mediadomain.VideoOperation
	switch operation {
	case gatewayVideoOperationEdit:
		op = mediadomain.VideoOperationEdit
	case gatewayVideoOperationExtend:
		op = mediadomain.VideoOperationExtend
	default:
		op = mediadomain.VideoOperationGenerate
	}
	job, err := h.gateway.CreateVideo(c.Request.Context(), gateway.VideoInput{
		RequestID: requestID, ClientKey: clientKey, PublicModel: model,
		Operation: op,
		Prompt:    prompt, Duration: duration, AspectRatio: aspectRatio, Resolution: resolution,
		ImageURL: imageURL, ReferenceURLs: referenceURLs, ReferenceAudios: referenceAudios, VideoURL: videoURL,
	})
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"request_id": job.ID})
}

func (h *Handler) getVideo(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	job, err := h.gateway.GetVideo(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	c.JSON(http.StatusOK, videoGenerationResponse(job, h.videoPlaybackURL(job)))
}

func (h *Handler) publicURL(path string) string {
	baseURL := h.publicAPIBaseURL
	if h.publicBaseURL != nil {
		baseURL = strings.TrimRight(strings.TrimSpace(h.publicBaseURL()), "/")
	}
	if baseURL == "" {
		return path
	}
	return baseURL + path
}

func (h *Handler) videoContentURL(jobID string) string {
	return h.publicURL("/v1/videos/" + url.PathEscape(jobID) + "/content")
}

// videoPlaybackURL prefers the stored asset served by the public media route, so the
// returned link opens directly in browsers and players. /v1/videos/{id}/content needs
// the client API key, which makes the URL unusable outside an authenticated client.
// Images already return their public media URL; this keeps video consistent. Jobs
// without a stored asset keep the protected content endpoint.
func (h *Handler) videoPlaybackURL(job mediadomain.Job) string {
	if assetID := strings.TrimSpace(job.ResultAssetID); assetID != "" {
		return h.publicURL("/v1/media/videos/" + url.PathEscape(assetID))
	}
	return h.videoContentURL(job.ID)
}

func (h *Handler) getVideoContent(c *gin.Context) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	body, contentType, size, err := h.gateway.OpenVideoContent(c.Request.Context(), strings.TrimSpace(c.Param("requestId")), clientKey)
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	defer func() { _ = body.Close() }()
	writeVideoContent(c, body, contentType, size, strings.TrimSpace(c.Param("requestId")))
}

func writeVideoContent(c *gin.Context, body io.Reader, contentType string, size int64, downloadName string) {
	if size > maxMediaResponseTransferBytes {
		writeOpenAIError(c, http.StatusBadGateway, "media_too_large", "上游媒体超过 2 GiB 安全上限")
		return
	}
	contentType, ok := normalizeVideoResponseContentType(contentType)
	if !ok {
		writeOpenAIError(c, http.StatusBadGateway, "invalid_media_type", "上游视频服务返回了不受支持的内容类型")
		return
	}
	// Clients that save the response need an extension to get a playable file.
	c.Header("Content-Disposition", mediafile.VideoContentDisposition(downloadName, contentType))
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Header("Referrer-Policy", "no-referrer")
	if size >= 0 {
		c.Header("Content-Length", strconv.FormatInt(size, 10))
	} else {
		c.Header("Trailer", mediaTransferErrorTrailer)
	}
	if err := writeMediaBody(c, body, contentType, http.StatusOK, maxMediaResponseTransferBytes); err != nil && size < 0 {
		errorCode := "stream_interrupted"
		if errors.Is(err, errResponseTransferLimit) {
			errorCode = "response_too_large"
		}
		c.Header(mediaTransferErrorTrailer, errorCode)
	}
}

func normalizeVideoResponseContentType(value string) (string, bool) {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	switch strings.ToLower(mediaType) {
	case "video/mp4", "video/quicktime", "video/webm":
		return strings.ToLower(mediaType), true
	default:
		return "", false
	}
}

func parseVideoDuration(durationRaw json.RawMessage) (int, error) {
	duration, hasDuration, err := parseOptionalVideoInteger(durationRaw)
	if err != nil {
		return 0, fmt.Errorf("duration 必须是整数或整数字符串")
	}
	value := 8
	if hasDuration {
		value = duration
	}
	if value < 1 || value > 15 {
		return 0, fmt.Errorf("duration 必须在 1 到 15 秒之间")
	}
	return value, nil
}

func parseOptionalVideoInteger(raw json.RawMessage) (int, bool, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return 0, false, nil
	}
	var number int
	if json.Unmarshal(raw, &number) != nil {
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil {
			return 0, true, errors.New("必须是整数或整数字符串")
		}
		number = parsed
	}
	return number, true, nil
}

func hasJSONValue(value json.RawMessage) bool {
	trimmed := bytes.TrimSpace(value)
	return len(trimmed) > 0 && !bytes.Equal(trimmed, []byte("null"))
}

func validVideoAspectRatio(value string) bool {
	switch value {
	case "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3":
		return true
	default:
		return false
	}
}

func validImageAspectRatio(value string) bool {
	switch value {
	case "auto", "1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3", "2:1", "1:2", "19.5:9", "9:19.5", "20:9", "9:20":
		return true
	default:
		return false
	}
}

func validImageEditSize(value string) bool {
	switch value {
	case "auto", "1024x1024", "1024x1536", "1536x1024":
		return true
	default:
		return false
	}
}

func videoGenerationResponse(job mediadomain.Job, contentURLs ...string) gin.H {
	switch job.Status {
	case mediadomain.StatusCompleted:
		videoURL := job.UpstreamURL
		if len(contentURLs) > 0 && contentURLs[0] != "" {
			videoURL = contentURLs[0]
		}
		video := gin.H{"url": videoURL, "respect_moderation": true}
		operation := job.Operation
		if operation == "" {
			operation = mediadomain.VideoOperationGenerate
		}
		if operation == mediadomain.VideoOperationGenerate && job.Seconds > 0 {
			video["duration"] = job.Seconds
		}
		return gin.H{
			"status": "done", "model": job.Model, "progress": 100,
			"video": video,
		}
	case mediadomain.StatusFailed:
		return gin.H{
			"status": "failed",
			"error":  gin.H{"code": officialVideoErrorCode(job.ErrorCode), "message": job.ErrorMessage},
		}
	default:
		return gin.H{"status": "pending", "model": job.Model, "progress": min(99, max(0, job.Progress))}
	}
}

func officialVideoErrorCode(value string) string {
	switch value {
	case "account_unavailable", "provider_unavailable":
		return "service_unavailable"
	case "model_not_found":
		return "invalid_argument"
	default:
		return "internal_error"
	}
}

func (h *Handler) handleCreate(c *gin.Context, compact bool) {
	if !isJSONRequest(c) {
		writeOpenAIError(c, http.StatusUnsupportedMediaType, "invalid_request", "Responses only supports application/json")
		return
	}
	body, err := readRequestBody(c, h.maxBodyBytes)
	if err != nil {
		writeOpenAIError(c, http.StatusRequestEntityTooLarge, "request_too_large", "请求体超过限制")
		return
	}
	var request responsesRequest
	if unmarshalErr := json.Unmarshal(body, &request); unmarshalErr != nil {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "请求体不是有效的 JSON: "+unmarshalErr.Error())
		return
	}
	if strings.TrimSpace(request.Model) == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Responses 请求缺少有效 model")
		return
	}
	if compact {
		body, err = forceJSONBoolean(body, "stream", false)
		if err != nil {
			writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "Compact 请求格式无效")
			return
		}
		request.Stream = false
	}
	clientKey, requestIDValue, ok := requestIdentity(c)
	if !ok {
		return
	}
	input := gateway.Input{
		RequestID: requestIDValue, ClientKey: clientKey, PublicModel: request.Model,
		Body: body, Streaming: request.Stream, PromptCacheKey: request.PromptCacheKey,
		SessionSignals: extractClientSignals(c.Request.Header, body), PreviousResponseID: request.PreviousResponseID,
		StoreResponse: request.Store,
		GrokTurnIndex: c.GetHeader("x-grok-turn-idx"),
	}
	var result *gateway.Result
	if compact {
		result, err = h.gateway.CompactResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.CreateResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	h.writeResponsesResult(c, result, request.Stream && !compact, request.Model)
}

// readRequestBody 读取完整请求体。ContentLength 已知时按声明值预分配——
// io.ReadAll 的 512B 起步倍增在 1MB 体上要 ~12 次扩容拷贝(峰值瞬态
// ~2×);推理请求体必读完整,预分配是纯增益。声明值超过入口限额时
// 只按限额分配(超限读取会由 MaxBytesReader 以错误终止,大预分配纯属
// 浪费);ContentLength 未知(chunked)回退 ReadAll。
func readRequestBody(c *gin.Context, limit int64) ([]byte, error) {
	if c.Request == nil || c.Request.Body == nil || c.Request.ContentLength <= 0 {
		return io.ReadAll(c.Request.Body)
	}
	size := c.Request.ContentLength
	if limit > 0 && size > limit+1 {
		size = limit + 1
	}
	buffer := bytes.NewBuffer(make([]byte, 0, int(size)))
	if _, err := io.Copy(buffer, c.Request.Body); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func isJSONRequest(c *gin.Context) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func decodeSingleJSON(reader io.Reader, target any, disallowUnknown bool) error {
	decoder := json.NewDecoder(reader)
	if disallowUnknown {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("请求体只能包含一个 JSON 对象")
		}
		return err
	}
	return nil
}

func (h *Handler) getResponse(c *gin.Context) {
	h.handleOwnedResource(c, false)
}

func (h *Handler) deleteResponse(c *gin.Context) {
	h.handleOwnedResource(c, true)
}

func (h *Handler) handleOwnedResource(c *gin.Context, deleteResource bool) {
	clientKey, _, ok := requestIdentity(c)
	if !ok {
		return
	}
	input := gateway.ResourceInput{ClientKey: clientKey, ResponseID: strings.TrimSpace(c.Param("responseId")), RawQuery: c.Request.URL.RawQuery}
	if input.ResponseID == "" {
		writeOpenAIError(c, http.StatusBadRequest, "invalid_request", "response_id 不能为空")
		return
	}
	var result *gateway.Result
	var err error
	if deleteResource {
		result, err = h.gateway.DeleteResponse(c.Request.Context(), input)
	} else {
		result, err = h.gateway.GetResponse(c.Request.Context(), input)
	}
	if err != nil {
		writeGatewayError(c, err)
		return
	}
	// stored-response 查询无请求模型可兜底：trailer 的 model 由流内元数据
	// 或 compat 状态补全。
	h.writeResponsesResult(c, result, false, "")
}

// writeResponsesResult 额外携带请求模型作为 trailer 的 model 兜底：流在
// 首个事件前中止时元数据为空，serde 仍要求 model 键非缺失（TUI 0.2.93）。
func (h *Handler) writeResponsesResult(c *gin.Context, result *gateway.Result, stream bool, fallbackModel string) {
	h.writeProtocolResult(c, result, stream, false, streamProtocolResponses, fallbackModel)
}

func (h *Handler) writeAnthropicResult(c *gin.Context, result *gateway.Result, stream bool) {
	h.writeProtocolResult(c, result, stream, true, streamProtocolAnthropic, "")
}

func (h *Handler) writeProtocolResult(c *gin.Context, result *gateway.Result, stream, anthropic bool, protocol streamProtocol, fallbackModel string) {
	usage := gateway.Usage{}
	responseID := ""
	errorCode := ""
	defer func() { _ = result.Body.Close() }()
	defer func() { result.Finalize(usage, responseID, errorCode) }()
	delivery := gateway.DeliveryStats{}
	initialSize := max(0, c.Writer.Size())
	defer func() {
		if err := middleware.FinishResponseEncoding(c.Writer); err != nil && errorCode == "" {
			errorCode = copyCancellationCode(c, fmt.Errorf("%w: %w", errClientStreamWrite, err))
		}
		if result.RecordDelivery != nil {
			if c.Writer.Size() >= 0 {
				delivery.Bytes = int64(max(0, c.Writer.Size()-initialSize))
				delivery.StatusCode = c.Writer.Status()
			}
			result.RecordDelivery(delivery)
		}
	}()
	if result.BeginDelivery != nil {
		if err := result.BeginDelivery(); err != nil {
			errorCode = "request_canceled"
			return
		}
	}
	if isUpstreamCredentialStatus(result.StatusCode) {
		errorCode = writeCredentialStatusUnavailable(c, anthropic, result.StatusCode, result.Body)
		return
	}
	transferLimit := int64(maxJSONResponseTransferBytes)
	if stream {
		transferLimit = maxStreamResponseTransferBytes
	}
	if contentLength, parseErr := strconv.ParseInt(result.Header.Get("Content-Length"), 10, 64); parseErr == nil && contentLength > transferLimit {
		errorCode = "response_too_large"
		writeOpenAIError(c, http.StatusBadGateway, "response_too_large", "上游响应超过代理安全上限")
		return
	}
	if result.CommitDelivery != nil {
		if err := result.CommitDelivery(); err != nil {
			errorCode = "request_canceled"
			var failure *gateway.UpstreamFailure
			if errors.As(err, &failure) {
				errorCode = failure.AuditCode()
			}
			if anthropic {
				writeGatewayAnthropicError(c, err)
			} else {
				writeGatewayError(c, err)
			}
			return
		}
	}
	copyHeaders(c.Writer.Header(), result.Header)
	if stream {
		// SSE 不得被反向代理缓冲：Web 通道上游自带该头，Build/Console 通道
		// 只设 Content-Type——nginx 默认 proxy_buffering on 会攒住首批事件，
		// 真实部署的首字延迟被代理放大。统一在传输层补齐，覆盖全部协议与
		// 上游组合；非流式 JSON 不设置，避免不必要地关闭代理缓冲。
		c.Writer.Header().Set("X-Accel-Buffering", "no")
	}
	c.Status(result.StatusCode)
	if result.StatusCode >= 400 {
		errorCode = "upstream_error"
	}
	var err error
	var commitOutput func(responseMetadata) error
	if result.CommitCompletion != nil {
		commitOutput = func(meta responseMetadata) error {
			return result.CommitCompletion(gateway.Completion{Usage: meta.Usage, ResponseID: meta.ResponseID, NativeResponseID: meta.NativeResponseID})
		}
	}
	if stream {
		metadata, copyErr := copyStreamWithCompletion(c.Writer, result.Body, protocol, result.MarkFirstToken, fallbackModel, commitOutput)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
		if metadata.StreamFailure != nil && result.RecordStreamFailure != nil {
			result.RecordStreamFailure(*metadata.StreamFailure)
		}
		delivery.Events, delivery.Bytes = metadata.DeliveredEvents, metadata.DeliveredBytes
	} else {
		metadata, copyErr := copyJSONWithCompletion(c.Writer, result.Body, protocol, commitOutput)
		usage, responseID, err = metadata.Usage, metadata.ResponseID, copyErr
		delivery.Events, delivery.Bytes = metadata.DeliveredEvents, metadata.DeliveredBytes
	}
	if err != nil {
		canceled := copyCancellationCode(c, err)
		if canceled != "" {
			errorCode = canceled
		} else if code, _, matched := streamFailureShape(err); matched {
			errorCode = code
		} else {
			switch {
			case errors.Is(err, responsebuffer.ErrExhausted):
				errorCode = "response_resource_exhausted"
			case errors.Is(err, responsebuffer.ErrLimit), errors.Is(err, errResponseTransferLimit):
				errorCode = "response_too_large"
			case errors.Is(err, errUpstreamStreamRead):
				errorCode = "upstream_stream_interrupted"
			default:
				errorCode = "stream_interrupted"
			}
		}
		if canceled == "" && errorCode == "history_commit_failed" && !stream && !c.Writer.Written() {
			c.Writer.Header().Del("Content-Length")
			if anthropic {
				writeAnthropicError(c, http.StatusBadGateway, "api_error", "会话历史提交失败", "history_commit_failed")
			} else {
				writeOpenAIError(c, http.StatusBadGateway, "history_commit_failed", "会话历史提交失败")
			}
		}
		if !stream && result.CommitCompletion != nil && !c.Writer.Written() && canceled == "" {
			c.Writer.Header().Del("Content-Length")
			if anthropic {
				writeAnthropicError(c, http.StatusBadGateway, "api_error", "响应未能完整提交", errorCode)
			} else {
				writeOpenAIError(c, http.StatusBadGateway, errorCode, "响应未能完整提交")
			}
		}
	}
}

type responseMetadata struct {
	Usage                    gateway.Usage
	cacheCreationInputTokens int64
	NativeResponseID         string
	ResponseID               string
	Model                    string
	SequenceNumber           int64
	StreamFailure            *gateway.StreamFailureDiagnostic
	// DeliveredEvents/DeliveredBytes 由 copyStream 填充：转发到客户端的
	// SSE data 事件数与累计写出字节（非流式为响应体字节数）。回答
	// 「200 且带错误码时实际交付了多少」（轮26）。
	DeliveredEvents int64
	DeliveredBytes  int64
}

func copyHeaders(destination, source http.Header) {
	excluded := map[string]struct{}{
		"connection": {}, "content-length": {}, "keep-alive": {}, "proxy-authenticate": {},
		"proxy-authorization": {}, "set-cookie": {}, "te": {}, "trailer": {},
		"transfer-encoding": {}, "upgrade": {}, "x-models-etag": {},
	}
	for _, value := range source.Values("Connection") {
		for name := range strings.SplitSeq(value, ",") {
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "" {
				excluded[name] = struct{}{}
			}
		}
	}
	for name, values := range source {
		lower := strings.ToLower(name)
		if _, skip := excluded[lower]; skip {
			continue
		}
		for _, value := range values {
			destination.Add(name, value)
		}
	}
}

func writeOpenAIError(c *gin.Context, status int, code, message string) {
	httphelpers.WriteOpenAIError(c, status, code, message)
}

func writeImageGenerationUserError(c *gin.Context, code, param, message string) {
	httphelpers.WriteOpenAIErrorTyped(c, http.StatusBadRequest, "image_generation_user_error", code, param, message)
}

func writeGatewayError(c *gin.Context, err error) {
	view := classifyClientError(err)
	encodeClientError(c, view, false)
}

func encodeClientError(c *gin.Context, view clientError, anthropic bool) {
	provider.ApplyHistoryRecoveryWarnings(c.Writer.Header(), view.HistoryRecovery)
	if view.RetryAfter > 0 {
		c.Header("Retry-After", strconv.FormatInt(retryafter.SecondsCeil(view.RetryAfter), 10))
	}
	if anthropic {
		if view.OmitAnthropicCode {
			writeAnthropicError(c, view.Status, view.AnthropicType, view.Message)
		} else {
			writeAnthropicError(c, view.Status, view.AnthropicType, view.Message, view.Code)
		}
		return
	}
	if view.Param != "" {
		httphelpers.WriteOpenAIErrorTyped(c, view.Status, "invalid_request_error", view.Code, view.Param, view.Message)
		return
	}
	writeOpenAIError(c, view.Status, view.Code, view.Message)
}

func writeGatewayAnthropicError(c *gin.Context, err error) {
	encodeClientError(c, classifyClientError(err), true)
}

func writeAnthropicError(c *gin.Context, status int, errorType, message string, errorCode ...string) {
	httphelpers.WriteAnthropicError(c, status, errorType, message, errorCode...)
}

func readCredentialErrorCode(status int, source io.Reader) string {
	body, err := io.ReadAll(io.LimitReader(source, maxCredentialErrorInspectBytes+1))
	if err != nil || len(body) > maxCredentialErrorInspectBytes {
		return "upstream_unavailable"
	}
	return gateway.ClientCredentialErrorCodeFromBody(status, body)
}

func forceJSONBoolean(body []byte, key string, value bool) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	payload[key] = json.RawMessage("false")
	if value {
		payload[key] = json.RawMessage("true")
	}
	return json.Marshal(payload)
}
