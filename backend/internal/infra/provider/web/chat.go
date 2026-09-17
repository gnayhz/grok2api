package web

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	dialect "github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/searchresult"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonvalue"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/pkg/upstreamtrace"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

const maxDeferredSearchTextBytes = 8 << 20

const maxTrackedServerTools = 1024

const (
	maxTrackedCitationSources = 256
	maxTrackedAnnotations     = 2048
)

var (
	errWebAntiBot    = errors.New("Grok Web anti-bot rejection")
	errWebUsageLimit = errors.New("Grok Web usage limit reached")
)

var (
	grokRenderPattern   = regexp.MustCompile(`(?s)<grok:render\s+card_id="([^"]+)"\s+card_type="([^"]+)"\s+type="([^"]+)"[^>]*>.*?</grok:render>`)
	grokToolNamePattern = regexp.MustCompile(`(?is)<xai:tool_name>\s*(.*?)\s*</xai:tool_name>`)
)

type openAIRequest struct {
	Model              string          `json:"model"`
	Stream             bool            `json:"stream"`
	Stop               json.RawMessage `json:"stop"`
	Input              json.RawMessage `json:"input"`
	Instructions       string          `json:"instructions"`
	PreviousResponseID string          `json:"previous_response_id"`
	Store              *bool           `json:"store"`
	Messages           []chatMessage   `json:"messages"`
	// Include is the xAI/OpenAI Responses include list (inline_citations / no_inline_citations).
	Include           []string         `json:"include"`
	Tools             json.RawMessage  `json:"tools"`
	ToolChoice        json.RawMessage  `json:"tool_choice"`
	WebSearchOptions  json.RawMessage  `json:"web_search_options"`
	ParallelToolCalls *bool            `json:"parallel_tool_calls"`
	ImageConfig       *imageChatConfig `json:"image_config"`
}

type imageChatConfig struct {
	Count          *int   `json:"n"`
	ResponseFormat string `json:"response_format"`
	AspectRatio    string `json:"aspect_ratio"`
	Resolution     string `json:"resolution"`
}

type chatMessage struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
	CallID     string          `json:"call_id"`
	Name       string          `json:"name"`
	Arguments  string          `json:"arguments"`
	Output     json.RawMessage `json:"output"`
}

type normalizedChatInput struct {
	Prompt      string
	Attachments []chatAttachmentInput
}

type chatAttachmentInput struct {
	Source   string
	Filename string
	Image    bool
}

// hostedSearchCall tracks one mgw web_search / x_search invocation for xAI Responses output items.
type hostedSearchCall struct {
	ID      string
	Kind    string // web_search | x_search
	Query   string
	Status  string // in_progress | completed
	Sources []map[string]any
}

// trackedTextBuilder keeps OpenAI citation offsets in Unicode characters
// without rescanning the complete response for every citation.
type trackedTextBuilder struct {
	builder    strings.Builder
	characters int
}

func (b *trackedTextBuilder) WriteString(value string) (int, error) {
	written, err := b.builder.WriteString(value)
	b.characters += utf8.RuneCountInString(value[:written])
	return written, err
}

func (b *trackedTextBuilder) Reset() {
	b.builder.Reset()
	b.characters = 0
}

func (b *trackedTextBuilder) String() string { return b.builder.String() }

func (b *trackedTextBuilder) Len() int { return b.builder.Len() }

func (b *trackedTextBuilder) CharacterLen() int { return b.characters }

type parsedChat struct {
	GenerationOutcome string
	resources         *webResponseResources
	ResponseID        string
	ConversationID    string
	ParentID          string
	Text              trackedTextBuilder
	upstreamText      strings.Builder
	Reasoning         strings.Builder
	Images            []string
	SearchSources     []map[string]any
	Annotations       []map[string]any
	// ResponseOutput is populated by the Responses streaming state machine so
	// response.completed reuses the exact item IDs and ordering emitted in SSE.
	ResponseOutput []any
	// HostedSearchCalls are ordered web_search_call / x_search_call items (xAI Responses).
	HostedSearchCalls []hostedSearchCall
	hostedSearchByID  map[string]int
	// InlineCitations mirrors xAI default-on [[N]](url) embedding.
	DisableInlineCitations bool
	sourceKeys             map[string]struct{}
	serverToolKeys         map[string]struct{}
	webSearchKeys          map[string]struct{}
	xSearchKeys            map[string]struct{}
	cardCache              map[string]map[string]any
	moderatedImages        map[string]struct{}
	citationIndex          map[string]int
	lastCitation           int
	ServerTools            int64
	WebSearchTools         int64
	XSearchTools           int64
	InputTokens            int64
	ToolCalls              []parsedToolCall
	Tools                  []any
	ToolChoice             any
	ParallelTools          bool
}

func (p *parsedChat) textCharacterLen() int {
	if p == nil {
		return 0
	}
	return p.Text.CharacterLen()
}

func (p *parsedChat) appendText(value string) {
	if p == nil || value == "" {
		return
	}
	_, _ = p.Text.WriteString(value)
}

func (p *parsedChat) resetText(value string) {
	if p == nil {
		return
	}
	p.Text.Reset()
	_, _ = p.Text.WriteString(value)
}

