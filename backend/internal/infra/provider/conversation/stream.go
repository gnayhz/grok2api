package conversation

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/streampipe"
)

const (
	maxDeferredSearchTextBytes       = 8 << 20
	maxDeferredReasoningSummaryBytes = 8 << 20
	maxSSEEventBytes                 = 8 << 20
	// maxParsedSSEJSONBytes 与 inference 热路径同口径：超过则不再把整段
	// JSON（通常是 encrypted_content）Unmarshal 进 map/结构体。
	maxParsedSSEJSONBytes = 64 << 10

	// ThinkingEvidenceComment 是转换器在「客户端未请求 thinking 的
	// Messages 流式请求」上看到上游可见思考文本时写入的内部 SSE 注释。
	// 推理模型对每个回答都会思考（语料复核：未指定强度的
	// 首轮 36/36、续写轮 136/153 均产生思考），未请求 thinking 时转换器
	// 不转发任何思考增量，质量守卫将失去区分健康与降智（零思考直接
	// 正文）的唯一证据通道。该注释语义等同 thinking_delta，由 gateway
	// 扫描器（quality_retry_scan.go）计为思考证据，并由 transport 层
	// 剥离，不进入客户端流量。
	ThinkingEvidenceComment = ": grok2api-thinking-evidence"

	// contentDoomLoopThreshold 连续重复同一可见内容增量时终止流。真正的
	// 内容循环会消耗配额和客户端上下文，因此远低于推理上限；但仍需容纳
	// 合法重复：markdown 分隔线与表格边框会以相同单字符增量（"-"、"="、
	// "|"）连续输出。
	contentDoomLoopThreshold = 128

	// reasoningDoomLoopThreshold 高于内容阈值：high/xhigh 推理会大量重复
	// 同一短标记（"so"、"hmm"、"wait"、列表符号）。共用低阈值会过早终止
	// 有效的深度推理响应。
	reasoningDoomLoopThreshold = 256
)

// ConvertResponseStream 将 Responses SSE 转换为 Chat Completions 或 Anthropic Messages SSE。
func ConvertResponseStream(source io.ReadCloser, operation string) io.ReadCloser {
	return ConvertResponseStreamWithOptions(source, operation, ResponseOptions{})
}

// ConvertResponseStreamWithOptions 按下游协议选项生成 Chat 或 Anthropic SSE。
func ConvertResponseStreamWithOptions(source io.ReadCloser, operation string, options ResponseOptions) io.ReadCloser {
	if operation == OperationResponses {
		return guardResponseStream(source)
	}
	reader, writer := io.Pipe()
	stream := newStreamPipeReadCloser(reader, source)
	go func() {
		defer stream.closeSource()
		// 转换器直接解析上游字节流, panic 不得击穿进程:streampipe 捕获后以
		// 错误关闭 pipe, 客户端得到可重试的流错误而非进程崩溃。
		streampipe.Run(writer, func() error {
			converter := newStreamConverter(writer, operation, options)
			err := consumeSSE(source, converter.handle)
			if err == nil {
				err = converter.finish()
			}
			return err
		})
	}()
	return stream
}

type streamConverter struct {
	writer            io.Writer
	operation         string
	id                string
	model             string
	created           int64
	started           bool
	finished          bool
	textStarted       bool
	textIndex         int
	thinkingStarted   bool
	thinkingClosed    bool
	thinkingIndex     int
	thinkingItemID    string
	reasoningItems    map[string]*reasoningStreamState
	reasoningOrder    []string
	activeReasoningID string
	nextIndex         int
	tools             map[string]streamTool
	webSearch         []webSearchCall
	webSearchEmitted  map[string]bool
	deferSearchText   bool
	pendingSearchText strings.Builder
	usage             responseUsage
	options           ResponseOptions
	stopFilter        *anthropicStreamStopFilter
	stopSequence      string
	refused           bool
	repeatTracker     streamRepeatTracker
	outBuf            bytes.Buffer
	// thinkingEvidenceMarked 保证 ThinkingEvidenceComment 至多写一次。
	thinkingEvidenceMarked bool
}

