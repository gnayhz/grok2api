package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	"github.com/bogdanfinn/websocket"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

const (
	maxGeneratedImages            = 10
	mediaOutputAttempts           = 3
	imageDownloadTimeout          = 60 * time.Second
	imagineSelfUploadSource       = "IMAGINE_SELF_UPLOAD_FILE_SOURCE"
	directFileUploadResponseLimit = 2 << 20
)

var errLiteImageReady = errors.New("Lite 图片已完成")

type imagineModelConfig struct {
	Pro            bool
	ExpectedCount  int
	MaxReturnCount int
}

type imagineImageValue struct {
	ID       string
	URL      string
	Blob     string
	Position int
	Width    int
	Height   int
	position bool
}

type imagineSlot struct {
	image          imagineImageValue
	preview        imagineImageValue
	final          bool
	previewReady   bool
	previewEmitted bool
	completed      bool
	moderated      bool
	emitted        bool
}

type imagineCollector struct {
	slots         map[string]*imagineSlot
	terminalCount int
}

func (a *Adapter) GenerateImage(ctx context.Context, request provider.ImageGenerationRequest) (*provider.Response, error) {
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if request.Streaming && count != 1 {
		return imageGenerationUserError("Streaming is only supported with n=1.", "input", "unsupported_parameter")
	}
	if request.PartialImages < 0 || request.PartialImages > 3 {
		return invalidImageRequest("partial_images 必须在 0 到 3 之间")
	}
	if request.PartialImages > 0 && !request.Streaming {
		return invalidImageRequest("partial_images 仅可在 stream=true 时使用")
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("response_format 必须是 url 或 b64_json")
	}
	spec, modelKnown := Resolve(request.Model)
	if !modelKnown || spec.Capability != "image" {
		return invalidImageRequest("模型不支持图片生成")
	}
	protocolModel := spec.ProtocolModel
	if protocolModel == "" {
		protocolModel = spec.UpstreamModel
	}
	if protocolModel == "imagine-lite" {
		if request.Streaming {
			return invalidImageRequest("grok-imagine-image-lite 不支持 stream")
		}
		if count > maxGeneratedImages {
			return invalidImageRequest("n 不能超过 10")
		}
		return a.generateLiteImage(ctx, request, count, format)
	}
	ratio, err := resolveImageAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidImageRequest(err.Error())
	}
	modelConfig, ok := resolveImagineModel(protocolModel, spec.ImaginePro, count)
	if !ok {
		return invalidImageRequest("模型不支持图片生成")
	}
	if count > modelConfig.MaxReturnCount {
		return invalidImageRequest(fmt.Sprintf("n 不能超过 %d", modelConfig.MaxReturnCount))
	}
	return a.generateWSImage(ctx, request, count, format, ratio, modelConfig)
}

func (a *Adapter) generateLiteImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format string) (*provider.Response, error) {
	spec, _ := Resolve(request.Model)
	urls := make([]string, 0, count)
	for len(urls) < count {
		value, err := a.generateLiteImageURL(ctx, request.Credential, spec, request.Prompt, func() {
			provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{Started: true, UpstreamStatus: http.StatusOK})
		})
		if err != nil {
			var mediaErr *webMediaUpstreamError
			if errors.As(err, &mediaErr) && len(urls) == 0 {
				return mediaErr.providerResponse(), nil
			}
			var upstreamErr *liteUpstreamError
			if errors.As(err, &upstreamErr) && len(urls) == 0 {
				return upstreamErr.Response(), nil
			}
			if len(urls) > 0 {
				return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
					"message": fmt.Sprintf("Lite 图片仅完成 %d/%d 张", len(urls), count),
					"type":    "server_error", "code": "image_generation_incomplete",
				}}), nil
			}
			return nil, err
		}
		urls = append(urls, value)
		provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{OutputImages: len(urls), QuotaUnits: len(urls), Completed: len(urls) >= count})
	}
	response, err := a.imageResponse(ctx, request.Credential, urls, nil, count, format)
	if response != nil {
		response.QuotaUnits = count
	}
	return response, err
}

type liteUpstreamError struct {
	StatusCode int
	Status     string
	Body       []byte
}

func (e *liteUpstreamError) Error() string {
	return fmt.Sprintf("Lite 图片上游返回 %d", e.StatusCode)
}