func (a *Adapter) ForwardResponse(ctx context.Context, request provider.ResponseResourceRequest) (result *provider.Response, resultErr error) {
	ctx, finishTrace := upstreamtrace.Network(ctx, "web", request.Operation)
	defer finishTrace()
	var physicalAttempt attemptmeta.Identity
	defer func() {
		if result != nil && result.Attempt.ID == "" {
			result.Attempt = physicalAttempt
		}
	}()
	if request.Method == http.MethodGet || request.Method == http.MethodDelete {
		return a.handleResponseResource(ctx, request)
	}
	if request.Path == "/responses/compact" {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
			"type": "invalid_request_error", "code": "unsupported_operation",
			"message": "Grok Web 模型不支持 /responses/compact",
		}}), nil
	}
	if request.Method != http.MethodPost {
		return jsonProviderResponse(http.StatusMethodNotAllowed, map[string]any{"error": map[string]any{"message": "method not allowed"}}), nil
	}
	var conversationOptions conversation.ResponseOptions
	var imageOptions struct {
		ImageConfig *imageChatConfig `json:"image_config"`
	}
	if request.Operation == conversation.OperationMessages {
		// Preserve the Web image extension around the shared Messages conversion.
		if err := json.Unmarshal(request.Body, &imageOptions); err != nil {
			return invalidWebToolRequest(request.Operation, err), nil
		}
		converted, options, err := conversation.ConvertRequestWithOptions(request.Body, request.Model, request.Operation)
		if err != nil {
			return invalidWebToolRequest(request.Operation, err), nil
		}
		request.Body = converted
		conversationOptions = options
	}

	var input openAIRequest
	if err := json.Unmarshal(request.Body, &input); err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "请求 JSON 无效", "type": "invalid_request_error"}}), nil
	}
	conversationOptions.Store = input.Store
	if request.Operation == conversation.OperationMessages {
		input.ImageConfig = imageOptions.ImageConfig
	}
	if request.Operation == conversation.OperationChat {
		stop, err := conversation.ParseChatStopSequences(input.Stop)
		if err != nil {
			return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error", "param": "stop"}}), nil
		}
		conversationOptions.StopSequences = stop
	}
	if len(input.Include) > 0 {
		conversationOptions.Include = append([]string{}, input.Include...)
	}
	spec, modelKnown := Resolve(request.Model)
	var normalized normalizedChatInput
	var err error
	if modelKnown && spec.Capability == modeldomain.CapabilityImage {
		normalized, err = normalizeLatestImageInput(input, request.Operation)
	} else {
		normalized, err = normalizeOpenAIInput(input, request.Operation)
	}
	if err != nil {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": err.Error(), "type": "invalid_request_error"}}), nil
	}
	toolDeclarations, err := mergeWebSearchOptions(input.Tools, input.WebSearchOptions)
	if err != nil {
		return invalidWebToolRequest(request.Operation, err), nil
	}
	tools, err := parseToolConfiguration(toolDeclarations, input.ToolChoice)
	if err != nil {
		return invalidWebToolRequest(request.Operation, err), nil
	}
	parallelTools := true
	if input.ParallelToolCalls != nil {
		parallelTools = *input.ParallelToolCalls
	}
	if modelKnown && spec.Capability == modeldomain.CapabilityImage {
		if len(tools.ResponseTools) > 0 {
			return invalidImageRequest("图片生成模型不支持 tools")
		}
		return a.forwardImageChatCompletion(ctx, request, input, normalized, spec)
	}
	if modelKnown && spec.Capability == modeldomain.CapabilityImageEdit {
		return invalidImageRequest("图片编辑模型请使用 /v1/images/edits，并在当前请求中显式提供输入图片")
	}
	if !modelKnown || spec.Capability != modeldomain.CapabilityChat {
		return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "模型不支持文本对话", "type": "invalid_request_error"}}), nil
	}

	normalized.Prompt = injectToolPrompt(normalized.Prompt, tools)
	responseID := newWebID("resp")
	streaming := input.Stream || request.Streaming
	var parsed parsedChat
	var previous *inferencedomain.WebResponseState
	resources := newWebResponseResources(ctx)
	defer resources.Close()
	for attempt := 0; attempt < 2; attempt++ {
		attemptCtx := ctx
		if attempt > 0 {
			attemptCtx = infraegress.WithPhysicalCallStage(ctx, "anti_bot_retry")
		}
		upstream, lease, currentPrevious, statsigTarget, openErr := a.openChat(attemptCtx, request.Credential, input.PreviousResponseID, spec, normalized, gatewayOpenOptions{enforceStreamIdle: true})
		if openErr != nil {
			if errors.Is(openErr, errInvalidChatAttachment) || errors.Is(openErr, errInvalidChatImage) || errors.Is(openErr, errInvalidChatFile) {
				code := "invalid_attachment_input"
				if errors.Is(openErr, errInvalidChatImage) {
					code = "invalid_image_input"
				}
				return jsonProviderResponse(http.StatusBadRequest, map[string]any{"error": map[string]any{
					"message": openErr.Error(), "type": "invalid_request_error", "code": code,
				}}), nil
			}
			return nil, openErr
		}
		previous = currentPrevious
		physicalAttempt = attemptmeta.FromResponse(upstream)
		if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
			if upstream.StatusCode == http.StatusForbidden {
				// Preserve definitive account-block signals before a Statsig retry can discard the first response.
				body, readErr := io.ReadAll(io.LimitReader(upstream.Body, 4<<20))
				_ = upstream.Body.Close()
				if readErr != nil {
					lease.Release()
					return nil, readErr
				}
				if dialect.IsDefinitiveAccountBlockBody(body) {
					return &provider.Response{
						StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
						UpstreamURL: responseUpstreamURL(upstream),
						Body: &releaseBody{ReadCloser: io.NopCloser(bytes.NewReader(body)), release: func() {
							lease.Release()
						}},
					}, nil
				}
				lease.InvalidateClearance()
				if statsigTarget != "" && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
					lease.Release()
					continue
				}
				return &provider.Response{
					StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
					UpstreamURL: responseUpstreamURL(upstream),
					Body: &releaseBody{ReadCloser: io.NopCloser(bytes.NewReader(body)), release: func() {
						lease.Observe(upstream.StatusCode, nil)
						lease.Release()
					}},
				}, nil
			}
			return &provider.Response{
				StatusCode: upstream.StatusCode, Status: upstream.Status, Header: http.Header(upstream.Header),
				UpstreamURL: responseUpstreamURL(upstream),
				Body: &releaseBody{ReadCloser: upstream.Body, release: func() {
					lease.Observe(upstream.StatusCode, nil)
					lease.Release()
				}},
			}, nil
		}

		if traceDir, enabled := upstreamtrace.Enabled(); enabled {
			traceBody, _ := json.Marshal(map[string]any{"model": request.Model, "prompt": normalized.Prompt})
			upstreamtrace.DumpRequest(traceDir, "web_"+request.Operation, request.Model, streaming, traceBody)
			upstream.Body = upstreamtrace.TeeStream(traceDir, "web_"+request.Operation, request.Model, upstream.Body)
		}
		if streaming {
			prepared, preflightErr := preflightUpstream(upstream.Body)
			if preflightErr == nil {
				pending := a.pendingResponseState(ctx, request.Operation, input.Store)
				body := a.streamOpenAIResponse(ctx, prepared, lease, request.Credential, responseID, input.Model, request.Operation, normalized.Prompt, previous, tools, parallelTools, conversationOptions, pending, physicalAttempt.ID)
				return pending.attach(&provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: streamHeaders(), Body: body}), nil
			}
			if statsigTarget != "" && errors.Is(preflightErr, errWebAntiBot) && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
				a.releaseStatsigRetry(upstream, lease)
				continue
			}
			_ = upstream.Body.Close()
			lease.Release()
			if errors.Is(preflightErr, errWebAntiBot) {
				a.feedbackAntiBot(ctx, lease, statsigTarget)
				return antiBotProviderResponse(), nil
			}
			return nil, preflightErr
		}

		currentParsed := parsedChat{DisableInlineCitations: !conversationOptions.InlineCitationsEnabled(), resources: resources}
		consumeErr := consumeUpstreamInto(upstream.Body, &currentParsed, nil)
		currentParsed.InputTokens = estimateTokens(normalized.Prompt)
		observeWebGeneration(ctx, physicalAttempt.ID, &currentParsed)
		_ = upstream.Body.Close()
		if statsigTarget != "" && errors.Is(consumeErr, errWebAntiBot) && attempt == 0 && a.invalidateSignedStatsig(http.MethodPost, statsigTarget) {
			resources.Close()
			lease.Release()
			continue
		}
		lease.Release()
		if consumeErr != nil {
			if errors.Is(consumeErr, errWebAntiBot) {
				a.feedbackAntiBot(ctx, lease, statsigTarget)
				return antiBotProviderResponse(), nil
			}
			if errors.Is(consumeErr, errWebUsageLimit) {
				// usage-limit/封禁信号是上游配额状态, 与传输错误不同:以 429 语义
				// 返回, 让网关走限流处理(账号冷却/换号)而不是当作网络故障重试。
				// 与 lite-image 路径(image.go)对齐; 此前哨兵在 chat 主链路从不被
				// 检查, 流内 usage limit 以裸错误终止。
				lease.Observe(http.StatusTooManyRequests, nil)
				return usageLimitProviderResponse(), nil
			}
			lease.Observe(0, consumeErr)
			return nil, consumeErr
		}
		lease.Observe(http.StatusOK, nil)
		parsed = currentParsed
		break
	}
	parsed.InputTokens = estimateTokens(normalized.Prompt)
	parsed.Tools = tools.ResponseTools
	parsed.ToolChoice = tools.ResponseChoice
	parsed.ParallelTools = parallelTools
	applyParsedToolCalls(&parsed, tools)
	if err := checkToolChoice(&parsed, tools); err != nil {
		return nil, err
	}
	if err := checkChatOutput(&parsed); err != nil {
		return nil, err
	}
	if err := a.archiveChatImages(ctx, request.Credential, &parsed); err != nil {
		return nil, err
	}
	payload := buildOpenAIResult(request.Operation, responseID, input.Model, parsed, false, conversationOptions)
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	buffered := responsebuffer.New(resources.budget, responsebuffer.JSONLimit)
	if _, err := buffered.Write(data); err != nil {
		_ = buffered.Close()
		return nil, err
	}
	pending := a.pendingResponseState(ctx, request.Operation, input.Store)
	if err := pending.prepare(request.Credential.ID, responseID, parsed, data); err != nil {
		_ = buffered.Close()
		pending.discard()
		return nil, err
	}
	return pending.attach(&provider.Response{StatusCode: http.StatusOK, Status: "200 OK", Header: jsonHeaders(), Body: buffered.Body()}), nil
}

