package web

// Web 图片编辑(media 组件):编辑请求载荷构建、宽高比解析、
// 编辑流帧解析与公开 OpenAI 图片编辑事件编码。生成在 image.go。

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/bogdanfinn/websocket"
	account "github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	mediadomain "github.com/chenyme/grok2api/backend/internal/domain/media"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	provider "github.com/chenyme/grok2api/backend/internal/port/provider"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"
)

func (a *Adapter) EditImage(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		response, err := a.editImageAttempt(ctx, request)
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
		a.log().Warn("web_image_clearance_retry", "operation", "edit", "status", upstreamErr.status, "body_kind", upstreamErr.bodyKind)
	}
	return nil, fmt.Errorf("图片编辑 Clearance 重试耗尽")
}

func (a *Adapter) editImageAttempt(ctx context.Context, request provider.ImageEditRequest) (*provider.Response, error) {
	if strings.TrimSpace(request.Quality) != "" {
		return invalidImageRequest("Grok Web 图片模型不支持 quality")
	}
	if len(request.ImageURLs) == 0 || len(request.ImageURLs) > 8 {
		return invalidImageRequest("image 数量必须在 1 到 8 之间")
	}
	count := request.Count
	if count <= 0 {
		count = 1
	}
	if count != 1 {
		return invalidImageRequest("Grok Web 图片编辑当前仅支持 n=1")
	}
	if request.PartialImages < 0 || request.PartialImages > 3 {
		return invalidImageRequest("partial_images 必须在 0 到 3 之间")
	}
	if request.PartialImages > 0 && !request.Streaming {
		return invalidImageRequest("partial_images 仅可在 stream=true 时使用")
	}
	resolution := strings.ToLower(strings.TrimSpace(request.Resolution))
	if resolution == "" {
		resolution = "1k"
	}
	if resolution != "1k" {
		return invalidImageRequest("Grok Web 图片编辑当前仅支持 resolution=1k")
	}
	format := strings.ToLower(strings.TrimSpace(request.ResponseFormat))
	if format == "" {
		format = "url"
	}
	if format != "url" && format != "b64_json" {
		return invalidImageRequest("response_format 必须是 url 或 b64_json")
	}
	ratio, err := resolveImageEditAspectRatio(request.AspectRatio, request.Size)
	if err != nil {
		return invalidImageRequest(err.Error())
	}
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
	images := make([]provider.ImageInput, 0, len(request.ImageURLs))
	for _, rawURL := range request.ImageURLs {
		image, loadErr := a.loadChatImage(ctx, lease, rawURL, cfg.MaxInputImageBytes)
		if loadErr != nil {
			if errors.Is(loadErr, errInvalidChatImage) || errors.Is(loadErr, errInvalidChatAttachment) {
				return invalidImageRequest("编辑图片输入无效或超过大小限制")
			}
			return nil, loadErr
		}
		images = append(images, image)
	}
	assets := make([]string, 0, len(images))
	for _, image := range images {
		uploaded, uploadErr := a.uploadFileV2Direct(ctx, cfg, lease, token, image, cfg.BaseURL+"/imagine", imagineSelfUploadSource, "image_edit_upload")
		if uploadErr != nil {
			return nil, uploadErr
		}
		if uploaded.MetadataID == "" {
			return nil, fmt.Errorf("上传图片成功但上游未返回 fileMetadataId")
		}
		assets = append(assets, uploaded.MetadataID)
	}
	payload := buildImageEditPayload(request.Prompt, assets, ratio)
	response, err := a.postJSONWithReferer(ctx, cfg, lease, token, cfg.BaseURL+"/rest/app-chat/conversations/new", payload, cfg.ImageTimeout, cfg.BaseURL+"/imagine")
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		_ = response.Body.Close()
		truncated := len(body) > webMediaDiagnosticBodyLimit
		if truncated {
			body = body[:webMediaDiagnosticBodyLimit]
		}
		upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
		a.logWebMediaUpstreamRejection("image_edit_generate", response, upstreamErr)
		return nil, upstreamErr
	}
	provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{Started: true, UpstreamStatus: response.StatusCode})
	if request.Streaming {
		leaseOwned = false
		body := produceImageStream(ctx, response.Body, func(streamCtx context.Context, writer io.Writer) error {
			return a.streamImageEdit(streamCtx, writer, response.Body, lease, request.Credential, request.PartialImages, request.Size, ratio, request.Observe)
		})
		return &provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: body, QuotaUnits: 1}, nil
	}
	defer func() { _ = response.Body.Close() }()
	capture := &boundedCapture{limit: 8 << 20}
	parsed, consumeErr := consumeUpstream(io.TeeReader(response.Body, capture), func(kind, delta string) error {
		if kind == "image" && strings.TrimSpace(delta) != "" {
			provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{OutputImages: 1, QuotaUnits: 1, Completed: true})
		}
		return nil
	})
	if consumeErr != nil {
		return nil, consumeErr
	}
	urls := imageEditResultURLs(&parsed, capture.Bytes())
	if len(urls) == 0 {
		return jsonProviderResponse(http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "上游未返回可用的编辑图片",
			"type":    "server_error", "code": "image_edit_incomplete",
		}}), nil
	}
	provider.ObserveImageGeneration(request.Observe, provider.ImageGenerationObservation{OutputImages: 1, QuotaUnits: 1, Completed: true})
	result, err := a.imageResponse(ctx, request.Credential, urls, nil, 1, format)
	if result != nil {
		result.QuotaUnits = 1
	}
	return result, err
}