func (e *liteUpstreamError) Response() *provider.Response {
	return &provider.Response{StatusCode: e.StatusCode, Status: e.Status, Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(e.Body))}
}

func (a *Adapter) generateLiteImageURL(ctx context.Context, credential account.Credential, spec ModelSpec, prompt string, accepted func()) (string, error) {
	for attempt := 0; attempt < 2; attempt++ {
		upstream, lease, _, statsigTarget, err := a.openChat(ctx, credential, "", spec, normalizedChatInput{Prompt: "Drawing: " + prompt}, gatewayOpenOptions{deferForbidden: true})
		if err != nil {
			return "", err
		}
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(upstream.Body, webMediaDiagnosticBodyLimit+1))
			_ = upstream.Body.Close()
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			if upstream.StatusCode == http.StatusForbidden {
				upstreamErr := newWebMediaUpstreamError(upstream.StatusCode, body, truncated)
				a.logWebMediaUpstreamRejection("image_lite_handshake", upstream, upstreamErr)
				if isClearanceRefreshableMediaError(upstreamErr) {
					// The failed WebSocket handshake invalidates the current browser
					// session. Statsig is independent and must not gate reacquiring
					// a fresh lease for the retry.
					lease.InvalidateClearance()
					_ = a.invalidateSignedStatsig(http.MethodPost, statsigTarget)
					if attempt == 0 {
						lease.Release()
						continue
					}
				}
				lease.Release()
				return "", upstreamErr
			}
			lease.Observe(upstream.StatusCode, nil)
			lease.Release()
			return "", &liteUpstreamError{StatusCode: upstream.StatusCode, Status: upstream.Status, Body: body}
		}
		if accepted != nil {
			accepted()
		}
		firstImage := ""
		capture := &boundedCapture{limit: 8 << 20}
		parsed, consumeErr := consumeUpstream(io.TeeReader(upstream.Body, capture), func(kind, delta string) error {
			if kind != "image" || strings.TrimSpace(delta) == "" {
				return nil
			}
			firstImage = delta
			return errLiteImageReady
		})
		_ = upstream.Body.Close()
		if consumeErr != nil && !errors.Is(consumeErr, errLiteImageReady) {
			if errors.Is(consumeErr, errWebUsageLimit) {
				lease.Release()
				response := jsonProviderResponse(http.StatusTooManyRequests, map[string]any{"error": map[string]any{
					"message": "Grok Imagine 速率限制中，请稍后重试",
					"type":    "rate_limit_error",
					"code":    "usage_limit_reached",
				}})
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				return "", &liteUpstreamError{StatusCode: http.StatusTooManyRequests, Status: "429 Too Many Requests", Body: body}
			}
			status := 0
			if errors.Is(consumeErr, errWebAntiBot) {
				status = http.StatusForbidden
				// A challenge can arrive inside an otherwise successful stream,
				// so the handshake path cannot invalidate it for us.
				lease.InvalidateClearance()
				if attempt == 0 {
					_ = a.invalidateSignedStatsig(http.MethodPost, statsigTarget)
					lease.Release()
					continue
				}
			}
			lease.Observe(status, consumeErr)
			lease.Release()
			if status == http.StatusForbidden {
				response := antiBotProviderResponse()
				body, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				return "", &liteUpstreamError{StatusCode: status, Status: "403 Forbidden", Body: body}
			}
			return "", consumeErr
		}
		lease.Observe(http.StatusOK, nil)
		lease.Release()
		if firstImage != "" {
			return firstImage, nil
		}
		if len(parsed.Images) == 0 {
			parsed.Images = extractMarkdownImages(parsed.Text.String())
		}
		if len(parsed.Images) == 0 {
			parsed.Images = extractCapturedImageURLs(capture.Bytes())
		}
		if len(parsed.Images) == 0 {
			diagnostics := inspectLiteCapture(capture.Bytes())
			a.log().Warn("web_lite_image_not_found",
				"account_id", credential.ID,
				"captured_bytes", len(capture.Bytes()),
				"frames", diagnostics.Frames,
				"response_fields", diagnostics.ResponseFields,
				"message_tags", diagnostics.MessageTags,
				"image_chunks", diagnostics.ImageChunks,
				"image_urls", diagnostics.ImageURLs,
				"image_fields", diagnostics.ImageFields,
				"max_progress", diagnostics.MaxProgress,
				"soft_stop", diagnostics.SoftStop,
				"upstream_error_code", diagnostics.ErrorCode,
				"upstream_error", diagnostics.ErrorMessage,
			)
			return "", fmt.Errorf("Grok Web Lite 响应结束但未解析到最终图片")
		}
		// Lite 上游固定生成两张，但每次查询只计一次 Fast 额度；按旧协议取首张并为 n 重复查询。
		return parsed.Images[0], nil
	}
	return "", fmt.Errorf("Grok Web Lite 图片签名刷新失败")
}

