package web

// Web 方言上游响应解析(native response codec):SSE 帧消费、
// modelResponse 收集、错误/用量/图片与搜索工具结果归并。
// 请求解码在 chat_request_codec.go;调用与流处理在 chat.go。

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	provider "github.com/chenyme/grok2api/backend/internal/port/provider"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func consumeUpstream(source io.Reader, emit func(string, string) error) (parsedChat, error) {
	return consumeUpstreamWithCitations(source, emit, true)
}

func consumeUpstreamWithCitations(source io.Reader, emit func(string, string) error, inlineCitations bool) (parsedChat, error) {
	parsed := parsedChat{DisableInlineCitations: !inlineCitations}
	err := consumeUpstreamInto(source, &parsed, emit)
	return parsed, err
}

func consumeUpstreamInto(source io.Reader, parsed *parsedChat, emit func(string, string) error) error {
	var budget *responsebuffer.Budget
	if parsed.resources != nil {
		budget = parsed.resources.budget
	}
	return consumeJSONObjectsWithBudget(source, 8<<20, budget, func(data []byte) error {
		if budget != nil {
			workspace, err := responsebuffer.JSONWorkspace(budget, data)
			if err != nil {
				return err
			}
			defer workspace.Release()
		}
		kind, delta, err := parseUpstreamFrame(data, parsed)
		if err != nil {
			return err
		}
		if err := parsed.resources.retainFrame(data, kind, delta); err != nil {
			return err
		}
		if emit == nil {
			return nil
		}
		// Always invoke emit so streaming can flush tool/citation side-channels
		// even when the frame produced no visible text delta.
		return emit(kind, delta)
	})
}

// consumeJSONObjects borrows complete frames from each read buffer. Only a
// frame split across reads is copied; consumers must not retain the slice.
func consumeJSONObjects(source io.Reader, maxObjectBytes int, consume func([]byte) error) error {
	return consumeJSONObjectsWithBudget(source, maxObjectBytes, nil, consume)
}

func consumeJSONObjectsWithBudget(source io.Reader, maxObjectBytes int, budget *responsebuffer.Budget, consume func([]byte) error) error {
	var readState *responsebuffer.State
	if budget != nil {
		readState = responsebuffer.NewState(budget, maxObjectBytes*2+64<<10)
		defer readState.Close()
		if err := readState.Grow(0, 64<<10); err != nil {
			return err
		}
	}
	reserveFrame := func(n int) error {
		if readState == nil {
			return nil
		}
		return readState.Grow(0, (64<<10)+n*2)
	}
	buffer := make([]byte, 64<<10)
	var frame []byte
	depth := 0
	inString, escaped := false, false
	emptyReads := 0
	tooLarge := func() error {
		return fmt.Errorf("Grok Web 单个响应帧超过 %d MiB", maxObjectBytes>>20)
	}
	for {
		n, readErr := source.Read(buffer)
		if n == 0 && readErr == nil {
			emptyReads++
			if emptyReads >= 100 {
				return io.ErrNoProgress
			}
			continue
		}
		emptyReads = 0
		start := 0
		for index := 0; index < n; index++ {
			value := buffer[index]
			if depth == 0 {
				if value != '{' {
					continue
				}
				start = index
				depth = 1
				inString, escaped = false, false
				continue
			}
			if inString {
				if escaped {
					escaped = false
				} else if value == '\\' {
					escaped = true
				} else if value == '"' {
					inString = false
				}
				continue
			}
			switch value {
			case '"':
				inString = true
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					part := buffer[start : index+1]
					if len(frame)+len(part) > maxObjectBytes {
						return tooLarge()
					}
					if len(frame) > 0 {
						if err := reserveFrame(len(frame) + len(part)); err != nil {
							return err
						}
						frame = append(frame, part...)
						part = frame
					}
					if err := consume(part); err != nil {
						return err
					}
					frame = frame[:0]
				}
			}
		}
		if depth != 0 {
			if len(frame)+n-start > maxObjectBytes {
				return tooLarge()
			}
			if err := reserveFrame(len(frame) + n - start); err != nil {
				return err
			}
			frame = append(frame, buffer[start:n]...)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if depth != 0 {
					return io.ErrUnexpectedEOF
				}
				return nil
			}
			return readErr
		}
	}
}