func buildImageEditPayload(prompt string, assets []string, aspectRatio string) map[string]any {
	imageToImage := map[string]any{
		"prompt":      prompt,
		"inputAssets": assets,
	}
	if aspectRatio != "" {
		imageToImage["aspectRatio"] = aspectRatio
	}
	return map[string]any{
		"modelName": "imagine-image-edit", "message": prompt,
		"enableImageStreaming": true, "enableSideBySide": true, "sendFinalMetadata": true,
		"mediaGenInput": map[string]any{"imageToImage": imageToImage},
	}
}

func resolveImageEditAspectRatio(aspectRatio, size string) (string, error) {
	if strings.TrimSpace(aspectRatio) == "" && strings.TrimSpace(size) == "" {
		return "", nil
	}
	return resolveImageAspectRatio(aspectRatio, size)
}

type imageEditStreamFrame struct {
	URL       string
	Progress  int
	Moderated bool
}

func parseImageEditStreamFrame(data []byte) (imageEditStreamFrame, bool) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return imageEditStreamFrame{}, false
	}
	result, _ := root["result"].(map[string]any)
	response, _ := result["response"].(map[string]any)
	imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any)
	if imageResponse == nil {
		return imageEditStreamFrame{}, false
	}
	rawURL := firstString(imageResponse, "imageUrl", "url")
	if rawURL == "" {
		return imageEditStreamFrame{}, false
	}
	progress, hasProgress := numberAsInt(imageResponse["progress"])
	if !hasProgress {
		if final, _ := imageResponse["isFinal"].(bool); !final {
			return imageEditStreamFrame{}, false
		}
		progress = 100
	}
	moderated, _ := imageResponse["moderated"].(bool)
	return imageEditStreamFrame{URL: absoluteAssetURL(rawURL), Progress: progress, Moderated: moderated}, true
}

func (a *Adapter) streamImageEdit(
	ctx context.Context,
	writer io.Writer,
	source io.ReadCloser,
	lease *infraegress.Lease,
	credential account.Credential,
	partialImages int,
	size string,
	aspectRatio string,
	observe func(provider.ImageGenerationObservation),
) error {
	defer lease.Release()
	createdAt := time.Now().Unix()
	parsed := parsedChat{}
	capture := &boundedCapture{limit: 8 << 20}
	seenPartials := make(map[string]struct{}, partialImages)
	partialIndex := 0
	consumeErr := consumeJSONObjects(io.TeeReader(source, capture), 8<<20, func(data []byte) error {
		if _, _, err := parseUpstreamFrame(data, &parsed); err != nil {
			return err
		}
		frame, ok := parseImageEditStreamFrame(data)
		if ok && !frame.Moderated && frame.Progress >= 100 {
			provider.ObserveImageGeneration(observe, provider.ImageGenerationObservation{OutputImages: 1, QuotaUnits: 1, Completed: true})
		}
		if !ok || frame.Moderated || frame.Progress >= 100 || partialIndex >= partialImages {
			return nil
		}
		if _, exists := seenPartials[frame.URL]; exists {
			return nil
		}
		raw, err := a.imageBytes(ctx, credential, imagineImageValue{URL: frame.URL})
		if err != nil {
			// partial_images 是尽力而为；预览下载失败不应阻断最终编辑结果。
			return nil
		}
		if err := writeSSE(writer, "image_edit.partial_image", openAIImageEditStreamEvent(
			"image_edit.partial_image", raw, createdAt, imageEditEventSize(size, aspectRatio), partialIndex,
		)); err != nil {
			return err
		}
		seenPartials[frame.URL] = struct{}{}
		partialIndex++
		return nil
	})
	if consumeErr != nil {
		lease.Observe(0, consumeErr)
		return consumeErr
	}
	urls := imageEditResultURLs(&parsed, capture.Bytes())
	if len(urls) == 0 {
		err := fmt.Errorf("上游未返回可用的编辑图片")
		lease.Observe(0, err)
		return err
	}
	provider.ObserveImageGeneration(observe, provider.ImageGenerationObservation{OutputImages: 1, QuotaUnits: 1, Completed: true})
	raw, err := a.imageBytes(ctx, credential, imagineImageValue{URL: urls[0]})
	if err != nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
	}
	if err := a.saveStreamImage(ctx, raw); err != nil {
		return err
	}
	if err := writeSSE(writer, "image_edit.completed", openAIImageEditStreamEvent(
		"image_edit.completed", raw, createdAt, imageEditEventSize(size, aspectRatio), 0,
	)); err != nil {
		return err
	}
	lease.Observe(http.StatusOK, nil)
	return nil
}