func (a *Adapter) forwardImageChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input openAIRequest, normalized normalizedChatInput, spec ModelSpec) (*provider.Response, error) {
	if request.Operation == conversation.OperationResponses && input.Store != nil && *input.Store {
		return invalidImageRequest("图片生成模型不支持存储 Responses，请使用 store:false")
	}
	if input.PreviousResponseID != "" {
		return invalidImageRequest("图片生成模型不支持 previous_response_id")
	}
	if len(normalized.Attachments) > 0 {
		return invalidImageRequest("文生图模型只接受当前用户消息中的纯文本；图生图请使用 grok-imagine-image-edit 和 /v1/images/edits")
	}
	count := 1
	format := "url"
	if input.ImageConfig != nil {
		if input.ImageConfig.Count != nil {
			count = *input.ImageConfig.Count
		}
		if strings.TrimSpace(input.ImageConfig.ResponseFormat) != "" {
			format = strings.ToLower(strings.TrimSpace(input.ImageConfig.ResponseFormat))
		}
	}
	if count < 1 || count > maxGeneratedImages {
		return invalidImageRequest("image_config.n 必须在 1 到 10 之间")
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("image_config.response_format 必须是 url 或 b64_json")
	}
	metadata := provider.NormalizedRequestMetadata{ImageOutputCount: count}
	if request.NormalizedMetadata != nil {
		*request.NormalizedMetadata = metadata
	}
	if request.OnNormalized != nil {
		if err := request.OnNormalized(metadata); err != nil {
			return nil, err
		}
	}
	if spec.ProtocolModel != "imagine-lite" {
		return a.forwardQualityImageChatCompletion(ctx, request, input, normalized, count, format)
	}
	responseID := newWebID("resp")
	parsed := parsedChat{ResponseID: responseID, InputTokens: estimateTokens(normalized.Prompt)}
	for i := range count {
		rawURL, err := a.generateLiteImageURL(ctx, request.Credential, spec, normalized.Prompt, func() {
			provider.ObserveImageGeneration(request.ObserveImage, provider.ImageGenerationObservation{Started: true, UpstreamStatus: http.StatusOK})
		})
		if err != nil {
			var mediaErr *webMediaUpstreamError
			if errors.As(err, &mediaErr) && parsed.Text.Len() == 0 {
				return mediaErr.providerResponse(), nil
			}
			var upstreamErr *liteUpstreamError
			if errors.As(err, &upstreamErr) && parsed.Text.Len() == 0 {
				return upstreamErr.Response(), nil
			}
			return nil, err
		}
		provider.ObserveImageGeneration(request.ObserveImage, provider.ImageGenerationObservation{OutputImages: i + 1, QuotaUnits: i + 1, Completed: i+1 >= count})
		item, err := a.imageDataItem(ctx, request.Credential, imagineImageValue{URL: rawURL}, format)
		if err != nil {
			return nil, err
		}
		if parsed.Text.Len() > 0 {
			parsed.appendText("\n\n")
		}
		parsed.appendText(liteImageMarkdown(item))
	}
	if input.Stream || request.Streaming {
		stream, err := buildImageCompatibilityStream(request.Operation, responseID, input.Model, &parsed)
		if err != nil {
			return nil, err
		}
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: io.NopCloser(bytes.NewReader(stream)), QuotaUnits: count}, nil
	}
	payload := buildImageCompatibilityResult(request.Operation, responseID, input.Model, parsed)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: count}, nil
}