// streamRepeatTracker 在协议转换、缓冲和 stop filter 之前跟踪上游增量，
// 避免任一下游路径绕过循环保护。
type streamRepeatTracker struct {
	// 循环检测：可见内容与推理各自计数。推理合法地远比可见输出更常重复
	// 同一短标记，共用一个计数器会过早终止有效的 high/xhigh 响应。
	lastContentDelta   string
	contentRepeatCount int
	lastReasonDelta    string
	reasonRepeatCount  int
}

// streamPipeReadCloser ensures a downstream cancellation immediately closes the
// upstream body, including while the forwarding goroutine is blocked in Read.
type streamPipeReadCloser struct {
	*io.PipeReader
	source    io.ReadCloser
	closeOnce sync.Once
	closeErr  error
}

func newStreamPipeReadCloser(reader *io.PipeReader, source io.ReadCloser) *streamPipeReadCloser {
	return &streamPipeReadCloser{PipeReader: reader, source: source}
}

func (r *streamPipeReadCloser) Close() error {
	readerErr := r.PipeReader.Close()
	sourceErr := r.closeSource()
	if readerErr != nil {
		return readerErr
	}
	return sourceErr
}

func (r *streamPipeReadCloser) closeSource() error {
	r.closeOnce.Do(func() {
		r.closeErr = r.source.Close()
	})
	return r.closeErr
}

type streamTool struct {
	Index     int
	ID        string
	Name      string
	Arguments string
	SentArgs  bool
	Closed    bool
}

type reasoningStreamState struct {
	summary   strings.Builder
	rawSeen   bool
	done      bool
	anonymous bool
}

func newStreamConverter(writer io.Writer, operation string, options ResponseOptions) *streamConverter {
	return &streamConverter{
		writer: writer, operation: operation, created: time.Now().Unix(), tools: make(map[string]streamTool),
		webSearchEmitted: make(map[string]bool),
		reasoningItems:   make(map[string]*reasoningStreamState),
		deferSearchText:  operation == OperationMessages && options.AnthropicWebSearch,
		options:          options, stopFilter: newAnthropicStreamStopFilter(options.StopSequences),
	}
}

// noteWebSearch records a Build web_search_call. Emission is deferred to doneMessages
// so we always use the completed action.sources payload from the final envelope when available.
// For progressive UI we still emit server_tool_use as soon as we see the call.
func (c *streamConverter) noteWebSearch(call webSearchCall, final bool) error {
	filtered := dedupeWebSearchCalls([]webSearchCall{call})
	if len(filtered) == 0 {
		return nil
	}
	call = filtered[0]
	replaced := false
	for i, existing := range c.webSearch {
		if existing.ID == call.ID {
			// Prefer richer final payload.
			if final || len(call.Hits) >= len(existing.Hits) {
				c.webSearch[i] = call
			}
			replaced = true
			break
		}
	}
	if !replaced {
		if len(c.webSearch) >= maxWebSearchCalls {
			return nil
		}
		c.webSearch = append(c.webSearch, call)
	}
	if !c.textStarted {
		c.deferSearchText = true
	}
	if c.textStarted || (c.thinkingStarted && !c.thinkingClosed) {
		return nil
	}
	// Emit server_tool_use promptly so Claude Code can show "Searching: …".
	return c.emitWebSearchUse(call)
}

func (c *streamConverter) emitWebSearchUse(call webSearchCall) error {
	if err := c.start(); err != nil {
		return err
	}
	if c.webSearchEmitted[call.ID+"#use"] {
		return nil
	}
	index := c.nextIndex
	c.nextIndex++
	c.webSearchEmitted[call.ID+"#use"] = true
	if err := c.writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": index,
		"content_block": map[string]any{"type": "server_tool_use", "id": call.ID, "name": "web_search", "input": map[string]any{}},
	}); err != nil {
		return err
	}
	if call.Query != "" {
		if err := c.writeEvent("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": queryJSONPartial(call.Query)},
		}); err != nil {
			return err
		}
	}
	if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
		return err
	}
	return nil
}