func (a *Adapter) releaseStatsigRetry(upstream *http.Response, lease *infraegress.Lease) {
	_ = upstream.Body.Close()
	lease.Release()
}

func (a *Adapter) feedbackAntiBot(ctx context.Context, lease *infraegress.Lease, statsigTarget string) {
	if statsigTarget != "" {
		a.invalidateSignedStatsig(http.MethodPost, statsigTarget)
	}
	lease.Observe(http.StatusForbidden, nil)
}

func preflightUpstream(source io.ReadCloser) (io.ReadCloser, error) {
	reader := bufio.NewReaderSize(source, 64<<10)
	var prefetched bytes.Buffer
	for prefetched.Len() <= 1<<20 {
		line, err := reader.ReadString('\n')
		if line != "" {
			prefetched.WriteString(line)
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "data:") {
				trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			}
			if strings.HasPrefix(trimmed, "{") {
				var root map[string]any
				if json.Unmarshal([]byte(trimmed), &root) == nil {
					if errorValue, ok := root["error"].(map[string]any); ok {
						return nil, webResponseError(errorValue)
					}
					if event, ok := root["event"].(map[string]any); ok {
						if event["type"] == "error" {
							return nil, gatewayEventError(event)
						}
						return &readerCloser{Reader: io.MultiReader(bytes.NewReader(prefetched.Bytes()), reader), closer: source}, nil
					}
					if result, ok := root["result"].(map[string]any); ok && (result["conversation"] != nil || result["response"] != nil) {
						return &readerCloser{Reader: io.MultiReader(bytes.NewReader(prefetched.Bytes()), reader), closer: source}, nil
					}
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && prefetched.Len() > 0 {
				return &readerCloser{Reader: bytes.NewReader(prefetched.Bytes()), closer: source}, nil
			}
			return nil, err
		}
	}
	return nil, fmt.Errorf("Grok Web 首个流事件超过安全检查上限")
}

func checkChatOutput(parsed *parsedChat) error {
	if strings.TrimSpace(parsed.Text.String()) == "" && len(parsed.ToolCalls) == 0 && len(parsed.Images) == 0 && len(parsed.HostedSearchCalls) == 0 {
		return responsecheck.ErrEmptyOutput
	}
	return nil
}

func (a *Adapter) archiveChatImages(ctx context.Context, credential account.Credential, parsed *parsedChat) error {
	for _, rawURL := range parsed.Images {
		item, err := a.imageDataItem(ctx, credential, imagineImageValue{URL: rawURL}, "url")
		if err != nil {
			return err
		}
		if parsed.Text.Len() > 0 {
			parsed.appendText("\n\n")
		}
		parsed.appendText(liteImageMarkdown(item))
	}
	return nil
}

func appendUniqueString(values []string, value string) []string {
	if containsString(values, value) {
		return values
	}
	return append(values, value)
}

func containsString(values []string, value string) bool {
	for _, existing := range values {
		if existing == value {
			return true
		}
	}
	return false
}

func collectCardAttachment(parsed *parsedChat, value any) string {
	if values, ok := value.([]any); ok {
		first := ""
		for _, item := range values {
			if rawURL := collectCardAttachment(parsed, item); first == "" && rawURL != "" {
				first = rawURL
			}
		}
		return first
	}
	data := cardAttachmentData(value)
	if data == nil {
		return ""
	}
	if id, _ := data["id"].(string); id != "" {
		if parsed.cardCache == nil {
			parsed.cardCache = make(map[string]map[string]any)
		}
		parsed.cardCache[id] = data
	}
	return imageURLFromCardData(data)
}

func cardAttachmentData(value any) map[string]any {
	card, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if raw, ok := card["jsonData"].(map[string]any); ok {
		return raw
	}
	if raw, _ := card["jsonData"].(string); raw != "" {
		var data map[string]any
		if json.Unmarshal([]byte(raw), &data) == nil {
			return data
		}
	}
	if card["image_chunk"] != nil || card["imageChunk"] != nil {
		return card
	}
	return nil
}

func imageURLFromCardData(data map[string]any) string {
	chunk, _ := data["image_chunk"].(map[string]any)
	if chunk == nil {
		chunk, _ = data["imageChunk"].(map[string]any)
	}
	if chunk == nil {
		return ""
	}
	moderated, _ := chunk["moderated"].(bool)
	progress, _ := numberAsInt(chunk["progress"])
	if moderated || progress < 100 {
		return ""
	}
	imageURL, _ := chunk["imageUrl"].(string)
	if imageURL == "" {
		imageURL, _ = chunk["image_url"].(string)
	}
	return imageURL
}

func cleanChatToken(parsed *parsedChat, token string) string {
	if !strings.Contains(token, "<grok:render") {
		if token != "" {
			// Visible assistant text separates two citations. Only truly adjacent
			// duplicate render frames should be collapsed.
			parsed.lastCitation = 0
		}
		return token
	}
	matches := grokRenderPattern.FindAllStringSubmatchIndex(token, -1)
	if len(matches) == 0 {
		return token
	}
	var builder strings.Builder
	builderCharacters := 0
	cursor := 0
	for _, match := range matches {
		prefix := token[cursor:match[0]]
		builder.WriteString(prefix)
		builderCharacters += utf8.RuneCountInString(prefix)
		if prefix != "" {
			parsed.lastCitation = 0
		}
		cardID := token[match[2]:match[3]]
		renderType := token[match[6]:match[7]]
		replacement, annotation := renderChatCard(parsed, cardID, renderType)
		if annotation != nil {
			if replacement != "" {
				start := parsed.textCharacterLen() + builderCharacters
				annotation["start_index"] = start
				annotation["end_index"] = start + utf8.RuneCountInString(replacement)
			}
			parsed.Annotations = append(parsed.Annotations, annotation)
		}
		builder.WriteString(replacement)
		builderCharacters += utf8.RuneCountInString(replacement)
		cursor = match[1]
	}
	suffix := token[cursor:]
	builder.WriteString(suffix)
	if suffix != "" {
		parsed.lastCitation = 0
	}
	return builder.String()
}

func renderChatCard(parsed *parsedChat, cardID, renderType string) (string, map[string]any) {
	if parsed.cardCache == nil {
		return "", nil
	}
	card := parsed.cardCache[cardID]
	if card == nil {
		return "", nil
	}
	switch renderType {
	case "render_generated_image", "render_file":
		return "", nil
	case "render_searched_image":
		image, _ := card["image"].(map[string]any)
		if image == nil {
			return "", nil
		}
		title, _ := image["title"].(string)
		thumbnail := firstString(image, "thumbnail", "original")
		link, _ := image["link"].(string)
		if thumbnail == "" {
			return "", nil
		}
		if title == "" {
			title = "image"
		}
		if link != "" {
			return fmt.Sprintf("[![%s](%s)](%s)", title, thumbnail, link), nil
		}
		return fmt.Sprintf("![%s](%s)", title, thumbnail), nil
	case "render_inline_citation":
		value, _ := card["url"].(string)
		value, valid := searchresult.NormalizeURL(value)
		if !valid {
			return "", nil
		}
		if parsed.citationIndex == nil {
			parsed.citationIndex = make(map[string]int)
		}
		index, exists := parsed.citationIndex[value]
		if !exists {
			if len(parsed.citationIndex) >= maxTrackedCitationSources {
				return "", nil
			}
			index = len(parsed.citationIndex) + 1
			parsed.citationIndex[value] = index
		}
		if parsed.lastCitation == index {
			return "", nil
		}
		parsed.lastCitation = index
		annotation := citationAnnotation(parsed, value, index)
		if parsed.DisableInlineCitations {
			if len(parsed.Annotations) >= maxTrackedAnnotations {
				return "", nil
			}
			return "", annotation
		}
		// Inline marker keeps the numeric label. When the structured annotation
		// cap is reached, keep the visible text without growing retained state.
		replacement := fmt.Sprintf("[[%d]](%s)", index, value)
		if len(parsed.Annotations) >= maxTrackedAnnotations {
			return replacement, nil
		}
		return replacement, annotation
	default:
		return "", nil
	}
}

// citationAnnotation keeps the xAI citation label in title while retaining the
// page title separately for the OpenAI Chat Completions nested citation shape.
func citationAnnotation(parsed *parsedChat, rawURL string, index int) map[string]any {
	if index < 1 {
		index = 1
	}
	annotation := map[string]any{
		"type": "url_citation", "url": rawURL, "title": fmt.Sprintf("%d", index),
	}
	if parsed != nil {
		if title := lookupSourcePageTitle(parsed.SearchSources, rawURL); title != "" {
			annotation["source_title"] = title
			return annotation
		}
		for _, call := range parsed.HostedSearchCalls {
			if title := lookupSourcePageTitle(call.Sources, rawURL); title != "" {
				annotation["source_title"] = title
				return annotation
			}
		}
	}
	return annotation
}

// lookupSourcePageTitle returns a non-empty page title for rawURL, or "" if
// unknown. It never falls back to the URL itself (callers decide fallback).
func lookupSourcePageTitle(sources []map[string]any, rawURL string) string {
	if normalized, valid := searchresult.NormalizeURL(rawURL); valid {
		rawURL = normalized
	}
	for _, source := range sources {
		if value, _ := source["url"].(string); value == rawURL {
			if title, _ := source["title"].(string); title != "" {
				return title
			}
			return ""
		}
	}
	return ""
}

func upsertHostedSearchCall(parsed *parsedChat, id, kind, query, status string) *hostedSearchCall {
	if parsed == nil {
		return nil
	}
	if id == "" {
		// Some mgw tool_result frames omit tool_call_id. Correlate only when
		// exactly one unfinished call of the same kind exists; otherwise keep
		// the result separate instead of attaching it to the wrong invocation.
		matchedID := ""
		for i := range parsed.HostedSearchCalls {
			call := &parsed.HostedSearchCalls[i]
			if call.Kind != kind || call.Status == "completed" {
				continue
			}
			if matchedID != "" {
				matchedID = ""
				break
			}
			matchedID = call.ID
		}
		if matchedID != "" {
			id = matchedID
		} else {
			id = fmt.Sprintf("%s_%d", kind, len(parsed.HostedSearchCalls)+1)
		}
	}
	if parsed.hostedSearchByID == nil {
		parsed.hostedSearchByID = make(map[string]int)
	}
	if idx, ok := parsed.hostedSearchByID[id]; ok {
		call := &parsed.HostedSearchCalls[idx]
		if query != "" {
			call.Query = query
		}
		if status != "" {
			call.Status = status
		}
		if kind != "" && call.Kind == "" {
			call.Kind = kind
		}
		return call
	}
	if len(parsed.HostedSearchCalls) >= maxTrackedServerTools {
		return nil
	}
	if status == "" {
		status = "in_progress"
	}
	parsed.HostedSearchCalls = append(parsed.HostedSearchCalls, hostedSearchCall{
		ID: id, Kind: kind, Query: query, Status: status,
	})
	parsed.hostedSearchByID[id] = len(parsed.HostedSearchCalls) - 1
	return &parsed.HostedSearchCalls[len(parsed.HostedSearchCalls)-1]
}

func appendHostedSearchSources(call *hostedSearchCall, sources []map[string]any) {
	if call == nil || len(sources) == 0 {
		return
	}
	seen := make(map[string]struct{}, len(call.Sources)+len(sources))
	for _, existing := range call.Sources {
		if u, _ := existing["url"].(string); u != "" {
			seen[u] = struct{}{}
		}
	}
	for _, source := range sources {
		u, _ := source["url"].(string)
		if u == "" {
			continue
		}
		if _, ok := seen[u]; ok {
			continue
		}
		if len(call.Sources) >= searchresult.MaxResults {
			break
		}
		seen[u] = struct{}{}
		// Normalize early so in-memory shape matches wire (type url + optional title).
		item := map[string]any{"type": "url", "url": u}
		if title, _ := source["title"].(string); title != "" {
			item["title"] = title
		}
		call.Sources = append(call.Sources, item)
	}
}

// hostedSearchActionSources copies sources for web_search_call/x_search_call action.
// Always sets type:"url" (OpenAPI required); preserves title when present.
func hostedSearchActionSources(sources []map[string]any) []map[string]any {
	if len(sources) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(sources))
	for _, source := range sources {
		u, _ := source["url"].(string)
		if u == "" {
			continue
		}
		item := map[string]any{"type": "url", "url": u}
		if title, _ := source["title"].(string); title != "" {
			item["title"] = title
		}
		out = append(out, item)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// xaiHostedSearchOutputItems builds Responses output items: web_search_call / x_search_call.
func xaiHostedSearchOutputItems(parsed parsedChat) []any {
	if len(parsed.HostedSearchCalls) == 0 {
		return nil
	}
	items := make([]any, 0, len(parsed.HostedSearchCalls))
	for _, call := range parsed.HostedSearchCalls {
		if call.Status != "completed" && len(call.Sources) == 0 {
			continue
		}
		typeName := "web_search_call"
		if call.Kind == "x_search" {
			typeName = "x_search_call"
		}
		status := call.Status
		if status == "" {
			status = "completed"
		}
		action := map[string]any{"type": "search"}
		if call.Query != "" {
			action["query"] = call.Query
		}
		if len(call.Sources) > 0 {
			// OpenAPI: sources[] requires type:"url"+url; title kept as optional extension.
			action["sources"] = hostedSearchActionSources(call.Sources)
		}
		items = append(items, map[string]any{
			"id": call.ID, "type": typeName, "status": status, "action": action,
		})
	}
	return items
}

func xaiServerSideToolUsage(parsed parsedChat) map[string]any {
	var web, x int64
	for _, call := range parsed.HostedSearchCalls {
		// Billable/successful executions: completed or with returned sources.
		if call.Status != "completed" && len(call.Sources) == 0 {
			continue
		}
		switch call.Kind {
		case "web_search":
			web++
		case "x_search":
			x++
		}
	}
	// Legacy Web frames do not expose hosted call completion records, so their
	// deduplicated tool counters remain the only available signal. For mgw,
	// never turn an in_progress/failed attempt into successful billable usage.
	if len(parsed.HostedSearchCalls) == 0 {
		web = parsed.WebSearchTools
		x = parsed.XSearchTools
	}
	usage := map[string]any{}
	if web > 0 {
		usage["SERVER_SIDE_TOOL_WEB_SEARCH"] = web
	}
	if x > 0 {
		usage["SERVER_SIDE_TOOL_X_SEARCH"] = x
	}
	if len(usage) == 0 {
		return nil
	}
	return usage
}

func buildOpenAIResult(operation, responseID, model string, parsed parsedChat, streaming bool, responseOptions ...conversation.ResponseOptions) map[string]any {
	created := time.Now().Unix()
	options := conversation.ResponseOptions{}
	if len(responseOptions) > 0 {
		options = responseOptions[0]
	}
	usage := webGenerationUsage(&parsed)
	inputTokens, outputTokens := usage.Input, usage.Output
	if operation == "chat" {
		finalizeXAIAnnotations(&parsed)
		visibleText, _ := applyWebStopSequences(parsed.Text.String(), options.StopSequences)
		message := map[string]any{"role": "assistant", "content": visibleText, "reasoning_content": parsed.Reasoning.String()}
		if len(parsed.Annotations) > 0 {
			message["annotations"] = chatAnnotations(parsed.Annotations)
		}
		finishReason := "stop"
		if len(parsed.ToolCalls) > 0 {
			finishReason = "tool_calls"
			if parsed.Text.Len() == 0 {
				message["content"] = nil
			}
			message["tool_calls"] = chatToolCalls(parsed.ToolCalls)
		}
		value := map[string]any{
			"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion", "created": created, "model": model,
			"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": finishReason}},
			"usage":   map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens, "total_tokens": inputTokens + outputTokens},
		}
		// xAI: top-level citations = all source URLs encountered (always when present).
		if citations := xaiCitationURLs(parsed); len(citations) > 0 {
			value["citations"] = citations
		}
		if usage := xaiServerSideToolUsage(parsed); usage != nil {
			value["server_side_tool_usage"] = usage
		}
		return value
	}
	if operation == conversation.OperationMessages {
		visibleText, stopSequence := applyWebStopSequences(parsed.Text.String(), options.StopSequences)
		emitWebSearch := shouldEmitWebMessagesSearch(parsed, options)
		// Zero initial capacity: tool/search counts come from untrusted upstream.
		content := make([]any, 0)
		if options.AnthropicThinking && parsed.Reasoning.Len() > 0 {
			content = append(content, map[string]any{"type": "thinking", "thinking": parsed.Reasoning.String()})
		}
		if emitWebSearch {
			content = append(content, webMessagesSearchBlocks(newWebID("srvtoolu"), parsed, options)...)
		}
		if visibleText != "" || len(parsed.ToolCalls) == 0 {
			content = append(content, map[string]any{"type": "text", "text": visibleText})
		}
		for _, call := range parsed.ToolCalls {
			var input any = map[string]any{}
			if jsonvalue.Unmarshal([]byte(call.Arguments), &input) != nil {
				input = map[string]any{}
			}
			content = append(content, map[string]any{"type": "tool_use", "id": webAnthropicToolID(call.ID), "name": call.Name, "input": input})
		}
		stopReason := "end_turn"
		if len(parsed.ToolCalls) > 0 {
			stopReason = "tool_use"
		} else if stopSequence != "" {
			stopReason = "stop_sequence"
		}
		usage := map[string]any{"input_tokens": inputTokens, "output_tokens": outputTokens, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0}
		if emitWebSearch {
			usage["server_tool_use"] = map[string]any{"web_search_requests": webMessagesSearchRequests(parsed)}
		}
		return map[string]any{
			"id": strings.Replace(responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
			"content": content, "stop_reason": stopReason, "stop_sequence": nullableWebString(stopSequence),
			"usage": usage,
		}
	}
	finalizeXAIAnnotations(&parsed)
	output := parsed.ResponseOutput
	if output == nil {
		output = make([]any, 0, 2)
		if parsed.Reasoning.Len() > 0 {
			output = append(output, map[string]any{"id": newWebID("rs"), "type": "reasoning", "status": "completed", "summary": []any{map[string]any{"type": "summary_text", "text": parsed.Reasoning.String()}}})
		}
		// xAI Tool Usage Details: web_search_call / x_search_call precede the assistant message.
		output = append(output, xaiHostedSearchOutputItems(parsed)...)
		if parsed.Text.Len() > 0 || len(parsed.ToolCalls) == 0 {
			annotations := responsesAnnotations(parsed.Annotations)
			if annotations == nil {
				annotations = []any{}
			}
			message := map[string]any{"id": newWebID("msg"), "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": parsed.Text.String(), "annotations": annotations, "logprobs": []any{}}}}
			output = append(output, message)
		}
		for _, call := range parsed.ToolCalls {
			output = append(output, map[string]any{
				"id": newWebID("fc"), "type": "function_call", "status": "completed",
				"call_id": call.ID, "name": call.Name, "arguments": call.Arguments,
			})
		}
	}
	tools := parsed.Tools
	if tools == nil {
		tools = []any{}
	}
	toolChoice := parsed.ToolChoice
	if toolChoice == nil {
		toolChoice = "auto"
	}
	value := map[string]any{
		"id": responseID, "object": "response", "created_at": created, "completed_at": created, "status": "completed", "model": model,
		"output": output, "parallel_tool_calls": parsed.ParallelTools, "tools": tools, "tool_choice": toolChoice, "store": historydomain.ResponseStorageRequested(options.Store),
		"usage": map[string]any{
			"input_tokens": inputTokens, "output_tokens": outputTokens, "total_tokens": inputTokens + outputTokens,
			"input_tokens_details":  map[string]any{"cached_tokens": 0},
			"output_tokens_details": map[string]any{"reasoning_tokens": estimateTokens(parsed.Reasoning.String())},
			"num_sources_used":      int64(len(parsed.SearchSources)), "num_server_side_tools_used": parsed.ServerTools,
		},
	}
	// xAI Citations docs: response.citations is the full URL list from tool research.
	if citations := xaiCitationURLs(parsed); len(citations) > 0 {
		value["citations"] = citations
	}
	if usage := xaiServerSideToolUsage(parsed); usage != nil {
		value["server_side_tool_usage"] = usage
	}
	return value
}