func openAIImageEditStreamEvent(eventType string, raw []byte, createdAt int64, size string, partialIndex int) map[string]any {
	value := map[string]any{
		"type": eventType, "b64_json": base64.StdEncoding.EncodeToString(raw),
		"created_at": createdAt, "size": size, "quality": "auto",
		"background": "auto", "output_format": imageOutputFormat(raw),
	}
	if eventType == "image_edit.partial_image" {
		value["partial_image_index"] = partialIndex
	} else {
		value["usage"] = map[string]any{
			"total_tokens": 0, "input_tokens": 0, "output_tokens": 0,
			"input_tokens_details": map[string]any{"text_tokens": 0, "image_tokens": 0},
		}
	}
	return value
}

func imageEditEventSize(size, aspectRatio string) string {
	switch value := strings.ToLower(strings.TrimSpace(size)); value {
	case "1024x1024", "1024x1536", "1536x1024", "auto":
		return value
	}
	switch strings.ToLower(strings.TrimSpace(aspectRatio)) {
	case "1:1":
		return "1024x1024"
	case "2:3":
		return "1024x1536"
	case "3:2":
		return "1536x1024"
	default:
		return "auto"
	}
}

func imageEditResultURLs(parsed *parsedChat, captured []byte) []string {
	values := append([]string(nil), parsed.Images...)
	if len(values) == 0 {
		values = extractCapturedImageURLs(captured)
	}
	if len(values) == 0 {
		values = extractMarkdownImages(parsed.Text.String())
	}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = absoluteAssetURL(value)
		if _, moderated := parsed.moderatedImages[value]; moderated || containsString(result, value) {
			continue
		}
		result = append(result, value)
	}
	return result
}

type boundedCapture struct {
	data  []byte
	limit int
}

func (w *boundedCapture) Write(value []byte) (int, error) {
	remaining := w.limit - len(w.data)
	if remaining > 0 {
		w.data = append(w.data, value[:min(remaining, len(value))]...)
	}
	return len(value), nil
}

func (w *boundedCapture) Bytes() []byte { return w.data }

func extractCapturedImageURLs(data []byte) []string {
	results := make([]string, 0, 2)
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		var value any
		if json.Unmarshal(frame, &value) == nil {
			collectCapturedImageURLs(value, &results)
		}
		return nil
	})
	return results
}

type liteCaptureDiagnostics struct {
	Frames         int
	ResponseFields []string
	MessageTags    []string
	ImageChunks    int
	ImageURLs      int
	ImageFields    []string
	MaxProgress    int
	SoftStop       bool
	ErrorCode      string
	ErrorMessage   string
}

func inspectLiteCapture(data []byte) liteCaptureDiagnostics {
	result := liteCaptureDiagnostics{}
	fields := make(map[string]struct{})
	tags := make(map[string]struct{})
	imageFields := make(map[string]struct{})
	_ = consumeJSONObjects(bytes.NewReader(data), 8<<20, func(frame []byte) error {
		result.Frames++
		var root map[string]any
		if json.Unmarshal(frame, &root) != nil {
			return nil
		}
		value, _ := root["result"].(map[string]any)
		response, _ := value["response"].(map[string]any)
		for key := range response {
			fields[key] = struct{}{}
		}
		if tag, _ := response["messageTag"].(string); tag != "" {
			tags[tag] = struct{}{}
		}
		if stopped, _ := response["isSoftStop"].(bool); stopped {
			result.SoftStop = true
		}
		if responseError, ok := response["error"].(map[string]any); ok {
			result.ErrorCode = fmt.Sprint(responseError["code"])
			result.ErrorMessage = firstString(responseError, "message", "error")
			if len(result.ErrorMessage) > 200 {
				result.ErrorMessage = result.ErrorMessage[:200]
			}
		}
		inspectLiteCaptureValue(response, &result, imageFields)
		return nil
	})
	result.ResponseFields = sortedSetValues(fields)
	result.MessageTags = sortedSetValues(tags)
	result.ImageFields = sortedSetValues(imageFields)
	return result
}

