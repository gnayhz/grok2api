package console

// Console 媒体原生编解码(native request/response codec):
// 请求参数归一(格式/分辨率/质量/宽高比/输入 URL 校验)与
// 响应解析(视频创建/状态轮询)。调用与传输在 media.go。

import (
	"encoding/json"
	"errors"
	"fmt"
	provider "github.com/chenyme/grok2api/backend/internal/port/provider"
	"net/url"
	"strings"
)

func normalizeConsoleImageFormat(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "url", nil
	}
	if value != "url" && value != "b64_json" {
		return "", errors.New("response_format 必须是 url 或 b64_json")
	}
	return value, nil
}

func normalizeConsoleImageResolution(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if value != "1k" && value != "2k" {
		return "", errors.New("resolution 必须是 1k 或 2k")
	}
	return value, nil
}

func normalizeConsoleImageQuality(model, value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return "", nil
	}
	if strings.TrimSpace(model) != "grok-imagine-image-2.0" {
		return "", errors.New("quality 仅支持 grok-imagine-image-2.0")
	}
	if value != "low" && value != "medium" {
		return "", errors.New("quality 必须是 low 或 medium")
	}
	return value, nil
}

func resolveConsoleImageAspectRatio(aspectRatio, size string) (string, error) {
	value := strings.ToLower(strings.TrimSpace(aspectRatio))
	if value == "" {
		value = strings.ToLower(strings.TrimSpace(size))
	}
	if value == "" || value == "auto" {
		return "", nil
	}
	values := map[string]string{
		"1:1": "1:1", "16:9": "16:9", "9:16": "9:16", "4:3": "4:3", "3:4": "3:4", "3:2": "3:2", "2:3": "2:3", "2:1": "2:1", "1:2": "1:2",
		"1024x1024": "1:1", "1280x720": "16:9", "720x1280": "9:16", "1792x1024": "3:2", "1536x1024": "3:2", "1024x1792": "2:3", "1024x1536": "2:3",
	}
	if resolved := values[value]; resolved != "" {
		return resolved, nil
	}
	return "", errors.New("aspect_ratio 或 size 不受支持")
}

func validConsoleMediaInputURL(value, mediaType string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	if strings.HasPrefix(lower, "data:"+mediaType+"/") {
		return strings.Contains(lower, ";base64,")
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func parseConsoleVideoCreate(body []byte) (string, error) {
	var payload struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("解析 Console 视频创建响应: %w", err)
	}
	if strings.TrimSpace(payload.RequestID) == "" {
		return "", errors.New("Console 视频创建响应缺少 request_id")
	}
	return payload.RequestID, nil
}

func parseConsoleVideoStatus(body []byte, progress func(int)) (provider.VideoResult, bool, error) {
	var payload struct {
		Status   string `json:"status"`
		Progress int    `json:"progress"`
		Video    struct {
			URL string `json:"url"`
		} `json:"video"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return provider.VideoResult{}, false, fmt.Errorf("解析 Console 视频状态响应: %w", err)
	}
	if progress != nil && payload.Progress > 0 {
		progress(min(99, payload.Progress))
	}
	switch status := strings.ToLower(strings.TrimSpace(payload.Status)); status {
	case "done", "completed", "succeeded", "success", "ready":
		if strings.TrimSpace(payload.Video.URL) == "" {
			return provider.VideoResult{ContentType: "video/mp4"}, true, provider.NewMediaPostProcessingError(provider.MediaPostProcessingDownload, errors.New("Console 视频生成完成但没有返回内容 URL"))
		}
		return provider.VideoResult{URL: strings.TrimSpace(payload.Video.URL), ContentType: "video/mp4"}, true, nil
	case "failed", "expired", "cancelled", "canceled", "error":
		message := safeConsoleMediaErrorValue(payload.Error)
		if message == "" {
			message = strings.ToLower(strings.TrimSpace(payload.Status))
		}
		return provider.VideoResult{}, false, &provider.VideoGenerationFailure{Err: fmt.Errorf("Console 视频生成失败: %s", message)}
	case "pending", "processing", "in_progress", "queued":
		return provider.VideoResult{}, false, nil
	default:
		return provider.VideoResult{}, false, fmt.Errorf("Console 视频状态无效: %q", safeConsoleMediaText(status))
	}
}