func (c *streamConverter) emitPendingWebSearchResults() error {
	c.webSearch = dedupeWebSearchCalls(c.webSearch)
	for _, call := range c.webSearch {
		if c.webSearchEmitted[call.ID+"#result"] {
			continue
		}
		if !c.webSearchEmitted[call.ID+"#use"] {
			if err := c.emitWebSearchUse(call); err != nil {
				return err
			}
		}
		if err := c.start(); err != nil {
			return err
		}
		index := c.nextIndex
		c.nextIndex++
		c.webSearchEmitted[call.ID+"#result"] = true
		var content any
		if call.Failed {
			code := call.Code
			if code == "" {
				code = "unavailable"
			}
			content = map[string]any{"type": "web_search_tool_result_error", "error_code": code}
		} else {
			hits := make([]any, 0, len(call.Hits))
			for _, hit := range call.Hits {
				hits = append(hits, map[string]any{"type": "web_search_result", "title": hit.Title, "url": hit.URL})
			}
			content = hits
		}
		if err := c.writeEvent("content_block_start", map[string]any{
			"type": "content_block_start", "index": index,
			"content_block": map[string]any{
				"type": "web_search_tool_result", "tool_use_id": call.ID, "content": content,
			},
		}); err != nil {
			return err
		}
		if err := c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": index}); err != nil {
			return err
		}
	}
	return nil
}