func inspectLiteCaptureValue(value any, result *liteCaptureDiagnostics, imageFields map[string]struct{}) {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			if key == "jsonData" {
				if encoded, _ := nested.(string); encoded != "" {
					var decoded any
					if json.Unmarshal([]byte(encoded), &decoded) == nil {
						inspectLiteCaptureValue(decoded, result, imageFields)
					}
				}
			}
			if key == "image_chunk" || key == "imageChunk" {
				if chunk, ok := nested.(map[string]any); ok {
					result.ImageChunks++
					for field := range chunk {
						imageFields[field] = struct{}{}
					}
					if firstString(chunk, "imageUrl", "image_url", "url") != "" {
						result.ImageURLs++
					}
					if progress, ok := numberAsInt(chunk["progress"]); ok && progress > result.MaxProgress {
						result.MaxProgress = progress
					}
				}
			}
			inspectLiteCaptureValue(nested, result, imageFields)
		}
	case []any:
		for _, nested := range current {
			inspectLiteCaptureValue(nested, result, imageFields)
		}
	}
}

func sortedSetValues(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func collectCapturedImageURLs(value any, results *[]string) {
	switch current := value.(type) {
	case map[string]any:
		if rawURL := imageURLFromCardData(current); rawURL != "" {
			appendCapturedImageURL(results, rawURL)
		}
		moderated, _ := current["moderated"].(bool)
		progress, hasProgress := numberAsInt(current["progress"])
		if !moderated && hasProgress && progress >= 100 {
			appendCapturedImageURL(results, firstString(current, "imageUrl", "image_url", "url"))
		}
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case []any:
		for _, nested := range current {
			collectCapturedImageURLs(nested, results)
		}
	case string:
		trimmed := strings.TrimSpace(current)
		if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
			var nested any
			if json.Unmarshal([]byte(trimmed), &nested) == nil {
				collectCapturedImageURLs(nested, results)
				return
			}
		}
		appendCapturedImageURL(results, trimmed)
	}
}

func appendCapturedImageURL(results *[]string, value string) {
	value = strings.TrimSpace(value)
	if !strings.Contains(value, "/generated/") || strings.Contains(value, "-part-") || strings.ContainsAny(value, "{}[]\"") {
		return
	}
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "users/") && !strings.HasPrefix(value, "/users/") {
		return
	}
	value = absoluteAssetURL(value)
	if !containsString(*results, value) {
		*results = append(*results, value)
	}
}

