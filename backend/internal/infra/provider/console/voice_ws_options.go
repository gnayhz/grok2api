package console

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
)

// PrepareVoiceWebSocket is pure protocol preparation. It runs before account
// hydration/network activity and owns both supported query fields and their
// wire values. Model selection remains an explicit Gateway input.
func (a *Adapter) PrepareVoiceWebSocket(request provider.VoiceWebSocketRequest) (provider.VoiceWebSocketRequest, error) {
	path := strings.TrimSpace(request.Path)
	if path != "/stt" && path != "/realtime" {
		return request, voiceWebSocketValidation("path", "不支持的 voice websocket path")
	}
	query := make(url.Values, len(request.Query))
	for key, values := range request.Query {
		if path != "/stt" {
			return request, voiceWebSocketValidation(key, "不支持的 realtime WebSocket 参数")
		}
		if key == "keyterm" {
			if len(values) == 0 || len(values) > 100 {
				return request, voiceWebSocketValidation(key, "keyterm 必须为1–100个词条")
			}
			for _, v := range values {
				if strings.TrimSpace(v) == "" || utf8.RuneCountInString(v) > 50 {
					return request, voiceWebSocketValidation(key, "keyterm 每个词条必须为1–50个字符")
				}
			}
			query[key] = append([]string(nil), values...)
			continue
		}
		if len(values) != 1 {
			return request, voiceWebSocketValidation(key, "该 WebSocket 参数必须只出现一次")
		}
		value := strings.TrimSpace(values[0])
		valid := false
		switch key {
		case "encoding":
			valid = value == "pcm" || value == "mulaw" || value == "alaw" || value == "opus"
		case "sample_rate":
			switch value {
			case "8000", "16000", "22050", "24000", "44100", "48000":
				valid = true
			}
		case "interim_results", "multichannel", "diarize", "filler_words":
			parsed, err := strconv.ParseBool(value)
			valid = err == nil
			value = strconv.FormatBool(parsed)
		case "endpointing", "channels", "smart_turn_timeout":
			parsed, err := strconv.Atoi(value)
			lo, hi := 0, 5000
			if key == "channels" {
				lo, hi = 1, 8
			}
			if key == "smart_turn_timeout" {
				lo = 1
			}
			valid = err == nil && parsed >= lo && parsed <= hi
			value = strconv.Itoa(parsed)
		case "smart_turn", "vad_threshold":
			parsed, err := strconv.ParseFloat(value, 64)
			valid = err == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) && parsed >= 0 && parsed <= 1
			value = strconv.FormatFloat(parsed, 'f', -1, 64)
		case "language":
			// BCP-47 syntax is preserved without copying the changing supported-language
			// catalogue. The Provider may still return a request-scoped refusal.
			valid = len(value) > 0 && len(value) <= 128
			for _, char := range value {
				if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-') {
					valid = false
				}
			}
		default:
			return request, voiceWebSocketValidation(key, "不支持的 STT WebSocket 参数")
		}
		if !valid {
			return request, voiceWebSocketValidation(key, "无效的 STT WebSocket 参数值")
		}
		query.Set(key, value)
	}
	if query.Get("multichannel") == "true" {
		channels, _ := strconv.Atoi(query.Get("channels"))
		if channels < 2 {
			return request, voiceWebSocketValidation("channels", "multichannel=true 需要 channels 为2–8")
		}
		if query.Get("encoding") == "opus" {
			return request, voiceWebSocketValidation("encoding", "Opus WebSocket 只支持单声道")
		}
	}
	if query.Get("encoding") == "opus" && query.Get("channels") != "" && query.Get("channels") != "1" {
		return request, voiceWebSocketValidation("channels", "Opus WebSocket 只支持单声道")
	}
	if query.Has("smart_turn_timeout") && !query.Has("smart_turn") {
		return request, voiceWebSocketValidation("smart_turn_timeout", "smart_turn_timeout 需要 smart_turn")
	}
	request.Path, request.Query = path, query
	return request, nil
}

func voiceWebSocketValidation(param, message string) error {
	return &inferencedomain.RequestValidationError{Code: "invalid_request", Param: param, Message: message}
}