func (c *streamConverter) handle(event string, data []byte) error {
	if c.finished {
		return nil
	}
	typeName, root, ok := parseSSEEvent(event, data)
	if !ok {
		return nil
	}
	if err := c.repeatTracker.trackEvent(typeName, root); err != nil {
		return err
	}
	if c.stopSequence != "" && typeName != "response.completed" && typeName != "response.incomplete" && typeName != "response.failed" && typeName != "error" {
		return nil
	}
	if root == nil {
		switch typeName {
		case "response.output_item.done":
			return c.handleHugeOutputItemDone(data)
		case "response.completed", "response.incomplete":
			return c.handleHugeCompleted(data, typeName)
		case "response.failed":
			return c.streamError(jsonpeek.Prefix(data, 8192))
		default:
			return nil
		}
	}
	switch typeName {
	case "response.created", "response.in_progress":
		var response responseEnvelope
		_ = json.Unmarshal(root["response"], &response)
		c.setResponse(response)
		return c.start()
	case "response.output_text.delta":
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		if err := c.start(); err != nil {
			return err
		}
		if c.operation == OperationMessages && c.deferSearchText {
			return c.bufferSearchText(delta)
		}
		return c.textDelta(delta)
	case "response.refusal.delta":
		var delta string
		_ = json.Unmarshal(root["delta"], &delta)
		c.refused = true
		if c.operation == OperationChat {
			return c.chatDelta(map[string]any{"refusal": delta})
		}
		return c.textDeltaMessages(delta)
	case "response.output_text.annotation.added":
		if c.operation != OperationChat {
			return nil
		}
		var annotation any
		if json.Unmarshal(root["annotation"], &annotation) != nil || annotation == nil {
			return nil
		}
		return c.chatDelta(map[string]any{"annotations": []any{annotation}})
	case "response.reasoning_summary_text.delta":
		var itemID, delta string
		_ = json.Unmarshal(root["item_id"], &itemID)
		_ = json.Unmarshal(root["delta"], &delta)
		return c.reasoningSummaryDelta(itemID, delta)
	case "response.reasoning_text.delta":
		var itemID, delta string
		_ = json.Unmarshal(root["item_id"], &itemID)
		_ = json.Unmarshal(root["delta"], &delta)
		return c.reasoningTextDelta(itemID, delta)
	case "response.output_item.added":
		var item responseItem
		_ = json.Unmarshal(root["item"], &item)
		if item.Type == "reasoning" && c.reasoningOutputEnabled() {
			c.ensureReasoningState(item.ID)
		}
		if item.Type == "reasoning" && c.operation == OperationMessages && c.options.AnthropicThinking {
			return c.thinkingStart(item.ID)
		}
		if item.Type == "web_search_call" && c.operation == OperationMessages && c.options.AnthropicWebSearch {
			if call, ok := parseWebSearchCallItem(item); ok {
				return c.noteWebSearch(call, false)
			}
			return nil
		}
		if item.Type != "function_call" {
			return nil
		}
		var outputIndex int
		_ = json.Unmarshal(root["output_index"], &outputIndex)
		return c.toolStart(item, outputIndex)
	case "response.function_call_arguments.delta":
		var itemID, delta string
		_ = json.Unmarshal(root["item_id"], &itemID)
		_ = json.Unmarshal(root["delta"], &delta)
		return c.toolDelta(itemID, delta)
	case "response.function_call_arguments.done":
		var itemID, arguments string
		_ = json.Unmarshal(root["item_id"], &itemID)
		_ = json.Unmarshal(root["arguments"], &arguments)
		return c.toolArgumentsDone(itemID, arguments)
	case "response.output_item.done":
		var item responseItem
		_ = json.Unmarshal(root["item"], &item)
		if item.Type == "function_call" {
			return c.toolArgumentsDone(item.ID, item.Arguments)
		}
		if item.Type == "reasoning" {
			if c.reasoningOutputEnabled() {
				if err := c.reasoningDone(item); err != nil {
					return err
				}
			}
			return c.thinkingDone(item)
		}
		if item.Type == "web_search_call" && c.operation == OperationMessages && c.options.AnthropicWebSearch {
			if call, ok := parseWebSearchCallItem(item); ok {
				return c.noteWebSearch(call, true)
			}
		}
	case "response.completed", "response.incomplete":
		var response responseEnvelope
		_ = json.Unmarshal(root["response"], &response)
		c.setResponse(response)
		if c.operation == OperationMessages && c.options.AnthropicWebSearch {
			parsed := parseResponse(response)
			for _, call := range parsed.WebSearch {
				if err := c.noteWebSearch(call, true); err != nil {
					return err
				}
			}
		}
		status := response.Status
		if status == "" && typeName == "response.incomplete" {
			status = "incomplete"
		}
		return c.done(status)
	case "error", "response.failed":
		return c.streamError(data)
	}
	return nil
}

func (c *streamConverter) reasoningOutputEnabled() bool {
	return c.operation == OperationChat || (c.operation == OperationMessages && c.options.AnthropicThinking)
}

func (c *streamConverter) ensureReasoningState(itemID string) (string, *reasoningStreamState) {
	key := itemID
	if key != "" {
		if state, exists := c.reasoningItems[key]; exists {
			c.activeReasoningID = key
			return key, state
		}
		// Some compatible upstreams omit item_id on the first delta. Once the
		// real item arrives, attach that anonymous state instead of creating a
		// second source that could later replay the buffered summary.
		if anonymous := c.activeReasoningID; anonymous != "" {
			if state := c.reasoningItems[anonymous]; state != nil && state.anonymous && !state.done {
				delete(c.reasoningItems, anonymous)
				state.anonymous = false
				c.reasoningItems[key] = state
				for index, existing := range c.reasoningOrder {
					if existing == anonymous {
						c.reasoningOrder[index] = key
						break
					}
				}
				c.activeReasoningID = key
				return key, state
			}
		}
	}
	if key == "" {
		key = c.activeReasoningID
	}
	anonymous := false
	if key == "" {
		key = fmt.Sprintf("#reasoning-%d", len(c.reasoningOrder)+1)
		anonymous = true
	}
	state, exists := c.reasoningItems[key]
	if !exists {
		state = &reasoningStreamState{anonymous: anonymous}
		c.reasoningItems[key] = state
		c.reasoningOrder = append(c.reasoningOrder, key)
	}
	c.activeReasoningID = key
	return key, state
}