func (a *Adapter) forwardQualityImageChatCompletion(ctx context.Context, request provider.ResponseResourceRequest, input openAIRequest, normalized normalizedChatInput, count int, format string) (*provider.Response, error) {
	aspectRatio := ""
	resolution := ""
	if input.ImageConfig != nil {
		aspectRatio = input.ImageConfig.AspectRatio
		resolution = input.ImageConfig.Resolution
	}
	generated, err := a.GenerateImage(ctx, provider.ImageGenerationRequest{
		Observe: request.ObserveImage, Credential: request.Credential, Model: request.Model, Prompt: normalized.Prompt,
		Count: count, AspectRatio: aspectRatio, Resolution: resolution, ResponseFormat: format,
	})
	if err != nil {
		return nil, err
	}
	if generated.StatusCode < http.StatusOK || generated.StatusCode >= http.StatusMultipleChoices {
		return generated, nil
	}
	defer func() { _ = generated.Body.Close() }()
	var payload struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.NewDecoder(generated.Body).Decode(&payload); err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片生成兼容响应解析失败: %w", err))
	}
	parsed := parsedChat{ResponseID: newWebID("resp"), InputTokens: estimateTokens(normalized.Prompt)}
	for _, item := range payload.Data {
		markdown := liteImageMarkdown(item)
		if markdown == "" {
			continue
		}
		if parsed.Text.Len() > 0 {
			parsed.appendText("\n\n")
		}
		parsed.appendText(markdown)
	}
	if parsed.Text.Len() == 0 {
		return nil, fmt.Errorf("图片生成兼容响应中没有图片")
	}
	if input.Stream || request.Streaming {
		stream, err := buildImageCompatibilityStream(request.Operation, parsed.ResponseID, input.Model, &parsed)
		if err != nil {
			return nil, err
		}
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: io.NopCloser(bytes.NewReader(stream)), QuotaUnits: generated.QuotaUnits}, nil
	}
	result := buildImageCompatibilityResult(request.Operation, parsed.ResponseID, input.Model, parsed)
	data, err := json.Marshal(result)
	if err != nil {
		return nil, err
	}
	return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data)), QuotaUnits: generated.QuotaUnits}, nil
}

// Image response IDs describe this output only; they have no native Web state.
func buildImageCompatibilityResult(operation, responseID, model string, parsed parsedChat) map[string]any {
	result := buildOpenAIResult(operation, responseID, model, parsed, false)
	if operation == conversation.OperationResponses {
		result["store"] = false
	}
	return result
}

func buildImageCompatibilityStream(operation, responseID, model string, parsed *parsedChat) ([]byte, error) {
	var stream bytes.Buffer
	writeStreamStart(&stream, operation, responseID, model, parsed.InputTokens, false)
	if operation == conversation.OperationResponses {
		responsesStream := newWebResponsesStream(&stream, responseID)
		if err := responsesStream.Delta("text", parsed.Text.String()); err != nil {
			return nil, err
		}
		if err := responsesStream.Finish(parsed); err != nil {
			return nil, err
		}
	} else if err := writeStreamDelta(&stream, operation, responseID, model, "text", parsed.Text.String()); err != nil {
		return nil, err
	}
	payload := buildImageCompatibilityResult(operation, responseID, model, *parsed)
	writeStreamDone(&stream, operation, responseID, model, *parsed, payload)
	return stream.Bytes(), nil
}

func liteImageMarkdown(item map[string]any) string {
	if value, _ := item["url"].(string); value != "" {
		return "![image](" + value + ")"
	}
	if value, _ := item["b64_json"].(string); value != "" {
		mimeType, _ := item["mime_type"].(string)
		if mimeType == "" {
			mimeType = "image/jpeg"
		}
		return "![image](data:" + mimeType + ";base64," + value + ")"
	}
	return ""
}

func (a *Adapter) generateWSImage(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio string, modelConfig imagineModelConfig) (*provider.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := a.generateWSImageAttempt(ctx, request, count, format, ratio, modelConfig)
		if err == nil {
			return response, nil
		}
		var upstreamErr *webMediaUpstreamError
		if !errors.As(err, &upstreamErr) || !isClearanceRefreshableMediaError(upstreamErr) || attempt > 0 {
			if errors.As(err, &upstreamErr) {
				return upstreamErr.providerResponse(), nil
			}
			return nil, err
		}
		a.log().Warn("web_image_clearance_retry", "operation", "imagine", "status", upstreamErr.status, "body_kind", upstreamErr.bodyKind)
	}
	return nil, fmt.Errorf("Imagine WebSocket Clearance 重试耗尽")
}