func applyWebStopSequences(text string, sequences []string) (string, string) {
	matchAt := -1
	matched := ""
	for _, sequence := range sequences {
		if sequence == "" {
			continue
		}
		if index := strings.Index(text, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt < 0 {
		return text, ""
	}
	return text[:matchAt], matched
}

func nullableWebString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func webAnthropicToolID(value string) string {
	if strings.HasPrefix(value, "toolu_") {
		return value
	}
	return "toolu_" + value
}

func webMessagesSearchBlocks(id string, parsed parsedChat, options conversation.ResponseOptions) []any {
	use := map[string]any{
		"type": "server_tool_use", "id": id, "name": "web_search",
		"input": map[string]any{"query": options.AnthropicWebSearchQuery},
	}
	hits := make([]any, 0, searchresult.MaxResults)
	seen := make(map[string]struct{}, searchresult.MaxResults)
	for _, source := range parsed.SearchSources {
		if len(hits) >= searchresult.MaxResults {
			break
		}
		rawURL, _ := source["url"].(string)
		rawURL, valid := searchresult.NormalizeURL(rawURL)
		if !valid {
			continue
		}
		if _, exists := seen[rawURL]; exists {
			continue
		}
		seen[rawURL] = struct{}{}
		title, _ := source["title"].(string)
		title = searchresult.NormalizeTitle(title, rawURL)
		hits = append(hits, map[string]any{"type": "web_search_result", "title": title, "url": rawURL})
	}
	var content any = hits
	if parsed.WebSearchTools == 0 && len(hits) == 0 {
		content = map[string]any{"type": "web_search_tool_result_error", "error_code": "unavailable"}
	}
	result := map[string]any{"type": "web_search_tool_result", "tool_use_id": id, "content": content}
	return []any{use, result}
}

func shouldEmitWebMessagesSearch(parsed parsedChat, options conversation.ResponseOptions) bool {
	return options.AnthropicWebSearch && (options.AnthropicWebSearchRequired || parsed.WebSearchTools > 0 || len(parsed.SearchSources) > 0)
}

func webMessagesSearchRequests(parsed parsedChat) int64 {
	if parsed.WebSearchTools > 0 {
		return parsed.WebSearchTools
	}
	return 1
}

func chatToolCalls(calls []parsedToolCall) []any {
	values := make([]any, 0, len(calls))
	for _, call := range calls {
		values = append(values, map[string]any{
			"id": call.ID, "type": "function",
			"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
		})
	}
	return values
}

func chatAnnotations(annotations []map[string]any) []any {
	values := make([]any, 0, len(annotations))
	for _, annotation := range annotations {
		values = append(values, chatURLCitation(annotation))
	}
	return values
}

// chatURLCitation is Chat Completions nested url_citation (OpenAI-compatible wire).
// OpenAI Chat Completions uses the page title; fall back to the numeric label.
func chatURLCitation(annotation map[string]any) map[string]any {
	title := annotation["title"]
	if sourceTitle, _ := annotation["source_title"].(string); sourceTitle != "" {
		title = sourceTitle
	}
	inner := map[string]any{
		"url": annotation["url"], "title": title,
	}
	if _, ok := annotation["start_index"]; ok {
		inner["start_index"] = annotation["start_index"]
		inner["end_index"] = annotation["end_index"]
	}
	return map[string]any{"type": "url_citation", "url_citation": inner}
}

// responsesURLCitation is the xAI/OpenAI Responses flat url_citation on output_text.
func responsesURLCitation(annotation map[string]any) map[string]any {
	out := map[string]any{
		"type":  "url_citation",
		"url":   annotation["url"],
		"title": annotation["title"],
	}
	if _, ok := annotation["start_index"]; ok {
		out["start_index"] = annotation["start_index"]
		out["end_index"] = annotation["end_index"]
	}
	return out
}

func responsesAnnotations(annotations []map[string]any) []any {
	values := make([]any, 0, len(annotations))
	for _, annotation := range annotations {
		values = append(values, responsesURLCitation(annotation))
	}
	return values
}

// xaiCitationURLs builds response.citations: all source URLs from search tool results.
func xaiCitationURLs(parsed parsedChat) []string {
	out := make([]string, 0, len(parsed.SearchSources)+len(parsed.Annotations))
	seen := make(map[string]struct{}, cap(out))
	appendURL := func(u string) {
		if u == "" {
			return
		}
		if _, exists := seen[u]; exists {
			return
		}
		seen[u] = struct{}{}
		out = append(out, u)
	}
	for _, source := range parsed.SearchSources {
		u, _ := source["url"].(string)
		appendURL(u)
	}
	// render_citation can arrive for a source missing from tool_result (for
	// example a truncated result pool); it must not disappear from citations.
	for _, annotation := range parsed.Annotations {
		u, _ := annotation["url"].(string)
		appendURL(u)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// finalizeXAIAnnotations ensures annotations exist for no_inline mode from search pool
// when render_citation did not emit per-hit markers.
func finalizeXAIAnnotations(parsed *parsedChat) {
	if parsed == nil {
		return
	}
	if !parsed.DisableInlineCitations {
		return
	}
	if len(parsed.Annotations) > 0 {
		// Strip any accidental positional fields.
		for _, ann := range parsed.Annotations {
			delete(ann, "start_index")
			delete(ann, "end_index")
		}
		return
	}
	if len(parsed.SearchSources) == 0 {
		return
	}
	n := 0
	for _, source := range parsed.SearchSources {
		u, _ := source["url"].(string)
		if u == "" {
			continue
		}
		n++
		annotation := map[string]any{
			"type":  "url_citation",
			"url":   u,
			"title": fmt.Sprintf("%d", n),
		}
		if sourceTitle, _ := source["title"].(string); sourceTitle != "" {
			annotation["source_title"] = sourceTitle
		}
		parsed.Annotations = append(parsed.Annotations, annotation)
	}
}

func estimateToolCallTokens(calls []parsedToolCall) int64 {
	var total int64
	for _, call := range calls {
		total += estimateTokens(call.Name) + estimateTokens(call.Arguments)
	}
	return total
}

type webMessagesStream struct {
	writer          io.Writer
	responseID      string
	model           string
	inputTokens     int64
	options         conversation.ResponseOptions
	started         bool
	thinkingStarted bool
	thinkingClosed  bool
	thinkingIndex   int
	textStarted     bool
	textClosed      bool
	textIndex       int
	nextIndex       int
	hasTools        bool
	webSearchID     string
	webSearchUse    bool
	pendingText     strings.Builder
	stopSequence    string
	stopFilter      *webStopFilter
}

func newWebMessagesStream(writer io.Writer, responseID, model string, inputTokens int64, options conversation.ResponseOptions) *webMessagesStream {
	return &webMessagesStream{
		writer: writer, responseID: responseID, model: model, inputTokens: inputTokens,
		options: options, stopFilter: newWebStopFilter(options.StopSequences),
	}
}

func (s *webMessagesStream) Start() error {
	if s.started {
		return nil
	}
	s.started = true
	if err := writeSSE(s.writer, "message_start", map[string]any{
		"type": "message_start", "message": map[string]any{
			"id": strings.Replace(s.responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": s.model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": s.inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
		},
	}); err != nil {
		return err
	}
	if s.options.AnthropicWebSearchRequired && !s.options.AnthropicThinking {
		return s.startWebSearch()
	}
	return nil
}

func (s *webMessagesStream) Delta(kind, delta string) error {
	if err := s.Start(); err != nil {
		return err
	}
	if s.stopSequence != "" {
		return nil
	}
	if kind == "reasoning" {
		if !s.options.AnthropicThinking {
			return nil
		}
		if !s.thinkingStarted {
			s.thinkingStarted = true
			s.thinkingIndex = s.nextIndex
			s.nextIndex++
			if err := writeSSE(s.writer, "content_block_start", map[string]any{
				"type": "content_block_start", "index": s.thinkingIndex,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return err
			}
		}
		return writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.thinkingIndex,
			"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
		})
	}
	if kind != "text" {
		return nil
	}
	if s.options.AnthropicWebSearch {
		if err := s.closeThinking(); err != nil {
			return err
		}
		if s.options.AnthropicWebSearchRequired {
			if err := s.startWebSearch(); err != nil {
				return err
			}
		}
		return s.bufferSearchText(delta)
	}
	if err := s.startText(); err != nil {
		return err
	}
	emit, matched := s.stopFilter.Push(delta)
	if matched != "" {
		s.stopSequence = matched
	}
	if emit == "" {
		return nil
	}
	return s.writeTextDelta(emit)
}

func (s *webMessagesStream) bufferSearchText(delta string) error {
	pending := s.pendingText.Len()
	if pending >= maxDeferredSearchTextBytes || len(delta) > maxDeferredSearchTextBytes-pending {
		return fmt.Errorf("WebSearch 延迟文本缓冲超过 %d MiB", maxDeferredSearchTextBytes>>20)
	}
	s.pendingText.WriteString(delta)
	return nil
}

func (s *webMessagesStream) startText() error {
	if s.textStarted && !s.textClosed {
		return nil
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	s.textStarted = true
	s.textClosed = false
	s.textIndex = s.nextIndex
	s.nextIndex++
	return writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": s.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

func (s *webMessagesStream) writeTextDelta(delta string) error {
	if delta == "" {
		return nil
	}
	return writeSSE(s.writer, "content_block_delta", map[string]any{
		"type": "content_block_delta", "index": s.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	})
}

func (s *webMessagesStream) Tools(calls []parsedToolCall) error {
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.closeText(); err != nil {
		return err
	}
	for _, call := range calls {
		index := s.nextIndex
		s.nextIndex++
		id := webAnthropicToolID(call.ID)
		if err := writeSSE(s.writer, "content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": call.Name, "input": map[string]any{}},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
		}); err != nil {
			return err
		}
		if err := writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
		s.hasTools = true
	}
	return nil
}

func (s *webMessagesStream) startWebSearch() error {
	if s.webSearchUse {
		return nil
	}
	s.webSearchUse = true
	if s.webSearchID == "" {
		s.webSearchID = newWebID("srvtoolu")
	}
	index := s.nextIndex
	s.nextIndex++
	if err := writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "server_tool_use", "id": s.webSearchID, "name": "web_search", "input": map[string]any{}},
	}); err != nil {
		return err
	}
	if query := s.options.AnthropicWebSearchQuery; query != "" {
		encoded, _ := json.Marshal(map[string]string{"query": query})
		if err := writeSSE(s.writer, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(encoded)},
		}); err != nil {
			return err
		}
	}
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (s *webMessagesStream) finishWebSearch(parsed parsedChat) error {
	if !shouldEmitWebMessagesSearch(parsed, s.options) {
		return nil
	}
	if err := s.startWebSearch(); err != nil {
		return err
	}
	blocks := webMessagesSearchBlocks(s.webSearchID, parsed, s.options)
	result := blocks[1]
	index := s.nextIndex
	s.nextIndex++
	if err := writeSSE(s.writer, "content_block_start", map[string]any{
		"type": "content_block_start", "index": index, "content_block": result,
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": index})
}

func (s *webMessagesStream) Finish(parsed parsedChat, payload map[string]any) error {
	if err := s.Start(); err != nil {
		return err
	}
	if err := s.closeThinking(); err != nil {
		return err
	}
	if err := s.finishWebSearch(parsed); err != nil {
		return err
	}
	if s.options.AnthropicWebSearch && s.pendingText.Len() > 0 {
		if err := s.startText(); err != nil {
			return err
		}
		emit, matched := s.stopFilter.Push(s.pendingText.String())
		if matched != "" {
			s.stopSequence = matched
		}
		if err := s.writeTextDelta(emit); err != nil {
			return err
		}
	}
	if s.stopSequence == "" {
		if pending := s.stopFilter.Flush(); pending != "" {
			if err := s.startText(); err != nil {
				return err
			}
			if err := s.writeTextDelta(pending); err != nil {
				return err
			}
		}
	}
	if err := s.closeText(); err != nil {
		return err
	}
	stopReason := "end_turn"
	if s.hasTools || len(parsed.ToolCalls) > 0 {
		stopReason = "tool_use"
	} else if s.stopSequence != "" {
		stopReason = "stop_sequence"
	}
	usage, _ := payload["usage"].(map[string]any)
	finalUsage := map[string]any{"output_tokens": usage["output_tokens"]}
	if shouldEmitWebMessagesSearch(parsed, s.options) {
		finalUsage["server_tool_use"] = map[string]any{"web_search_requests": webMessagesSearchRequests(parsed)}
	}
	if err := writeSSE(s.writer, "message_delta", map[string]any{
		"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nullableWebString(s.stopSequence)},
		"usage": finalUsage,
	}); err != nil {
		return err
	}
	return writeSSE(s.writer, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *webMessagesStream) closeThinking() error {
	if !s.thinkingStarted || s.thinkingClosed {
		return nil
	}
	s.thinkingClosed = true
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.thinkingIndex})
}

func (s *webMessagesStream) closeText() error {
	if !s.textStarted || s.textClosed {
		return nil
	}
	s.textClosed = true
	return writeSSE(s.writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": s.textIndex})
}

func writeWebStreamDelta(writer io.Writer, stream *webMessagesStream, operation, responseID, model, kind, delta string) error {
	if operation == conversation.OperationMessages {
		return stream.Delta(kind, delta)
	}
	return writeStreamDelta(writer, operation, responseID, model, kind, delta)
}

func writeWebStreamToolCalls(writer io.Writer, stream *webMessagesStream, operation, responseID, model string, calls []parsedToolCall) error {
	if operation == conversation.OperationMessages {
		return stream.Tools(calls)
	}
	return writeStreamToolCalls(writer, operation, responseID, model, calls)
}

type webStopFilter struct {
	sequences []string
	pending   string
	matched   string
}

func newWebStopFilter(sequences []string) *webStopFilter {
	filtered := make([]string, 0, len(sequences))
	for _, sequence := range sequences {
		if sequence != "" {
			filtered = append(filtered, sequence)
		}
	}
	return &webStopFilter{sequences: filtered}
}

func (f *webStopFilter) Push(delta string) (string, string) {
	if f == nil || len(f.sequences) == 0 {
		return delta, ""
	}
	if f.matched != "" {
		return "", f.matched
	}
	f.pending += delta
	matchAt := -1
	matched := ""
	for _, sequence := range f.sequences {
		if index := strings.Index(f.pending, sequence); index >= 0 && (matchAt < 0 || index < matchAt) {
			matchAt = index
			matched = sequence
		}
	}
	if matchAt >= 0 {
		emit := f.pending[:matchAt]
		f.pending = ""
		f.matched = matched
		return emit, matched
	}
	hold := 0
	for _, sequence := range f.sequences {
		maxPrefix := min(len(sequence)-1, len(f.pending))
		for size := maxPrefix; size > hold; size-- {
			if strings.HasSuffix(f.pending, sequence[:size]) {
				hold = size
				break
			}
		}
	}
	emitAt := len(f.pending) - hold
	emit := f.pending[:emitAt]
	f.pending = f.pending[emitAt:]
	return emit, ""
}

func (f *webStopFilter) Flush() string {
	if f == nil || f.matched != "" {
		return ""
	}
	value := f.pending
	f.pending = ""
	return value
}

func writeStreamStart(writer io.Writer, operation, responseID, model string, inputTokens int64, store bool) {
	if operation == "chat" {
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}}
		_ = writeSSE(writer, "", chunk)
		return
	}
	if operation == conversation.OperationMessages {
		_ = writeSSE(writer, "message_start", map[string]any{
			"type": "message_start", "message": map[string]any{
				"id": strings.Replace(responseID, "resp_", "msg_", 1), "type": "message", "role": "assistant", "model": model,
				"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
				"usage": map[string]any{"input_tokens": inputTokens, "output_tokens": 0, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0},
			},
		})
		_ = writeSSE(writer, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		return
	}
	_ = writeSSE(writer, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "status": "in_progress", "model": model, "output": []any{}, "store": store}})
}