func (c *streamConverter) reasoningSummaryDelta(itemID, delta string) error {
	if delta == "" {
		return nil
	}
	if !c.reasoningOutputEnabled() {
		// 空白增量不是思考证据（生产抓流：summary 尾部会补纯换行增量），
		// 不得写证据注释——守卫扫描端同样按非空白判定。
		if strings.TrimSpace(delta) == "" {
			return nil
		}
		return c.markThinkingEvidence()
	}
	_, state := c.ensureReasoningState(itemID)
	if state.done || state.rawSeen {
		return nil
	}
	// Console can publish the same client-facing thought through summary and
	// raw reasoning events. Defer summary until item completion so raw can take
	// precedence without relying on chunk boundaries or text equality.
	pending := state.summary.Len()
	if pending >= maxDeferredReasoningSummaryBytes || len(delta) > maxDeferredReasoningSummaryBytes-pending {
		return fmt.Errorf("reasoning summary 延迟缓冲超过 %d MiB", maxDeferredReasoningSummaryBytes>>20)
	}
	state.summary.WriteString(delta)
	return nil
}

func (c *streamConverter) reasoningTextDelta(itemID, delta string) error {
	if delta == "" {
		return nil
	}
	if !c.reasoningOutputEnabled() {
		// 空白增量不是思考证据（生产抓流：summary 尾部会补纯换行增量），
		// 不得写证据注释——守卫扫描端同样按非空白判定。
		if strings.TrimSpace(delta) == "" {
			return nil
		}
		return c.markThinkingEvidence()
	}
	_, state := c.ensureReasoningState(itemID)
	if state.done {
		return nil
	}
	if !state.rawSeen {
		state.rawSeen = true
		state.summary.Reset()
	}
	return c.emitReasoningDelta(delta)
}

// markThinkingEvidence 在客户端未请求 thinking 的 Messages 流式转换上，
// 把「上游产出了可见思考文本」以内部 SSE 注释保留为守卫证据（常量
// ThinkingEvidenceComment 详注）。仅此一种场景写入：thinking 已启用时
// 转换器转发 thinking_delta，chat 协议始终转发 reasoning_content，均无
// 需注释通道。注释行随后由 transport 层剥离。
func (c *streamConverter) markThinkingEvidence() error {
	if c.thinkingEvidenceMarked || c.operation != OperationMessages || c.options.AnthropicThinking {
		return nil
	}
	if err := c.start(); err != nil {
		return err
	}
	if _, err := io.WriteString(c.writer, ThinkingEvidenceComment+"\n\n"); err != nil {
		return err
	}
	c.thinkingEvidenceMarked = true
	return nil
}

func (c *streamConverter) emitReasoningDelta(delta string) error {
	if c.operation == OperationChat {
		return c.chatDelta(map[string]any{"reasoning_content": delta})
	}
	if c.operation == OperationMessages {
		return c.thinkingDelta(delta)
	}
	return nil
}

func (c *streamConverter) reasoningDone(item responseItem) error {
	key, state := c.ensureReasoningState(item.ID)
	if state.done {
		return nil
	}
	if err := c.flushReasoningSummary(state); err != nil {
		return err
	}
	state.done = true
	if c.activeReasoningID == key {
		c.activeReasoningID = ""
	}
	return nil
}

func (c *streamConverter) flushReasoningSummary(state *reasoningStreamState) error {
	if state == nil || state.rawSeen || state.summary.Len() == 0 {
		return nil
	}
	value := state.summary.String()
	state.summary.Reset()
	return c.emitReasoningDelta(value)
}