func (a *Adapter) uploadFileV2Direct(ctx context.Context, cfg Config, lease *infraegress.Lease, token string, file provider.ImageInput, referer, fileSource, stage string) (uploadedFile, error) {
	body, contentType, err := buildDirectFileUploadBody(file, fileSource)
	if err != nil {
		return uploadedFile{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, cfg.BaseURL+"/http/upload-file-v2/direct", bytes.NewReader(body))
	if err != nil {
		return uploadedFile{}, err
	}
	request.Header = buildHeaders(token, lease, contentType)
	request.Header.Del("x-xai-request-id")
	applyAppHeaders(request.Header, cfg.BaseURL, referer)
	response, err := lease.DoDeferredForbidden(request)
	if err != nil {
		return uploadedFile{}, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		responseBody, readErr := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
		if readErr != nil {
			return uploadedFile{}, fmt.Errorf("读取 V2 上传文件错误响应: %w", readErr)
		}
		truncated := len(responseBody) > webMediaDiagnosticBodyLimit
		if truncated {
			responseBody = responseBody[:webMediaDiagnosticBodyLimit]
		}
		upstreamErr := newWebMediaUpstreamError(response.StatusCode, responseBody, truncated)
		if isClearanceRefreshableMediaError(upstreamErr) {
			lease.InvalidateClearance()
		}
		a.logWebMediaUpstreamRejection(stage, response, upstreamErr)
		return uploadedFile{}, upstreamErr
	}
	uploaded, err := decodeDirectFileUploadResponse(io.LimitReader(response.Body, directFileUploadResponseLimit))
	if err != nil {
		return uploadedFile{}, err
	}
	return uploaded, nil
}

func buildDirectFileUploadBody(file provider.ImageInput, fileSource string) ([]byte, string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename="%s"`, browserMultipartFilename(file.Filename)))
	header.Set("Content-Type", file.MIMEType)
	part, err := writer.CreatePart(header)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(file.Data); err != nil {
		return nil, "", err
	}
	if fileSource != "" {
		if err := writer.WriteField("file_source", fileSource); err != nil {
			return nil, "", err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, "", err
	}
	return body.Bytes(), writer.FormDataContentType(), nil
}

func browserMultipartFilename(value string) string {
	value = strings.Map(func(character rune) rune {
		switch {
		case character == '\r' || character == '\n':
			return -1
		case character < 0x20 || character == 0x7f:
			return '_'
		default:
			return character
		}
	}, value)
	if strings.TrimSpace(value) == "" {
		value = "upload.bin"
	}
	return strings.NewReplacer("\\", "\\\\", `"`, `\"`).Replace(value)
}

func decodeDirectFileUploadResponse(source io.Reader) (uploadedFile, error) {
	var value struct {
		UploadID      string          `json:"uploadId"`
		TerminalError json.RawMessage `json:"terminalError"`
		FileMetadata  struct {
			ID      string `json:"fileMetadataId"`
			FileID  string `json:"fileId"`
			FileURI string `json:"fileUri"`
		} `json:"fileMetadata"`
	}
	if err := json.NewDecoder(source).Decode(&value); err != nil {
		return uploadedFile{}, fmt.Errorf("V2 上传文件响应无效: %w", err)
	}
	if directFileUploadTerminalError(value.TerminalError) {
		return uploadedFile{}, errors.New("V2 上传文件被上游拒绝")
	}
	metadataID := strings.TrimSpace(value.FileMetadata.ID)
	fileID := metadataID
	if fileID == "" {
		fileID = strings.TrimSpace(value.FileMetadata.FileID)
	}
	if fileID == "" {
		// Some successful uploads complete asynchronously and only expose the
		// upload task ID. Gateway accepts it as the file reference; prefer the
		// browser's fileMetadataId whenever it is already available.
		fileID = strings.TrimSpace(value.UploadID)
	}
	fileURI := ""
	if value.FileMetadata.FileURI != "" {
		fileURI = absoluteAssetURL(value.FileMetadata.FileURI)
	}
	if fileID == "" && fileURI == "" {
		return uploadedFile{}, fmt.Errorf("V2 上传文件成功但上游未返回完整文件标识")
	}
	return uploadedFile{ID: fileID, MetadataID: metadataID, URI: fileURI}, nil
}

func directFileUploadTerminalError(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) || bytes.Equal(trimmed, []byte("0")) {
		return false
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		return true
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed) != ""
	case map[string]any:
		return len(typed) != 0
	case []any:
		return len(typed) != 0
	case bool:
		return typed
	case float64:
		return typed != 0
	default:
		return value != nil
	}
}

func (a *Adapter) postJSON(ctx context.Context, cfg Config, lease *infraegress.Lease, token, endpoint string, payload any, timeout time.Duration) (*http.Response, error) {
	return a.postJSONWithReferer(ctx, cfg, lease, token, endpoint, payload, timeout, cfg.BaseURL+"/imagine")
}