type webVisibleStreamPhase struct {
	textStarted bool
}

// Allow keeps client-visible output monotonic when Grok Web emits additional
// reasoning after final text has already started. The complete reasoning is
// still retained in parsedChat for non-streaming output and usage accounting.
func (p *webVisibleStreamPhase) Allow(kind, delta string) bool {
	if delta == "" {
		return false
	}
	if kind == "reasoning" {
		return !p.textStarted
	}
	if kind == "text" {
		p.textStarted = true
	}
	return true
}

func writeStreamDelta(writer io.Writer, operation, responseID, model, kind, delta string) error {
	if operation == "chat" {
		field := "content"
		if kind == "reasoning" {
			field = "reasoning_content"
		}
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{field: delta}, "finish_reason": nil}}}
		return writeSSE(writer, "", chunk)
	}
	if operation == conversation.OperationMessages {
		if kind == "reasoning" {
			return nil
		}
		return writeSSE(writer, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": delta}})
	}
	return errors.New("Responses 流式 delta 必须通过统一 output 状态机发送")
}

func writeStreamToolCalls(writer io.Writer, operation, responseID, model string, calls []parsedToolCall) error {
	if operation == "chat" {
		for index, call := range calls {
			chunk := map[string]any{
				"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
				"created": time.Now().Unix(), "model": model,
				"choices": []any{map[string]any{"index": 0, "finish_reason": nil, "delta": map[string]any{
					"tool_calls": []any{map[string]any{
						"index": index, "id": call.ID, "type": "function",
						"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
					}},
				}}},
			}
			if err := writeSSE(writer, "", chunk); err != nil {
				return err
			}
		}
		return nil
	}
	if operation == conversation.OperationMessages {
		for index, call := range calls {
			contentIndex := index + 1
			if err := writeSSE(writer, "content_block_start", map[string]any{
				"type": "content_block_start", "index": contentIndex,
				"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
			}); err != nil {
				return err
			}
			if err := writeSSE(writer, "content_block_delta", map[string]any{
				"type": "content_block_delta", "index": contentIndex,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": call.Arguments},
			}); err != nil {
				return err
			}
			if err := writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": contentIndex}); err != nil {
				return err
			}
		}
		return nil
	}
	return errors.New("Responses 流式 tool call 必须通过统一 output 状态机发送")
}