func (c *streamConverter) flushPendingReasoning() error {
	for _, key := range c.reasoningOrder {
		state := c.reasoningItems[key]
		if state.done {
			continue
		}
		if err := c.flushReasoningSummary(state); err != nil {
			return err
		}
		state.done = true
	}
	c.activeReasoningID = ""
	return nil
}

func (c *streamConverter) bufferSearchText(delta string) error {
	pending := c.pendingSearchText.Len()
	if pending >= maxDeferredSearchTextBytes || len(delta) > maxDeferredSearchTextBytes-pending {
		return fmt.Errorf("WebSearch 延迟文本缓冲超过 %d MiB", maxDeferredSearchTextBytes>>20)
	}
	c.pendingSearchText.WriteString(delta)
	return nil
}

func (c *streamConverter) setResponse(value responseEnvelope) {
	if value.ID != "" {
		c.id = value.ID
	}
	if value.Model != "" {
		c.model = value.Model
	}
	if value.CreatedAt != 0 {
		c.created = value.CreatedAt
	}
	c.usage = mergeResponseUsage(c.usage, value.Usage)
}

func mergeResponseUsage(current, update responseUsage) responseUsage {
	if update.InputTokens != 0 {
		current.InputTokens = update.InputTokens
	}
	if update.OutputTokens != 0 {
		current.OutputTokens = update.OutputTokens
	}
	if update.TotalTokens != 0 {
		current.TotalTokens = update.TotalTokens
	}
	if update.CostInUSDTicks != 0 {
		current.CostInUSDTicks = update.CostInUSDTicks
	}
	if update.NumSourcesUsed != 0 {
		current.NumSourcesUsed = update.NumSourcesUsed
	}
	if update.NumServerSideToolsUsed != 0 {
		current.NumServerSideToolsUsed = update.NumServerSideToolsUsed
	}
	if update.InputTokensDetails.CachedTokens != 0 {
		current.InputTokensDetails.CachedTokens = update.InputTokensDetails.CachedTokens
	}
	if update.OutputTokensDetails.ReasoningTokens != 0 {
		current.OutputTokensDetails.ReasoningTokens = update.OutputTokensDetails.ReasoningTokens
	}
	if update.ContextDetails.InputTokens != 0 {
		current.ContextDetails.InputTokens = update.ContextDetails.InputTokens
	}
	if update.ContextDetails.OutputTokens != 0 {
		current.ContextDetails.OutputTokens = update.ContextDetails.OutputTokens
	}
	return current
}

func (c *streamConverter) start() error {
	if c.started {
		return nil
	}
	c.started = true
	if c.id == "" {
		c.id = "resp_" + fmt.Sprint(time.Now().UnixNano())
	}
	if c.operation == OperationChat {
		return c.startChat()
	}
	return c.startMessages()
}

func (c *streamConverter) textDelta(delta string) error {
	if c.operation == OperationChat {
		return c.textDeltaChat(delta)
	}
	return c.textDeltaMessages(delta)
}

func (c *streamConverter) toolStart(item responseItem, outputIndex int) error {
	if c.operation == OperationMessages {
		return c.toolStartMessages(item)
	}
	return c.toolStartChat(item, outputIndex)
}

func (c *streamConverter) toolDelta(itemID, delta string) error {
	if c.operation == OperationChat {
		return c.toolDeltaChat(itemID, delta)
	}
	return c.toolDeltaMessages(itemID, delta)
}

func (c *streamConverter) toolArgumentsDone(itemID, arguments string) error {
	if c.operation == OperationChat {
		return c.toolArgumentsDoneChat(itemID, arguments)
	}
	return c.toolArgumentsDoneMessages(itemID, arguments)
}

func (c *streamConverter) done(status string) error {
	if c.finished {
		return nil
	}
	if err := c.start(); err != nil {
		return err
	}
	if err := c.flushPendingReasoning(); err != nil {
		return err
	}
	if c.operation == OperationChat {
		return c.doneChat(status)
	}
	return c.doneMessages(status)
}