func (a *Adapter) postJSONWithReferer(ctx context.Context, cfg Config, lease *infraegress.Lease, token, endpoint string, payload any, timeout time.Duration, referer string) (*http.Response, error) {
	data, _ := json.Marshal(payload)
	for attempt := 0; attempt < 2; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(data))
		if err != nil {
			cancel()
			return nil, err
		}
		request.Header = buildHeaders(token, lease, "application/json")
		applyAppHeaders(request.Header, cfg.BaseURL, referer)
		a.applySignedStatsig(requestCtx, request, token, lease)
		response, err := lease.DoDeferredForbidden(request)
		if err != nil {
			cancel()
			return nil, err
		}
		if response.StatusCode == http.StatusForbidden {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, webMediaDiagnosticBodyLimit+1))
			_ = response.Body.Close()
			cancel()
			if readErr != nil {
				return nil, fmt.Errorf("读取 Grok Web 403 响应: %w", readErr)
			}
			truncated := len(body) > webMediaDiagnosticBodyLimit
			if truncated {
				body = body[:webMediaDiagnosticBodyLimit]
			}
			upstreamErr := newWebMediaUpstreamError(response.StatusCode, body, truncated)
			response.Body = io.NopCloser(bytes.NewReader(body))
			response.ContentLength = int64(len(body))
			if isClearanceRefreshableMediaError(upstreamErr) {
				lease.InvalidateClearance()
				_ = a.invalidateSignedStatsig(http.MethodPost, endpoint)
				return response, nil
			}
			// Code 7 is the application-layer equivalent of reloading the Grok
			// page: refresh only the path-bound Statsig signature and replay the
			// explicitly rejected POST once. It is not a Cloudflare challenge, so
			// the current Clearance lease remains valid.
			if isStatsigRefreshableMediaError(upstreamErr, body) {
				if attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, endpoint) {
					continue
				}
				return response, nil
			}
			// Remaining structured JSON responses are application policy decisions.
			// They must not invalidate Clearance, affect egress health, or be replayed.
			if upstreamErr.bodyKind == "json" || attempt > 0 || !a.invalidateSignedStatsig(http.MethodPost, endpoint) {
				return response, nil
			}
			continue
		}
		response.Body = &cancelBody{ReadCloser: response.Body, cancel: cancel}
		return response, nil
	}
	return nil, fmt.Errorf("Grok Web Statsig 刷新失败")
}

func (a *Adapter) imageResponse(ctx context.Context, credential account.Credential, urls, blobs []string, count int, format string) (*provider.Response, error) {
	data := make([]any, 0, min(count, len(urls)))
	for index := 0; index < count && index < len(urls); index++ {
		blob := ""
		if index < len(blobs) {
			blob = blobs[index]
		}
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: urls[index], Blob: blob}, format)
		if err != nil {
			return nil, err
		}
		data = append(data, item)
	}
	return jsonProviderResponse(http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data}), nil
}

func (a *Adapter) imageDataItem(ctx context.Context, credential account.Credential, image imagineImageValue, format string) (map[string]any, error) {
	if a.assets == nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片媒体存储未配置"))
	}
	raw, err := a.imageBytes(ctx, credential, image)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
	}
	asset, err := a.saveImageWithRetry(ctx, raw)
	if err != nil {
		return nil, provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	if format != "b64_json" {
		return map[string]any{"url": a.assets.PublicImageURL(asset.ID), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
	}
	return map[string]any{"b64_json": base64.StdEncoding.EncodeToString(raw), "mime_type": asset.MIMEType, "revised_prompt": ""}, nil
}

// saveImageWithRetry 只重试当前生成结果的本地持久化，不重新请求上游生成。
func (a *Adapter) saveImageWithRetry(ctx context.Context, raw []byte) (mediadomain.Asset, error) {
	var lastErr error
	for attempt := 0; attempt < mediaOutputAttempts; attempt++ {
		asset, err := a.assets.SaveImage(ctx, raw)
		if err == nil {
			return asset, nil
		}
		lastErr = err
		if ctx.Err() != nil || attempt+1 >= mediaOutputAttempts {
			break
		}
		if err := waitMediaOutputRetry(ctx, attempt); err != nil {
			return mediadomain.Asset{}, err
		}
	}
	return mediadomain.Asset{}, lastErr
}

func (a *Adapter) imageBytes(ctx context.Context, credential account.Credential, image imagineImageValue) ([]byte, error) {
	if strings.TrimSpace(image.Blob) != "" {
		raw, err := decodeImageBlob(image.Blob)
		if err == nil {
			return raw, nil
		}
		if strings.TrimSpace(image.URL) == "" {
			return nil, err
		}
	}
	return a.downloadImage(ctx, credential, image.URL)
}

func (a *Adapter) streamImagineImages(ctx context.Context, writer io.Writer, connection *infraegress.WebSocket, lease *infraegress.Lease, credential account.Credential, count, partialImages int, modelConfig imagineModelConfig, observe func(provider.ImageGenerationObservation)) error {
	defer lease.Release()
	collector := newImagineCollector()
	emitted := 0
	partialIndex := 0
	for emitted < count {
		messageType, data, readErr := connection.ReadMessage()
		if readErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			lease.ObserveWebSocketError(readErr)
			return readErr
		}
		if messageType != websocket.TextMessage {
			continue
		}
		var message map[string]any
		if json.Unmarshal(data, &message) != nil {
			continue
		}
		if message["type"] == "error" {
			provider.ObserveImageGeneration(observe, provider.ImageGenerationObservation{Failed: true})
			upstreamErr := fmt.Errorf("Imagine WebSocket 返回错误")
			lease.Observe(0, upstreamErr)
			return upstreamErr
		}
		collector.Accept(message)
		provider.ObserveImageGeneration(observe, provider.ImageGenerationObservation{OutputImages: collector.UsableCount(), QuotaUnits: collector.UsableCount(), Completed: collector.UsableCount() >= count})
		if partialImages > 0 {
			for _, image := range collector.ReadyPreviews() {
				if partialIndex >= partialImages {
					continue
				}
				raw, err := a.imageBytes(ctx, credential, image)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					continue
				}
				if err := writeSSE(writer, "image_generation.partial_image", openAIImageStreamEvent("image_generation.partial_image", image, raw, partialIndex)); err != nil {
					return err
				}
				partialIndex++
			}
		}
		for _, image := range collector.ReadyImages() {
			if emitted >= count {
				break
			}
			raw, err := a.imageBytes(ctx, credential, image)
			if err != nil {
				return provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, err)
			}
			if err := a.saveStreamImage(ctx, raw); err != nil {
				return err
			}
			if err := writeSSE(writer, "image_generation.completed", openAIImageStreamEvent("image_generation.completed", image, raw, 0)); err != nil {
				return err
			}
			emitted++
		}
		if collector.Done(modelConfig.ExpectedCount) && emitted < count {
			incompleteErr := fmt.Errorf("上游仅返回 %d/%d 张可用图片", emitted, count)
			return incompleteErr
		}
	}
	lease.Observe(http.StatusOK, nil)
	return nil
}