func writeStreamDone(writer io.Writer, operation, responseID, model string, parsed parsedChat, payload map[string]any) error {
	if operation == "chat" {
		finishReason := "stop"
		if len(parsed.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		// Progressive annotations were already emitted as deltas. Repeating all of
		// them here makes clients that accumulate deltas display duplicates.
		// Top-level citations + server_side_tool_usage match non-stream chat (xAI).
		chunk := map[string]any{"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": model, "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}}, "usage": payload["usage"]}
		if citations := payload["citations"]; citations != nil {
			chunk["citations"] = citations
		}
		if toolUsage := payload["server_side_tool_usage"]; toolUsage != nil {
			chunk["server_side_tool_usage"] = toolUsage
		}
		if err := writeSSE(writer, "", chunk); err != nil {
			return err
		}
		_, err := io.WriteString(writer, "data: [DONE]\n\n")
		return err
	}
	if operation == conversation.OperationMessages {
		if err := writeSSE(writer, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
			return err
		}
		stopReason := "end_turn"
		if len(parsed.ToolCalls) > 0 {
			stopReason = "tool_use"
		}
		usage, _ := payload["usage"].(map[string]any)
		if err := writeSSE(writer, "message_delta", map[string]any{
			"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": usage["output_tokens"]},
		}); err != nil {
			return err
		}
		return writeSSE(writer, "message_stop", map[string]any{"type": "message_stop"})
	}
	return writeSSE(writer, "response.completed", map[string]any{"type": "response.completed", "response": payload})
}

// writeStreamAnnotations emits Chat Completions delta.annotations. Responses
// annotations are owned by webResponsesStream so their item coordinates cannot drift.
func writeStreamAnnotations(writer io.Writer, operation, responseID, model string, annotations []map[string]any, _ int) error {
	if len(annotations) == 0 || operation == conversation.OperationMessages {
		return nil
	}
	if operation == "chat" {
		chunk := map[string]any{
			"id": strings.Replace(responseID, "resp_", "chatcmpl_", 1), "object": "chat.completion.chunk",
			"created": time.Now().Unix(), "model": model,
			"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"annotations": chatAnnotations(annotations)}, "finish_reason": nil,
			}},
		}
		return writeSSE(writer, "", chunk)
	}
	return nil
}