func (c *streamConverter) streamError(data []byte) error {
	if err := c.flushPendingReasoning(); err != nil {
		return err
	}
	c.finished = true
	if c.operation == OperationMessages {
		return c.streamErrorMessages(data)
	}
	return c.streamErrorChat(data)
}

func (c *streamConverter) finish() error {
	if c.finished {
		return nil
	}
	return c.done("")
}

func streamErrorValue(data []byte) any {
	if raw := jsonpeek.RawValue(data, "error"); len(raw) > 0 && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var value any
		if json.Unmarshal(raw, &value) == nil && value != nil {
			return value
		}
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return strings.TrimSpace(string(data))
	}
	if response, ok := root["response"].(map[string]any); ok {
		if value, exists := response["error"]; exists && value != nil {
			return value
		}
	}
	if value, exists := root["error"]; exists && value != nil {
		return value
	}
	if message, ok := root["message"].(string); ok {
		return message
	}
	return strings.TrimSpace(string(data))
}

func (c *streamConverter) writeData(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.outBuf.Reset()
	c.outBuf.Grow(len(data) + 8)
	c.outBuf.WriteString("data: ")
	c.outBuf.Write(data)
	c.outBuf.WriteByte(10)
	c.outBuf.WriteByte(10)
	_, err = c.writer.Write(c.outBuf.Bytes())
	return err
}

func (c *streamConverter) writeSignatureDelta(signatureJSON []byte) error {
	index := strconv.Itoa(c.thinkingIndex)
	c.outBuf.Reset()
	c.outBuf.Grow(len(signatureJSON) + 96 + len(index))
	c.outBuf.WriteString("event: content_block_delta\ndata: {\"delta\":{\"signature\":")
	c.outBuf.Write(signatureJSON)
	c.outBuf.WriteString(",\"type\":\"signature_delta\"},\"index\":")
	c.outBuf.WriteString(index)
	c.outBuf.WriteString(",\"type\":\"content_block_delta\"}\n\n")
	_, err := c.writer.Write(c.outBuf.Bytes())
	return err
}

func (c *streamConverter) writeEvent(event string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.outBuf.Reset()
	c.outBuf.Grow(len(event) + len(data) + 16)
	c.outBuf.WriteString("event: ")
	c.outBuf.WriteString(event)
	c.outBuf.WriteByte(10)
	c.outBuf.WriteString("data: ")
	c.outBuf.Write(data)
	c.outBuf.WriteByte(10)
	c.outBuf.WriteByte(10)
	_, err = c.writer.Write(c.outBuf.Bytes())
	return err
}

func consumeSSE(source io.Reader, handle func(string, []byte) error) error {
	reader := bufio.NewReaderSize(source, 64<<10)
	var event string
	var data bytes.Buffer
	var long []byte
	firstLine := true
	for {
		frag, err := reader.ReadSlice('\n')
		if len(frag) > 0 {
			var line []byte
			if err == bufio.ErrBufferFull {
				if cap(long) == 0 {
					long = make([]byte, 0, 64<<10)
				}
				long = append(long, frag...)
				if len(long) > maxSSEEventBytes {
					return fmt.Errorf("SSE 单事件超过 8 MiB")
				}
				continue
			}
			if len(long) > 0 {
				long = append(long, frag...)
				line = long
				long = nil
			} else {
				line = frag
			}
			for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
				line = line[:len(line)-1]
			}
			if firstLine {
				line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
				firstLine = false
			}
			if data.Len() > maxSSEEventBytes {
				return fmt.Errorf("SSE 单事件超过 8 MiB")
			}
			switch {
			case bytes.HasPrefix(line, []byte("event:")):
				event = string(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("event:"))))
			case bytes.HasPrefix(line, []byte("data:")):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:"))))
			case len(line) == 0:
				if data.Len() > 0 {
					if handleErr := handle(event, data.Bytes()); handleErr != nil {
						return handleErr
					}
				}
				event = ""
				data.Reset()
			}
		}
		if err != nil {
			if err == bufio.ErrBufferFull {
				continue
			}
			if err == io.EOF {
				if data.Len() > 0 {
					return handle(event, data.Bytes())
				}
				return nil
			}
			return err
		}
	}
}