func (a *Adapter) generateWSImageAttempt(ctx context.Context, request provider.ImageGenerationRequest, count int, format, ratio string, modelConfig imagineModelConfig) (*provider.Response, error) {
	cfg := a.config()
	token, err := a.cipher.Decrypt(request.Credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWeb, request.Credential)
	if err != nil {
		return nil, err
	}
	leaseOwned := true
	defer func() {
		if leaseOwned {
			lease.Release()
		}
	}()
	wsURL, err := imagineURL(cfg.BaseURL)
	if err != nil {
		return nil, err
	}
	headers := fhttp.Header{}
	headers.Set("Origin", cfg.BaseURL)
	headers.Set("User-Agent", lease.UserAgent)
	headers.Set("Cookie", egress.BuildSSOCookie(token, lease.CFCookies))
	headers.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	headers.Set("Cache-Control", "no-cache")
	headers.Set("Pragma", "no-cache")
	connection, response, err := lease.DialWebSocketDeferredForbidden(ctx, wsURL, headers, 30*time.Second)
	if err != nil {
		if response != nil {
			var body []byte
			if response.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
				_ = response.Body.Close()
			}
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
			if isClearanceRefreshableMediaError(upstreamErr) {
				lease.InvalidateClearance()
			}
			a.logWebMediaUpstreamRejection("image_imagine_handshake", &http.Response{
				StatusCode: response.StatusCode,
				Header:     http.Header(response.Header).Clone(),
			}, upstreamErr)
			if response.StatusCode != http.StatusForbidden {
				lease.Observe(response.StatusCode, err)
			}
			return nil, upstreamErr
		}
		lease.Observe(0, err)
		return nil, fmt.Errorf("连接 Imagine WebSocket: %w", err)
	}
	connectionOwned := true
	defer func() {
		if connectionOwned {
			_ = connection.Close()
		}
	}()
	connection.SetReadLimit(64 << 20)
	deadline := time.Now().Add(cfg.ImageTimeout)
	_ = connection.SetReadDeadline(deadline)
	_ = connection.SetWriteDeadline(deadline)
	if err := connection.WriteJSON(imagineResetMessage()); err != nil {
		lease.ObserveWebSocketError(err)
		return nil, err
	}
	if err := connection.WriteJSON(imagineRequestMessage(newWebID("img"), request.Prompt, ratio, cfg.AllowNSFW, modelConfig.Pro, modelConfig.ExpectedCount)); err != nil {
		lease.ObserveWebSocketError(err)
		return nil, err
	}
	provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{Started: true, UpstreamStatus: http.StatusSwitchingProtocols})
	if request.Streaming {
		leaseOwned, connectionOwned = false, false
		body := produceImageStream(ctx, connection, func(streamCtx context.Context, writer io.Writer) error {
			return a.streamImagineImages(streamCtx, writer, connection, lease, request.Credential, count, request.PartialImages, modelConfig, request.Observe)
		})
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: body, QuotaUnits: count}, nil
	}

	collector := newImagineCollector()
	for collector.UsableCount() < count && !collector.Done(modelConfig.ExpectedCount) {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			lease.ObserveWebSocketError(readErr)
			return nil, fmt.Errorf("读取 Imagine WebSocket: %w", readErr)
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{Failed: true})
			upstreamErr := fmt.Errorf("Imagine WebSocket 返回错误")
			lease.Observe(0, upstreamErr)
			return nil, upstreamErr
		}
		collector.Accept(message)
		provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{OutputImages: collector.UsableCount(), QuotaUnits: collector.UsableCount(), Completed: collector.UsableCount() >= count})
	}
	lease.Observe(http.StatusOK, nil)
	images := collector.Images()
	if len(images) == 0 {
		return nil, fmt.Errorf("Imagine WebSocket 完成但没有可用图片")
	}
	if len(images) < count {
		return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": fmt.Sprintf("上游仅返回 %d/%d 张可用图片", len(images), count),
			"type":    "server_error", "code": "image_generation_incomplete",
		}}), nil
	}
	urls := make([]string, 0, len(images))
	blobs := make([]string, 0, len(images))
	for _, image := range images {
		urls = append(urls, image.URL)
		blobs = append(blobs, image.Blob)
	}
	result, err := a.imageResponse(ctx, request.Credential, urls, blobs, count, format)
	if result != nil {
		result.QuotaUnits = count
	}
	return result, err
}

// EditImage retries the complete browser media flow once after a challenge
// response. Reacquiring the lease is required because the failed lease keeps
// the immutable browser-session cookies that were rejected upstream.