func openAIImageStreamEvent(eventType string, image imagineImageValue, raw []byte, partialIndex int) map[string]any {
	width, height := image.Width, image.Height
	size := "auto"
	if width > 0 && height > 0 {
		size = fmt.Sprintf("%dx%d", width, height)
	}
	value := map[string]any{
		"type": eventType, "b64_json": base64.StdEncoding.EncodeToString(raw),
		"created_at": time.Now().Unix(), "size": size, "quality": "auto",
		"background": "auto", "output_format": imageOutputFormat(raw),
	}
	if eventType == "image_generation.partial_image" {
		value["partial_image_index"] = partialIndex
	}
	return value
}

func imageOutputFormat(raw []byte) string {
	mimeType := http.DetectContentType(raw)
	switch mimeType {
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	default:
		return "jpeg"
	}
}

func (a *Adapter) saveStreamImage(ctx context.Context, raw []byte) error {
	if a.assets == nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, fmt.Errorf("图片媒体存储未配置"))
	}
	if _, err := a.saveImageWithRetry(ctx, raw); err != nil {
		return provider.NewMediaPostProcessingError(provider.MediaPostProcessingStorage, err)
	}
	return nil
}

func (a *Adapter) downloadImage(ctx context.Context, credential account.Credential, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || !trustedImageAssetHost(parsed.Hostname()) || parsed.User != nil {
		return nil, fmt.Errorf("图片内容 URL 不受信任")
	}
	token, err := a.cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return nil, err
	}
	downloadCtx, cancel := context.WithTimeout(ctx, imageDownloadTimeout)
	defer cancel()
	var lastErr error
	for attempt := 0; attempt < mediaOutputAttempts; attempt++ {
		raw, retryable, attemptErr := a.downloadImageAttempt(downloadCtx, credential, token, parsed.String())
		if attemptErr == nil {
			return raw, nil
		}
		lastErr = attemptErr
		if !retryable || downloadCtx.Err() != nil || attempt+1 >= mediaOutputAttempts {
			break
		}
		if err := waitMediaOutputRetry(downloadCtx, attempt); err != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// downloadImageAttempt 每次沿用同一账号，只允许出口管理器重新选择资源节点。
func (a *Adapter) downloadImageAttempt(ctx context.Context, credential account.Credential, token, rawURL string) ([]byte, bool, error) {
	lease, err := a.egress.AcquireCredential(ctx, domainegress.ScopeWebAsset, credential)
	if err != nil {
		return nil, true, err
	}
	defer lease.Release()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, false, err
	}
	request.Header = buildHeaders(token, lease, "")
	request.Header.Del("Content-Type")
	response, err := lease.Do(request)
	if err != nil {
		lease.Observe(0, err)
		return nil, ctx.Err() == nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		lease.Observe(response.StatusCode, nil)
		retryable := response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusRequestTimeout || response.StatusCode == http.StatusTooEarly || response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500
		return nil, retryable, fmt.Errorf("下载图片返回 %d", response.StatusCode)
	}
	contentType := strings.ToLower(strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0]))
	if contentType != "" && !strings.HasPrefix(contentType, "image/") {
		return nil, false, fmt.Errorf("上游图片 Content-Type 无效")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
	if err != nil {
		lease.Observe(0, err)
		return nil, ctx.Err() == nil, fmt.Errorf("读取图片内容: %w", err)
	}
	if len(raw) > 32<<20 {
		return nil, false, fmt.Errorf("图片下载超过 32 MiB")
	}
	lease.Observe(response.StatusCode, nil)
	return raw, false, nil
}

func waitMediaOutputRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(mediadomain.OutputPollDelay(attempt))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func decodeImageBlob(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "data:") {
		comma := strings.IndexByte(value, ',')
		if comma < 0 || !strings.Contains(strings.ToLower(value[:comma]), ";base64") {
			return nil, fmt.Errorf("图片 blob data URI 无效")
		}
		value = value[comma+1:]
	}
	if value == "" || base64.StdEncoding.DecodedLen(len(value)) > 32<<20 {
		return nil, fmt.Errorf("图片 blob 为空或超过 32 MiB")
	}
	raw, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(value)
	}
	if err != nil || len(raw) == 0 || len(raw) > 32<<20 {
		return nil, fmt.Errorf("图片 blob Base64 无效")
	}
	return raw, nil
}

func trustedImageAssetHost(host string) bool {
	return strings.EqualFold(host, "assets.grok.com") || strings.EqualFold(host, "imagine-public.x.ai") || strings.EqualFold(host, "imgen.x.ai")
}

func imagineURL(baseURL string) (string, error) {
	value, err := url.Parse(baseURL)
	if err != nil {
		return "", err
	}
	switch value.Scheme {
	case "https":
		value.Scheme = "wss"
	case "http":
		value.Scheme = "ws"
	default:
		return "", fmt.Errorf("Grok Web Base URL 协议无效")
	}
	value.Path = "/ws/imagine/listen"
	value.RawQuery = ""
	return value.String(), nil
}

func imagineResetMessage() map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"type": "reset"}}}}
}

func imagineRequestMessage(id, prompt, ratio string, nsfw, pro bool, generations int) map[string]any {
	return map[string]any{"type": "conversation.item.create", "timestamp": time.Now().UnixMilli(), "item": map[string]any{"type": "message", "content": []any{map[string]any{"requestId": id, "text": prompt, "type": "input_text", "properties": map[string]any{"section_count": 0, "is_kids_mode": false, "enable_nsfw": nsfw, "skip_upsampler": false, "enable_side_by_side": true, "is_initial": false, "aspect_ratio": ratio, "enable_pro": pro, "num_generations": generations}}}}}
}

func resolveImageAspectRatio(aspectRatio, size string) (string, error) {
	values := map[string]string{
		"auto": "auto", "1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4",
		"3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2", "19.5:9": "19.5:9", "9:19.5": "9:19.5", "20:9": "20:9", "9:20": "9:20",
		"1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3", "1024x1024": "1:1",
	}
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" {
		return "auto", nil
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", fmt.Errorf("aspect_ratio 不受支持")
}

func resolveAspectRatio(size string) string {
	if strings.TrimSpace(size) == "" {
		return "1:1"
	}
	value, err := resolveImageAspectRatio("", size)
	if err != nil {
		return "1:1"
	}
	return value
}

func imageIDFromURL(value string) string {
	parts := strings.Split(strings.Trim(value, "/"), "/")
	if len(parts) == 0 {
		return value
	}
	name := parts[len(parts)-1]
	if index := strings.IndexByte(name, '.'); index > 0 {
		return name[:index]
	}
	return name
}

func absoluteAssetURL(value string) string {
	if strings.HasPrefix(value, "https://") {
		return value
	}
	return "https://assets.grok.com/" + strings.TrimPrefix(value, "/")
}

func extractMarkdownImages(value string) []string {
	results := make([]string, 0, 2)
	for {
		start := strings.Index(value, "![image](")
		if start < 0 {
			break
		}
		value = value[start+len("![image]("):]
		end := strings.IndexByte(value, ')')
		if end < 0 {
			break
		}
		results = append(results, value[:end])
		value = value[end+1:]
	}
	return results
}