func parseSSEEvent(event string, data []byte) (string, map[string]json.RawMessage, bool) {
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		return "", nil, false
	}
	if len(data) > maxParsedSSEJSONBytes {
		typeName := event
		if typeName == "" {
			head := data
			if len(head) > 4096 {
				head = data[:4096]
			}
			typeName = jsonpeek.StringField(head, "type")
			if typeName == "" {
				// 上游原生帧当前 type 在前，但这是未声明的键序契约；头窗
				// 未命中时对完整帧做键序无关的根层扫描兜底（与 inference
				// 侧 sseEventType 同口径，防同类 terminal 丢失回归）。
				typeName = jsonpeek.RootStringFieldScan(data, "type")
			}
		}
		switch typeName {
		case "response.output_item.added", "response.output_item.done",
			"response.completed", "response.incomplete", "response.failed":
			return typeName, nil, true
		}
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return "", nil, false
	}
	typeName := event
	if typeName == "" {
		_ = json.Unmarshal(root["type"], &typeName)
	}
	return typeName, root, true
}

func (t *streamRepeatTracker) trackEvent(typeName string, root map[string]json.RawMessage) error {
	if root == nil {
		return nil
	}
	var delta string
	switch typeName {
	case "response.output_text.delta":
		_ = json.Unmarshal(root["delta"], &delta)
		return t.trackContent(delta)
	case "response.reasoning_summary_text.delta":
		_ = json.Unmarshal(root["delta"], &delta)
		return t.trackReasoning(delta, "model reasoning summary loop detected")
	case "response.reasoning_text.delta":
		_ = json.Unmarshal(root["delta"], &delta)
		return t.trackReasoning(delta, "model reasoning loop detected")
	default:
		return nil
	}
}

func (t *streamRepeatTracker) trackContent(delta string) error {
	if delta == "" {
		return nil
	}
	if delta != t.lastContentDelta {
		t.lastContentDelta = delta
		t.contentRepeatCount = 1
		return nil
	}
	t.contentRepeatCount++
	if t.contentRepeatCount > contentDoomLoopThreshold {
		return fmt.Errorf("%w (repeated content delta %d times)", neterror.ErrUpstreamOutputLoop, t.contentRepeatCount)
	}
	return nil
}

func (t *streamRepeatTracker) trackReasoning(delta, message string) error {
	if delta == "" {
		return nil
	}
	if delta != t.lastReasonDelta {
		t.lastReasonDelta = delta
		t.reasonRepeatCount = 1
		return nil
	}
	t.reasonRepeatCount++
	if t.reasonRepeatCount > reasoningDoomLoopThreshold {
		return fmt.Errorf("%w: %s (repeated delta %d times)", neterror.ErrUpstreamOutputLoop, message, t.reasonRepeatCount)
	}
	return nil
}

// guardResponseStream 保持 native Responses SSE 的原始字节不变，同时在读取时
// 解析事件并在检测到循环时关闭上游。
func guardResponseStream(source io.ReadCloser) io.ReadCloser {
	reader, writer := io.Pipe()
	stream := newStreamPipeReadCloser(reader, source)
	go func() {
		defer stream.closeSource()
		tracker := streamRepeatTracker{}
		err := consumeSSE(io.TeeReader(source, writer), func(event string, data []byte) error {
			typeName, root, ok := parseSSEEvent(event, data)
			if !ok {
				return nil
			}
			return tracker.trackEvent(typeName, root)
		})
		_ = writer.CloseWithError(err)
	}()
	return stream
}