func parseUpstreamFrame(data []byte, parsed *parsedChat) (string, string, error) {
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return "", "", nil
	}
	if event, ok := root["event"].(map[string]any); ok {
		return parseGatewayEvent(event, parsed)
	}
	if errorValue, ok := root["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	result, _ := root["result"].(map[string]any)
	if conversation, _ := result["conversation"].(map[string]any); conversation != nil {
		parsed.ConversationID, _ = conversation["conversationId"].(string)
		return "", "", nil
	}
	response, _ := result["response"].(map[string]any)
	if response == nil {
		return "", "", nil
	}
	if errorValue, ok := response["error"].(map[string]any); ok {
		return "", "", webResponseError(errorValue)
	}
	for _, key := range []string{"cardAttachment", "cardAttachments"} {
		if rawURL := collectCardAttachment(parsed, response[key]); rawURL != "" {
			rawURL = absoluteAssetURL(rawURL)
			parsed.Images = appendUniqueString(parsed.Images, rawURL)
			return "image", rawURL, nil
		}
	}
	if userResponse, _ := response["userResponse"].(map[string]any); userResponse != nil {
		if id, _ := userResponse["responseId"].(string); id != "" {
			parsed.ParentID = id
		}
	}
	collectSearchSources(parsed, response)
	token, _ := response["token"].(string)
	thinking, _ := response["isThinking"].(bool)
	tag, _ := response["messageTag"].(string)
	if tag == "tool_usage_card" {
		collectServerTool(parsed, response)
		// tool_usage_card 的 token 是 Grok 内部 XML 协议，不属于模型 reasoning。
		return "", "", nil
	}
	if token != "" && thinking {
		parsed.Reasoning.WriteString(token)
		return "reasoning", token, nil
	}
	if token != "" && !thinking && (tag == "final" || tag == "") {
		parsed.upstreamText.WriteString(token)
		cleaned := cleanChatToken(parsed, token)
		parsed.appendText(cleaned)
		return "text", cleaned, nil
	}
	if modelResponse, _ := response["modelResponse"].(map[string]any); modelResponse != nil {
		return collectModelResponse(parsed, modelResponse)
	}
	if imageResponse, _ := response["streamingImageGenerationResponse"].(map[string]any); imageResponse != nil {
		rawURL, _ := imageResponse["imageUrl"].(string)
		if rawURL == "" {
			rawURL, _ = imageResponse["url"].(string)
		}
		if rawURL != "" {
			moderated, _ := imageResponse["moderated"].(bool)
			if moderated {
				markModeratedImage(parsed, rawURL)
				return "", "", nil
			}
			completed, _ := imageResponse["isFinal"].(bool)
			if completed || imageResponse["progress"] == float64(100) {
				rawURL = absoluteAssetURL(rawURL)
				parsed.Images = appendUniqueString(parsed.Images, rawURL)
				return "image", rawURL, nil
			}
		}
	}
	return "", "", nil
}

func collectModelResponse(parsed *parsedChat, modelResponse map[string]any) (string, string, error) {
	if err := modelResponseStreamError(modelResponse); err != nil {
		return "", "", err
	}
	if parsed.ParentID == "" {
		parsed.ParentID, _ = modelResponse["parentResponseId"].(string)
	}
	collectSearchSources(parsed, modelResponse)
	firstImage := collectModelResponseImages(parsed, modelResponse)
	message, _ := modelResponse["message"].(string)
	if delta := mergeModelResponseText(parsed, message); delta != "" {
		return "text", delta, nil
	}
	if firstImage != "" {
		return "image", firstImage, nil
	}
	return "", "", nil
}

func mergeModelResponseText(parsed *parsedChat, message string) string {
	if message == "" {
		return ""
	}
	raw := parsed.upstreamText.String()
	if raw == message || strings.HasPrefix(raw, message) {
		return ""
	}
	if raw != "" && !strings.HasPrefix(message, raw) {
		// 已输出内容与最终 envelope 不同，保留已输出结果，避免重复或回滚流式内容。
		return ""
	}
	delta := message[len(raw):]
	parsed.upstreamText.WriteString(delta)
	delta = cleanChatToken(parsed, delta)
	parsed.appendText(delta)
	return delta
}

