package httpserver

// SwaggerMessage 表示 Chat Completions 请求中的一条消息。
type SwaggerMessage struct {
	Role    string `json:"role" example:"user"`
	Content any    `json:"content"`
}

// SwaggerResponsesRequest 表示最小 Responses 请求。
type SwaggerResponsesRequest struct {
	Model              string `json:"model" example:"grok-chat-auto"`
	Input              any    `json:"input"`
	Stream             bool   `json:"stream" example:"false"`
	Store              bool   `json:"store" example:"false"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	PromptCacheKey     string `json:"prompt_cache_key,omitempty"`
}

// SwaggerChatRequest 表示最小 Chat Completions 请求。
type SwaggerChatRequest struct {
	Model    string           `json:"model" example:"grok-chat-fast"`
	Messages []SwaggerMessage `json:"messages"`
	Stream   bool             `json:"stream" example:"false"`
}

// SwaggerMessagesRequest 表示最小 Anthropic Messages 请求。
type SwaggerMessagesRequest struct {
	Model     string           `json:"model" example:"grok-chat-expert"`
	MaxTokens int              `json:"max_tokens" example:"1024"`
	Messages  []SwaggerMessage `json:"messages"`
	Stream    bool             `json:"stream" example:"false"`
}

// SwaggerImageGenerationRequest 表示图片生成请求。
type SwaggerImageGenerationRequest struct {
	Model          string `json:"model" example:"grok-imagine-image-quality"`
	Prompt         string `json:"prompt" example:"A cinematic city at night"`
	N              int    `json:"n" example:"1"`
	AspectRatio    string `json:"aspect_ratio,omitempty" example:"16:9"`
	Resolution     string `json:"resolution,omitempty" example:"2k"`
	ResponseFormat string `json:"response_format,omitempty" example:"url"`
	Stream         bool   `json:"stream,omitempty" example:"false"`
	PartialImages  int    `json:"partial_images,omitempty" example:"0"`
}

// SwaggerImageReference 表示图片 URL 输入。
type SwaggerImageReference struct {
	URL string `json:"url" example:"https://example.com/source.png"`
}

// SwaggerImageEditRequest 表示图片编辑请求。
type SwaggerImageEditRequest struct {
	Model          string                `json:"model" example:"grok-imagine-image-edit"`
	Prompt         string                `json:"prompt" example:"Change the background to black"`
	Image          SwaggerImageReference `json:"image"`
	N              int                   `json:"n" example:"1"`
	Size           string                `json:"size,omitempty" example:"1024x1024"`
	AspectRatio    string                `json:"aspect_ratio,omitempty" example:"1:1"`
	Resolution     string                `json:"resolution,omitempty" example:"1k"`
	ResponseFormat string                `json:"response_format,omitempty" example:"url"`
	Stream         bool                  `json:"stream,omitempty" example:"false"`
	PartialImages  int                   `json:"partial_images,omitempty" example:"0"`
}

// SwaggerVideoGenerationRequest 表示视频生成请求。
// image 与 reference_images/reference_audios 互斥；参考图模式 resolution 最高 720p。
type SwaggerVideoGenerationRequest struct {
	Model           string                   `json:"model" example:"grok-imagine-video"`
	Prompt          string                   `json:"prompt" example:"A cinematic tracking shot in the rain"`
	Duration        int                      `json:"duration" example:"8"`
	AspectRatio     string                   `json:"aspect_ratio,omitempty" example:"16:9"`
	Resolution      string                   `json:"resolution,omitempty" example:"720p"`
	Image           *SwaggerVideoMediaInput  `json:"image,omitempty"`
	ReferenceImages []SwaggerVideoMediaInput `json:"reference_images,omitempty"`
	ReferenceAudios []SwaggerVideoAudioInput `json:"reference_audios,omitempty"`
	Video           *SwaggerVideoMediaInput  `json:"video,omitempty"`
}

// SwaggerVideoMediaInput 表示视频相关的图片/视频输入。
type SwaggerVideoMediaInput struct {
	URL    string `json:"url,omitempty" example:"https://example.com/input.png"`
	FileID string `json:"file_id,omitempty" example:"file_123"`
}

// SwaggerVideoAudioInput 表示参考音频（内置 voice_id）。
type SwaggerVideoAudioInput struct {
	VoiceID string `json:"voice_id" example:"eve"`
}

// swaggerHealth godoc
// @Summary 存活检查
// @Tags System
// @Produce json
// @Success 200 {object} map[string]bool
// @Router /healthz [get]
func swaggerHealth() {}

// swaggerReady godoc
// @Summary 就绪检查
// @Tags System
// @Produce json
// @Success 200 {object} map[string]bool
// @Failure 503 {object} map[string]bool
// @Router /readyz [get]
func swaggerReady() {}

// swaggerModels godoc
// @Summary 获取可用模型
// @Tags Models
// @Security BearerAuth
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Router /v1/models [get]
func swaggerModels() {}

// swaggerResponses godoc
// @Summary 创建 Response
// @Description 支持 JSON 与 SSE；stream=true 时返回 text/event-stream。
// @Tags Responses
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerResponsesRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Failure 401 {object} map[string]any
// @Router /v1/responses [post]
func swaggerResponses() {}

// swaggerCompactResponse godoc
// @Summary 压缩 Response 上下文
// @Tags Responses
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerResponsesRequest true "请求"
// @Success 200 {object} map[string]any
// @Router /v1/responses/compact [post]
func swaggerCompactResponse() {}

// swaggerGetResponse godoc
// @Summary 查询 Response
// @Tags Responses
// @Security BearerAuth
// @Produce json
// @Param response_id path string true "Response ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/responses/{response_id} [get]
func swaggerGetResponse() {}

// swaggerDeleteResponse godoc
// @Summary 删除 Response
// @Tags Responses
// @Security BearerAuth
// @Produce json
// @Param response_id path string true "Response ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/responses/{response_id} [delete]
func swaggerDeleteResponse() {}

// swaggerChat godoc
// @Summary 创建 Chat Completion
// @Description 支持 JSON 与 SSE、图片输入和函数工具。
// @Tags Chat
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerChatRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/chat/completions [post]
func swaggerChat() {}

// swaggerMessages godoc
// @Summary 创建 Anthropic Message
// @Tags Messages
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param anthropic-version header string true "Anthropic API version" default(2023-06-01)
// @Param request body SwaggerMessagesRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/messages [post]
func swaggerMessages() {}

// swaggerGenerateImage godoc
// @Summary 生成图片
// @Tags Images
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerImageGenerationRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/images/generations [post]
func swaggerGenerateImage() {}

// swaggerEditImage godoc
// @Summary 编辑图片
// @Tags Images
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerImageEditRequest true "请求"
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/images/edits [post]
func swaggerEditImage() {}

// swaggerGetImage godoc
// @Summary 获取归档图片
// @Tags Images
// @Produce image/png
// @Param asset_id path string true "Asset ID"
// @Success 200 {file} binary
// @Failure 404
// @Router /v1/media/images/{asset_id} [get]
func swaggerGetImage() {}

// swaggerGenerateVideo godoc
// @Summary 创建异步视频任务
// @Tags Videos
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerVideoGenerationRequest true "请求"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]any
// @Router /v1/videos/generations [post]
func swaggerGenerateVideo() {}

// swaggerEditVideo godoc
// @Summary 创建异步视频编辑任务
// @Tags Videos
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerVideoGenerationRequest true "请求"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]any
// @Router /v1/videos/edits [post]
func swaggerEditVideo() {}

// swaggerExtendVideo godoc
// @Summary 创建异步视频延长任务
// @Tags Videos
// @Security BearerAuth
// @Accept json
// @Produce json
// @Param request body SwaggerVideoGenerationRequest true "请求"
// @Success 200 {object} map[string]string
// @Failure 400 {object} map[string]any
// @Router /v1/videos/extensions [post]
func swaggerExtendVideo() {}

// swaggerGetVideo godoc
// @Summary 查询异步视频任务
// @Tags Videos
// @Security BearerAuth
// @Produce json
// @Param request_id path string true "Request ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/videos/{request_id} [get]
func swaggerGetVideo() {}

// swaggerTTS godoc
// @Summary 文本转语音（OpenAI 兼容）
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/tts [post]
func swaggerTTS() {}

// swaggerTTSVoices godoc
// @Summary 列出可用语音
// @Tags Audio
// @Security BearerAuth
// @Produce json
// @Success 200 {object} map[string]any
// @Router /v1/tts/voices [get]
func swaggerTTSVoices() {}

// swaggerTTSVoiceDetail godoc
// @Summary 查询单个语音
// @Tags Audio
// @Security BearerAuth
// @Produce json
// @Param voice_id path string true "Voice ID"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Router /v1/tts/voices/{voice_id} [get]
func swaggerTTSVoiceDetail() {}

// swaggerAudioSpeech godoc
// @Summary 文本转语音（/v1/audio/speech 别名）
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/audio/speech [post]
func swaggerAudioSpeech() {}

// swaggerSTT godoc
// @Summary 语音转文本
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/stt [post]
func swaggerSTT() {}

// swaggerSTTWebSocket godoc
// @Summary 语音转文本（流式 WebSocket）
// @Tags Audio
// @Security BearerAuth
// @Success 101 {string} string "Switching Protocols"
// @Router /v1/stt [get]
func swaggerSTTWebSocket() {}

// swaggerAudioTranscriptions godoc
// @Summary 语音转文本（/v1/audio/transcriptions 别名）
// @Tags Audio
// @Security BearerAuth
// @Accept json
// @Produce json
// @Success 200 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/audio/transcriptions [post]
func swaggerAudioTranscriptions() {}

// swaggerRealtime godoc
// @Summary 实时语音会话（WebSocket）
// @Tags Realtime
// @Security BearerAuth
// @Param model query string false "模型"
// @Success 101 {string} string "Switching Protocols"
// @Failure 400 {object} map[string]any
// @Router /v1/realtime [get]
func swaggerRealtime() {}

// swaggerMediaUpload godoc
// @Summary 媒体分片上传（凭票据 PUT）
// @Tags Media
// @Accept octet-stream
// @Produce json
// @Param token path string true "上传票据 token"
// @Success 200 {object} map[string]any
// @Failure 404 {object} map[string]any
// @Failure 410 {object} map[string]any
// @Failure 429 {object} map[string]any
// @Router /v1/media/uploads/{token} [put]
func swaggerMediaUpload() {}

// swaggerMediaImport godoc
// @Summary 导入外部媒体为临时输入
// @Tags Media
// @Security BearerAuth
// @Accept json
// @Produce json
// @Success 201 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Router /v1/media/inputs/import [post]
func swaggerMediaImport() {}

// swaggerMediaInputUpload godoc
// @Summary 上传临时媒体输入（multipart）
// @Tags Media
// @Security BearerAuth
// @Accept multipart/form-data
// @Produce json
// @Success 201 {object} map[string]any
// @Failure 400 {object} map[string]any
// @Failure 413 {object} map[string]any
// @Failure 503 {object} map[string]any
// @Router /v1/media/inputs/upload [post]
func swaggerMediaInputUpload() {}