func writeSSE(writer io.Writer, event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if event != "" {
		if _, err := fmt.Fprintf(writer, "event: %s\n", event); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(writer, "data: %s\n\n", data)
	return err
}

func estimateTokens(value string) int64 {
	count := utf8.RuneCountInString(value)
	if count == 0 {
		return 0
	}
	return int64((count + 3) / 4)
}

func newWebID(prefix string) string {
	value := make([]byte, 16)
	_, _ = rand.Read(value)
	return prefix + "_" + hex.EncodeToString(value)
}

func streamHeaders() http.Header {
	value := http.Header{}
	value.Set("Content-Type", "text/event-stream; charset=utf-8")
	value.Set("Cache-Control", "no-cache")
	value.Set("X-Accel-Buffering", "no")
	return value
}

func jsonHeaders() http.Header {
	value := http.Header{}
	value.Set("Content-Type", "application/json; charset=utf-8")
	return value
}

func jsonProviderResponse(status int, value any) *provider.Response {
	data, _ := json.Marshal(value)
	return &provider.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: jsonHeaders(), Body: io.NopCloser(bytes.NewReader(data))}
}

type releaseBody struct {
	io.ReadCloser
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	if b.release != nil {
		b.release()
		b.release = nil
	}
	return err
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

type readerCloser struct {
	io.Reader
	closer io.Closer
}

func (r *readerCloser) Close() error { return r.closer.Close() }

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close()
	if b.cancel != nil {
		b.cancel()
		b.cancel = nil
	}
	return err
}