func modelResponseStreamError(modelResponse map[string]any) error {
	values, _ := modelResponse["streamErrors"].([]any)
	for _, raw := range values {
		switch value := raw.(type) {
		case string:
			if message := strings.TrimSpace(value); message != "" {
				return errors.New(message)
			}
		case map[string]any:
			if nested, _ := value["error"].(map[string]any); nested != nil {
				return webResponseError(nested)
			}
			if message := firstString(value, "message", "error", "detail"); message != "" {
				return webResponseError(map[string]any{"message": message, "code": value["code"]})
			}
		}
	}
	return nil
}

func webResponseError(value map[string]any) error {
	message, _ := value["message"].(string)
	if message == "" {
		message = "Grok Web stream error"
	}
	code, _ := numberAsInt(value["code"])
	if code == 7 || strings.Contains(strings.ToLower(message), "anti-bot") {
		return fmt.Errorf("%w: %s", errWebAntiBot, message)
	}
	normalized := strings.ToLower(message)
	if strings.Contains(normalized, "usage limit") || strings.Contains(normalized, "usage quota") {
		return fmt.Errorf("%w: %s", errWebUsageLimit, message)
	}
	return errors.New(message)
}

func antiBotProviderResponse() *provider.Response {
	return jsonProviderResponse(http.StatusForbidden, map[string]any{"error": map[string]any{
		"message": "Grok Web 出口会话被上游反机器人规则拒绝，请检查代理、User-Agent 与 Cloudflare Cookie 是否来自同一浏览器会话",
		"type":    "upstream_error", "code": "anti_bot_rejected",
	}})
}

func usageLimitProviderResponse() *provider.Response {
	return jsonProviderResponse(http.StatusTooManyRequests, map[string]any{"error": map[string]any{
		"message": "Grok Web 账号已达用量上限，请稍后重试",
		"type":    "rate_limit_error", "code": "usage_limit_reached",
	}})
}

func collectModelResponseImages(parsed *parsedChat, modelResponse map[string]any) string {
	first := ""
	appendImage := func(value string) {
		if strings.TrimSpace(value) == "" {
			return
		}
		value = absoluteAssetURL(value)
		if _, moderated := parsed.moderatedImages[value]; moderated {
			return
		}
		if containsString(parsed.Images, value) {
			return
		}
		parsed.Images = append(parsed.Images, value)
		if first == "" {
			first = value
		}
	}
	if urls, ok := modelResponse["generatedImageUrls"].([]any); ok {
		for _, raw := range urls {
			value, _ := raw.(string)
			appendImage(value)
		}
	}
	if cards, ok := modelResponse["cardAttachmentsJson"].([]any); ok {
		for _, raw := range cards {
			encoded, _ := raw.(string)
			var card map[string]any
			if encoded == "" || json.Unmarshal([]byte(encoded), &card) != nil {
				continue
			}
			appendImage(imageURLFromCardData(card))
		}
	}
	return first
}

func markModeratedImage(parsed *parsedChat, value string) {
	if strings.TrimSpace(value) == "" {
		return
	}
	if parsed.moderatedImages == nil {
		parsed.moderatedImages = make(map[string]struct{})
	}
	parsed.moderatedImages[absoluteAssetURL(value)] = struct{}{}
}

func collectSearchSources(parsed *parsedChat, response map[string]any) {
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	collectWebSearchResults(parsed, response["webSearchResults"])
	collectWebSearchResults(parsed, response["citedWebSearchResults"])
	collectXSearchResults(parsed, response["xSearchResults"])
	collectXSearchResults(parsed, response["xposts"])
	collectXSearchResults(parsed, response["citedXposts"])
}

func collectWebSearchResults(parsed *parsedChat, value any) {
	if wrapped, _ := value.(map[string]any); wrapped != nil {
		value = wrapped["results"]
	}
	values, _ := value.([]any)
	for _, raw := range values {
		item, _ := raw.(map[string]any)
		rawURL, _ := item["url"].(string)
		if rawURL == "" {
			continue
		}
		title, _ := item["title"].(string)
		appendSearchSource(parsed, rawURL, title, "web")
	}
}

func collectXSearchResults(parsed *parsedChat, value any) {
	if wrapped, _ := value.(map[string]any); wrapped != nil {
		value = wrapped["results"]
	}
	values, _ := value.([]any)
	for _, raw := range values {
		item, _ := raw.(map[string]any)
		username, _ := item["username"].(string)
		postID, _ := item["postId"].(string)
		if username == "" || postID == "" {
			continue
		}
		title, _ := item["text"].(string)
		rawURL := "https://x.com/" + url.PathEscape(username) + "/status/" + url.PathEscape(postID)
		appendSearchSource(parsed, rawURL, title, "x_post")
	}
}

func appendSearchSource(parsed *parsedChat, value, title, sourceType string) {
	value, valid := searchresult.NormalizeURL(value)
	if !valid {
		return
	}
	if parsed.sourceKeys == nil {
		parsed.sourceKeys = make(map[string]struct{})
	}
	if _, exists := parsed.sourceKeys[value]; exists {
		return
	}
	if len(parsed.SearchSources) >= searchresult.MaxResults {
		return
	}
	parsed.sourceKeys[value] = struct{}{}
	title = searchresult.NormalizeTitle(title, value)
	parsed.SearchSources = append(parsed.SearchSources, map[string]any{"url": value, "title": title, "type": sourceType})
}

func collectServerTool(parsed *parsedChat, response map[string]any) {
	if parsed.serverToolKeys == nil {
		parsed.serverToolKeys = make(map[string]struct{})
	}
	key := serverToolKey(response)
	if _, exists := parsed.serverToolKeys[key]; !exists {
		if len(parsed.serverToolKeys) >= maxTrackedServerTools {
			return
		}
		parsed.serverToolKeys[key] = struct{}{}
		parsed.ServerTools++
	}
	if webServerToolName(response) != "web_search" {
		return
	}
	if parsed.webSearchKeys == nil {
		parsed.webSearchKeys = make(map[string]struct{})
	}
	if _, exists := parsed.webSearchKeys[key]; exists || len(parsed.webSearchKeys) >= maxTrackedServerTools {
		return
	}
	parsed.webSearchKeys[key] = struct{}{}
	parsed.WebSearchTools++
}

func serverToolKey(response map[string]any) string {
	key := firstString(response, "rolloutId", "responseId", "toolUsageCardId")
	step, hasStep := numberAsInt(response["messageStepId"])
	if key != "" {
		if hasStep {
			key += fmt.Sprintf(":%d", step)
		}
		return key
	}
	if token, _ := response["token"].(string); token != "" {
		sum := sha256.Sum256([]byte(token))
		return "token:" + hex.EncodeToString(sum[:8])
	}
	if hasStep {
		return fmt.Sprintf("step:%d", step)
	}
	return firstString(response, "messageTag")
}

func webServerToolName(response map[string]any) string {
	if name := strings.ToLower(strings.TrimSpace(firstString(response, "toolName", "tool_name"))); name != "" {
		return name
	}
	if card, _ := response["toolUsageCard"].(map[string]any); card != nil {
		if name := strings.ToLower(strings.TrimSpace(firstString(card, "toolName", "tool_name", "name"))); name != "" {
			return name
		}
		for _, tool := range []struct {
			field string
			name  string
		}{
			{field: "webSearch", name: "web_search"},
			{field: "web_search", name: "web_search"},
			{field: "xSearch", name: "x_search"},
			{field: "x_search", name: "x_search"},
			{field: "browsePage", name: "browse_page"},
			{field: "browse_page", name: "browse_page"},
			{field: "searchImages", name: "search_images"},
			{field: "search_images", name: "search_images"},
			{field: "chatroomSend", name: "chatroom_send"},
			{field: "chatroom_send", name: "chatroom_send"},
		} {
			if card[tool.field] != nil {
				return tool.name
			}
		}
	}
	token, _ := response["token"].(string)
	match := grokToolNamePattern.FindStringSubmatch(token)
	if len(match) < 2 {
		return ""
	}
	name := strings.TrimSpace(match[1])
	name = strings.TrimPrefix(name, "<![CDATA[")
	name = strings.TrimSuffix(name, "]]>")
	return strings.ToLower(strings.TrimSpace(name))
}

func applyParsedToolCalls(parsed *parsedChat, configuration toolConfiguration) {
	if len(configuration.Functions) == 0 || configuration.Choice == "none" {
		return
	}
	result := parseToolCalls(parsed.Text.String(), configuration.available)
	if len(result.Calls) == 0 {
		return
	}
	cleaned := removeToolSyntax(parsed.Text.String(), result)
	parsed.resetText(cleaned)
	parsed.ToolCalls = result.Calls
}
