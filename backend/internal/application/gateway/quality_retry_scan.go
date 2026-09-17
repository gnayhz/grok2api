package gateway

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

const (
	qualityProtocolChat       = "chat"
	qualityProtocolResponses  = "responses"
	qualityProtocolAnthropic  = "anthropic"
	qualityHoldMaxBufferBytes = 4 << 20
	qualityReadChunkBytes     = 32 << 10
	// Whole-body inspection has a 32 MiB input budget. Exceeding either
	// budget rejects delivery as inconclusive; it never establishes degradation.
	qualityBodyPeekLimit = 32 << 20
	// 注：SSE 注释行（": " 前缀，含上游 keepalive）不是思考证据——降智
	// 流同样会发；扫描器按非 data: 行统一跳过。历史上转换器发过私有时序
	// 注释 ": grok2api-reasoning-start"，该注入/剥除链路已随私有注释清理
	// （蓝图 #14）整体移除。
)

type qualityScanState struct {
	protocolErr error
	kernel      QualityHoldKernel
	protocol    string
	pending     []byte
	// hasThinking 仅由可见思考文本增量置位（三个协议同语义）：密文、
	// 注释、usage 声明都不构成思考证据。
	hasThinking bool
	// reasoningEndedWithoutThinking 推理阶段已闭合（Responses 的
	// output_item.done 携 reasoning item / Anthropic 的 thinking
	// content_block_stop）而未产出任何思考增量——零延迟拦截信号。
	reasoningEndedWithoutThinking bool
	// anthropicThinkingStarted 跟踪 Messages 协议 thinking 块已开启：
	// 块停止事件不携带类型，只能用该状态识别"thinking 块闭合"。
	anthropicThinkingStarted bool
	visibleRunes             int
	// aggregateRunes 记录仅在终态聚合形式到达的文本（completed.output[].
	// content[].text，无增量 delta）：同样是真实可见输出，计入可见估计。
	aggregateRunes int
	// semanticOutput 表示流携带非文本的语义输出（工具调用参数、非文本
	// 内容块）：既不是空流，证据截止也不把搜索静默当降智。终态由
	// emptyStreamVerdict 按思考期望扣留或放行，不是无条件交付。
	semanticOutput bool
	usage          Usage
	responseID     string
	terminal       bool
	// Completion facts are independent from the early admission decision.
	completed bool
	failed    bool
	// sawDataEvent 标记已解析到至少一个 SSE data 事件（任意类型）。供首事件
	// 截止使用：keepalive 注释不算——降智排队期间上游只发注释或零字节。
	sawDataEvent bool
	// 指纹采样（quality_hold attempt 存档）：事件类型去重序列与关键时序。
	// 不含文本/密文。startedAt 在 peek 入口赋值。
	startedAt     time.Time
	firstEventAt  time.Time
	itemDoneAt    time.Time
	summaryAt     time.Time
	eventTypes    []string
	sawEncrypted  bool
	firstItemType string
	// 逐帧解码 scratch：事件结构体挂在 state 上复用（见类型注释）。
	chatEvent      qualityChatEvent
	responsesEvent qualityResponsesEvent
	anthropicEvent qualityAnthropicEvent
}

const qualityFingerprintEventCap = 12

func noteQualityEvent(state *qualityScanState, typ string) {
	if state == nil || typ == "" {
		return
	}
	now := time.Now()
	if state.firstEventAt.IsZero() {
		state.firstEventAt = now
	}
	switch typ {
	case "response.output_item.done", "content_block_stop":
		if state.itemDoneAt.IsZero() {
			state.itemDoneAt = now
		}
	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "thinking_delta":
		if state.summaryAt.IsZero() {
			state.summaryAt = now
		}
	}
	if len(state.eventTypes) >= qualityFingerprintEventCap {
		return
	}
	for _, existing := range state.eventTypes {
		if existing == typ {
			return
		}
	}
	state.eventTypes = append(state.eventTypes, typ)
}

func noteQualityItem(state *qualityScanState, itemType string) {
	if state == nil || state.firstItemType != "" {
		return
	}
	itemType = strings.ToLower(strings.TrimSpace(itemType))
	if itemType == "" {
		return
	}
	state.firstItemType = itemType
}

func noteQualityEncrypted(state *qualityScanState, payload []byte) {
	if state == nil || state.sawEncrypted || len(payload) == 0 {
		return
	}
	if bytes.Contains(payload, []byte("encrypted_content")) && !bytes.Contains(payload, []byte("encrypted_content\":\"\"")) {
		state.sawEncrypted = true
	}
}

func relMS(start, at time.Time) int64 {
	if start.IsZero() || at.IsZero() {
		return 0
	}
	ms := at.Sub(start).Milliseconds()
	if ms < 0 {
		return 0
	}
	return ms
}

func (s *qualityScanState) fingerprint(verdict QualityVerdict, err error) qualityHoldFingerprint {
	if s == nil {
		return qualityHoldFingerprint{}
	}
	sig := s.signals()
	fp := qualityHoldFingerprint{
		Completed: s.completed, Failed: s.failed,
		Protocol:        s.protocol,
		Verdict:         string(verdict),
		Rule:            qualityHoldRule(sig, s.semanticOutput, err),
		HasThinking:     s.hasThinking,
		ReasoningEnded:  s.reasoningEndedWithoutThinking,
		SemanticOutput:  s.semanticOutput,
		SawDataEvent:    s.sawDataEvent,
		Encrypted:       s.sawEncrypted,
		VisibleRunes:    max(s.visibleRunes, s.aggregateRunes),
		OutputTokens:    sig.OutputTokens,
		ReasoningTokens: sig.ReasoningTokens,
		Events:          append([]string(nil), s.eventTypes...),
		FirstItem:       s.firstItemType,
		FirstEventMS:    relMS(s.startedAt, s.firstEventAt),
		ItemDoneMS:      relMS(s.startedAt, s.itemDoneAt),
		SummaryMS:       relMS(s.startedAt, s.summaryAt),
	}
	if !s.startedAt.IsZero() {
		fp.PeekMS = time.Since(s.startedAt).Milliseconds()
	}
	if err != nil {
		fp.Error = err.Error()
	}
	return fp
}

func qualityProtocolForOperation(operation audit.Operation) string {
	switch operation {
	case audit.OperationChat:
		return qualityProtocolChat
	case audit.OperationMessages:
		return qualityProtocolAnthropic
	default:
		return qualityProtocolResponses
	}
}

// semanticOutputOnly 报告"仅语义输出"形态：流有工具调用等非文本输出，
// 没有任何文本、思考或聚合内容。这样的流不是空流；终态走
// emptyStreamVerdict（思考期望内扣留，未期望则放行）。
func (s *qualityScanState) semanticOutputOnly() bool {
	return s.semanticOutput && s.visibleRunes == 0 && s.aggregateRunes == 0 && !s.hasThinking
}

// emptyEvidence 报告"零思考证据且零可见文本"形态。它是三个判决收口点
// （流内终态短路 / EOF 收尾 / 非流式完整 body）共用的空流判定前提——
// usage 声明（含声称 reasoning tokens）不得把该形态洗成"有内容"。
// 推理阶段已闭合却零增量（reasoningEndedWithoutThinking）不算空证据：
// 那是最强降智签名（EOF 补齐的末行同样携带它），必须走扣留而不是空流。
func (s *qualityScanState) emptyEvidence() bool {
	return !s.hasThinking && !s.reasoningEndedWithoutThinking && s.visibleRunes == 0 && s.aggregateRunes == 0
}

// emptyStreamVerdict 是空证据形态的终态判决:纯语义输出（工具调用等）
// 不是空流——不按空流冷却惩罚。思考期望内（推理模型的正常请求）的
// 纯语义输出是降智账号的裸工具调用形态（零思考零文本，整包末尾
// flush），按 missing-thinking 扣留换号重试；未期望思考（effort=none
// 等合法零思考请求）或无请求语义的调用方（探针）保持放行。
// 其余为空流（可重试的空闲路径）。
func (s *qualityScanState) emptyStreamVerdict(reasoningExpected bool) (QualityVerdict, error) {
	if s.semanticOutputOnly() {
		if reasoningExpected {
			return QualityWithhold, nil
		}
		return QualityDeliver, nil
	}
	return QualityWait, errQualityEmptyStream
}

func (s *qualityScanState) signals() QualityStreamSignals {
	visibleRunes := max(s.visibleRunes, s.aggregateRunes)
	visible := int64((visibleRunes + 3) / 4)
	// usage 声明仅在流本身已有内容/推理证据时才补充 output 估计：零内容
	// 零推理的流（含 usage 帧先于 terminal 到达的合并形态）不能靠 usage
	// 声明变成"有输出"——那会把 R5 空流误判成可扣留流（外部复核发现）。
	if s.usage.Reported && (visibleRunes > 0 || s.hasThinking) {
		fromUsage := s.usage.OutputTokens - s.usage.ReasoningTokens
		if fromUsage > visible {
			visible = fromUsage
		}
	}
	var output int64
	if visibleRunes > 0 || s.hasThinking {
		output = max(0, s.usage.OutputTokens)
	}
	// Thinking evidence is stream events only: non-empty reasoning text deltas
	// across all three protocols. A degraded upstream opens the reasoning item
	// but never streams reasoning text, while usage still claims reasoning tokens;
	// treating item headers, SSE comments, or the usage claim as evidence
	// delivered those streams to clients (observed live).
	// usage.ReasoningTokens stays recorded for audit but never flips the
	// verdict.
	return QualityStreamSignals{
		HasThinking:                   s.hasThinking,
		ReasoningEndedWithoutThinking: s.reasoningEndedWithoutThinking,
		VisibleTokens:                 visible,
		ReasoningTokens:               max(0, s.usage.ReasoningTokens),
		OutputTokens:                  output,
		Terminal:                      s.terminal,
	}
}

type qualityChatChoice struct {
	Index int `json:"index"`
	Delta struct {
		Content          string `json:"content"`
		Reasoning        string `json:"reasoning"`
		ReasoningContent string `json:"reasoning_content"`
		ThinkingContent  string `json:"thinking_content"`
		Refusal          string `json:"refusal"`
		ToolCalls        []any  `json:"tool_calls"`
		FunctionCall     any    `json:"function_call"`
	} `json:"delta"`
	FinishReason string `json:"finish_reason"`
}

// qualityChatEvent / qualityResponsesEvent / qualityAnthropicEvent 是三个协议
// 的帧事件形状。命名并挂在 scan state 上作为 scratch 复用：json.Unmarshal
// 会向既有切片追加，逐帧解码不必重新分配 Choices 等背衬数组。
type qualityChatEvent struct {
	ID      string              `json:"id"`
	Model   string              `json:"model"`
	Choices []qualityChatChoice `json:"choices"`
	Usage   *struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
}

// qualityReasoningItem 覆盖终态聚合输出的观测面（type + content）。注意
// encrypted_content 不在观测面内：实测降智流（RSC risk）与
// clean 流都携带密文，它对降智判定毫无判别力，不是思考证据。
type qualityReasoningItem struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Content []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

// noteResponsesAggregateOutput 统计终态聚合送达的可见文本（completed.output
// 里的 message content），并识别非文本内容块为语义输出。部分上游不发送
// 增量 delta、只在 completed 一次性给出全文：那同样是真实可见输出。
func noteResponsesAggregateOutput(state *qualityScanState, item qualityReasoningItem) int {
	if !strings.EqualFold(strings.TrimSpace(item.Type), "message") {
		if strings.TrimSpace(item.Type) != "" && !strings.EqualFold(strings.TrimSpace(item.Type), "reasoning") {
			// Function/shell/MCP 等调用 item 是有意义的输出，即使上游省略
			// usage 与参数增量事件。
			state.semanticOutput = true
		}
		return 0
	}
	visibleRunes := 0
	for _, content := range item.Content {
		text := content.Text
		if text == "" {
			text = content.Refusal
		}
		if text != "" {
			visibleRunes += utf8.RuneCountInString(text)
			continue
		}
		if content.Type != "" && content.Type != "output_text" && content.Type != "refusal" {
			state.semanticOutput = true
		}
	}
	if visibleRunes > 0 {
		state.semanticOutput = true
	}
	return visibleRunes
}

type qualityResponsesEvent struct {
	Type     string               `json:"type"`
	Item     qualityReasoningItem `json:"item"`
	Response *struct {
		ID     string                 `json:"id"`
		Status string                 `json:"status"`
		Model  string                 `json:"model"`
		Output []qualityReasoningItem `json:"output"`
		Usage  *struct {
			OutputTokens        int64 `json:"output_tokens"`
			InputTokens         int64 `json:"input_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			CostInUSDTicks      int64 `json:"cost_in_usd_ticks"`
			OutputTokensDetails struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	} `json:"response"`
}

type qualityAnthropicEvent struct {
	Type         string `json:"type"`
	ContentBlock struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`

	Usage *struct {
		OutputTokens        int64 `json:"output_tokens"`
		OutputTokensDetails struct {
			ThinkingTokens int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// scanQualityLines scans only newly arrived bytes for line endings. data may
// include an unfinished line whose first searched bytes end at searched. The
// caller owns the backing storage and retains data[consumed:] for the next read.
// cfg enables immediate return at the first decisive event; nil collects a trace.
func scanQualityLines(state *qualityScanState, data []byte, searched int, cfg *QualityRetryRuntime) (consumed int, verdict QualityVerdict, err error) {
	for len(data) > 0 {
		end := bytes.IndexByte(data[searched:], '\n')
		if end < 0 {
			break
		}
		end += searched
		observeQualityLine(state, data[:end])
		data = data[end+1:]
		consumed += end + 1
		searched = 0
		if cfg != nil {
			verdict, err = state.streamVerdict(cfg.ReasoningExpected)
			if verdict != QualityWait || err != nil {
				return
			}
		}
	}
	return consumed, QualityWait, nil
}

func observeQualityLine(state *qualityScanState, line []byte) {
	line = bytes.TrimSpace(line)
	if state.protocol == qualityProtocolAnthropic && bytes.Equal(line, []byte(provider.ThinkingEvidenceComment)) {
		state.hasThinking = true
		return
	}
	if !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	state.sawDataEvent = true
	payload := bytes.TrimSpace(line[len("data:"):])
	if bytes.Equal(payload, []byte("[DONE]")) {
		state.terminal = true
		return
	}
	observeQualityPayload(state, payload)
}

func (s *qualityScanState) streamVerdict(reasoningExpected bool) (QualityVerdict, error) {
	if s.protocolErr != nil {
		return QualityWait, s.protocolErr
	}
	if s.terminal && s.emptyEvidence() {
		return s.emptyStreamVerdict(reasoningExpected)
	}
	cfg := QualityRetryRuntime{}
	cfg.SetKernel(s.kernel)
	return classifyQualityHold(cfg, s.signals()), nil
}

func observeQualityPayload(state *qualityScanState, payload []byte) {
	// A partial or malformed JSON frame cannot establish thinking evidence.
	if !jsonpeek.Valid(payload) {
		return
	}
	noteQualityEncrypted(state, payload)
	switch state.protocol {
	case qualityProtocolChat:
		observeQualityChat(state, payload)
	case qualityProtocolAnthropic:
		observeQualityAnthropic(state, payload)
	default:
		observeQualityResponses(state, payload)
	}
	state.usage = boundedQualityUsage(state.usage)
}

func observeQualityChat(state *qualityScanState, payload []byte) {
	var choices []byte
	var responseID string
	decoded := false
	jsonpeek.ObjectFields(payload, func(key, value []byte) bool {
		switch string(key) {
		case "usage":
			decoded = true
		case "choices":
			choices = value
		case "id":
			if state.responseID == "" {
				responseID = string(jsonpeek.UnquoteBytes(value))
			}
		}
		return true
	})
	if !singleQualityChoice(choices) {
		state.protocolErr = errQualityChoices
		return
	}
	if decoded {
		observeQualityChatDecoded(state, payload)
		return
	}
	// Stage observations until the whole event is inspected. Repeated fields
	// need the decoder's replace/merge semantics; an earlier value must not
	// become evidence when a later value overwrites it.
	var thinking, semantic, terminal bool
	visibleRunes := 0
	jsonpeek.ArrayValues(choices, func(choice []byte) bool {
		var choiceFields uint8
		jsonpeek.ObjectFields(choice, func(key, value []byte) bool {
			switch string(key) {
			case "finish_reason":
				decoded = decoded || choiceFields&1 != 0
				choiceFields |= 1
				if len(jsonpeek.UnquoteBytes(value)) > 0 {
					terminal = true
				}
			case "delta":
				decoded = decoded || choiceFields&2 != 0
				choiceFields |= 2
				var deltaFields uint8
				jsonpeek.ObjectFields(value, func(key, value []byte) bool {
					var field uint8
					switch string(key) {
					case "reasoning":
						field = 1
						thinking = thinking || hasVisibleThinkingBytes(jsonpeek.UnquoteBytes(value))
					case "reasoning_content":
						field = 2
						thinking = thinking || hasVisibleThinkingBytes(jsonpeek.UnquoteBytes(value))
					case "thinking_content":
						field = 4
						thinking = thinking || hasVisibleThinkingBytes(jsonpeek.UnquoteBytes(value))
					case "content":
						field = 8
						visibleRunes += utf8.RuneCount(jsonpeek.UnquoteBytes(value))
					case "refusal":
						field = 64
						visibleRunes += utf8.RuneCount(jsonpeek.UnquoteBytes(value))
					case "tool_calls":
						field = 16
						jsonpeek.ArrayValues(value, func([]byte) bool { semantic = true; return false })
					case "function_call":
						field = 32
						if len(value) > 0 && value[0] == '{' {
							semantic = true
						}
					}
					decoded = decoded || deltaFields&field != 0
					deltaFields |= field
					return !decoded
				})
			}
			return !decoded
		})
		return !decoded
	})
	if decoded {
		observeQualityChatDecoded(state, payload)
		return
	}
	noteQualityEvent(state, "chat.chunk")
	if state.responseID == "" {
		state.responseID = responseID
	}
	state.hasThinking = state.hasThinking || thinking
	state.semanticOutput = state.semanticOutput || semantic
	state.visibleRunes += visibleRunes
	if terminal {
		state.terminal = true
		noteQualityEvent(state, "chat.finish")
	}
}

func observeQualityChatDecoded(state *qualityScanState, payload []byte) {
	event := &state.chatEvent
	// json.Unmarshal reuses slice backing WITHOUT resetting fields absent
	// from the new frame: clear each element first or stale content/reasoning
	// from the previous frame is resurrected and double-counted (P1).
	for i := range event.Choices {
		event.Choices[i] = qualityChatChoice{}
	}
	*event = qualityChatEvent{Choices: event.Choices[:0]}
	if json.Unmarshal(payload, event) != nil {
		return
	}
	if len(event.Choices) > 1 || len(event.Choices) == 1 && event.Choices[0].Index != 0 {
		state.protocolErr = errQualityChoices
		return
	}
	noteQualityEvent(state, "chat.chunk")
	if state.responseID == "" {
		state.responseID = event.ID
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.InputTokens = event.Usage.PromptTokens
		state.usage.OutputTokens = event.Usage.CompletionTokens
		state.usage.ReasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
		state.usage.TotalTokens = event.Usage.TotalTokens
		state.usage.ResponseModel = event.Model
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if hasVisibleThinkingText(delta.Reasoning) || hasVisibleThinkingText(delta.ReasoningContent) || hasVisibleThinkingText(delta.ThinkingContent) {
			state.hasThinking = true
		}
		// Refusal is visible response content, as in native Responses JSON/SSE.
		if delta.Content != "" {
			noteVisibleContent(state, delta.Content)
		}
		if delta.Refusal != "" {
			noteVisibleContent(state, delta.Refusal)
		}
		// 工具调用参数是语义输出：纯 tool_calls 流不是空流；终态走
		// emptyStreamVerdict（思考期望内扣留）。
		if len(delta.ToolCalls) > 0 || delta.FunctionCall != nil {
			state.semanticOutput = true
		}
		if choice.FinishReason != "" {
			state.terminal = true
			noteQualityEvent(state, "chat.finish")
		}
	}
}
func observeQualityResponses(state *qualityScanState, payload []byte) {
	// 高频帧与 item.added / reasoning item.done 只取 type+item 字段。
	// completed 与「无增量的 message item.done」仍走完整解码（usage / 聚合正文）。
	var typ string
	var delta, item []byte
	jsonpeek.ObjectFields(payload, func(key, value []byte) bool {
		switch string(key) {
		case "type":
			typ = jsonpeek.InternType(jsonpeek.UnquoteBytes(value))
		case "delta":
			delta = value
		case "item":
			item = value
		}
		return true
	})
	switch typ {
	case "response.created", "response.in_progress",
		"response.reasoning_summary_part.added", "response.content_part.added":
		noteQualityEvent(state, typ)
		if state.responseID == "" {
			if id := jsonpeek.StringField(payload, "id"); id != "" {
				state.responseID = id
			}
		}
		return
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		noteQualityEvent(state, typ)
		if hasVisibleThinkingBytes(jsonpeek.UnquoteBytes(delta)) {
			state.hasThinking = true
		}
		return
	case "response.output_text.delta", "response.refusal.delta":
		noteQualityEvent(state, typ)
		if delta := jsonpeek.UnquoteBytes(delta); len(delta) > 0 {
			noteVisibleContentBytes(state, delta)
		}
		return
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
		noteQualityEvent(state, typ)
		if len(jsonpeek.UnquoteBytes(delta)) > 0 {
			state.semanticOutput = true
		}
		return
	case "response.output_item.added":
		noteQualityEvent(state, typ)
		itemType, _ := qualityItemIdentity(item)
		noteQualityItem(state, itemType)
		switch itemType {
		case "reasoning", "message", "":
		default:
			state.semanticOutput = true
		}
		return
	case "response.output_item.done":
		noteQualityEvent(state, typ)
		itemType, itemID := qualityItemIdentity(item)
		if (itemType == "reasoning" || strings.HasPrefix(itemID, "rs_") || strings.HasPrefix(itemID, "reasoning_")) && !state.hasThinking {
			state.reasoningEndedWithoutThinking = true
		}
		switch itemType {
		case "message":
			// 无增量、只在 item.done 带全文的形态仍走解码吃聚合文本。
			observeQualityResponsesDecoded(state, payload)
		case "reasoning", "":
		default:
			state.semanticOutput = true
		}
		return
	}
	observeQualityResponsesDecoded(state, payload)
}

func qualityItemIdentity(item []byte) (typ, id string) {
	jsonpeek.ObjectFields(item, func(key, value []byte) bool {
		switch string(key) {
		case "type":
			typ = string(jsonpeek.UnquoteBytes(value))
		case "id":
			id = string(jsonpeek.UnquoteBytes(value))
		}
		return true
	})
	return
}

func observeQualityResponsesDecoded(state *qualityScanState, payload []byte) {
	event := &state.responsesEvent
	*event = qualityResponsesEvent{}
	if json.Unmarshal(payload, event) != nil {
		return
	}
	noteQualityEvent(state, event.Type)
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		state.terminal = true
		if event.Type != "response.completed" {
			state.failed = true
		} else if event.Response != nil && (event.Response.Status == "" || event.Response.Status == "completed") {
			state.completed = true
		} else {
			state.failed = true
		}
	case "error":
		state.failed = true
	case "response.output_item.done":
		// Only message aggregates reach this fallback; item signatures and
		// all deltas are classified by the structural parser above.
		state.aggregateRunes = max(state.aggregateRunes, noteResponsesAggregateOutput(state, event.Item))
	}
	if event.Response != nil {
		if state.responseID == "" {
			state.responseID = event.Response.ID
		}
		// response.completed 的 output 数组可能只在末尾携带聚合形式的文本
		// （无增量 delta）——那是真实可见输出；其中的 reasoning 项（含密文）
		// 不是思考证据，跳过。
		aggregateRunes := 0
		for _, item := range event.Response.Output {
			aggregateRunes += noteResponsesAggregateOutput(state, item)
		}
		state.aggregateRunes = max(state.aggregateRunes, aggregateRunes)
		if event.Response.Usage != nil {
			state.usage.Reported = true
			state.usage.InputTokens = event.Response.Usage.InputTokens
			state.usage.OutputTokens = event.Response.Usage.OutputTokens
			state.usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
			state.usage.TotalTokens = event.Response.Usage.TotalTokens
			state.usage.CostInUSDTicks = event.Response.Usage.CostInUSDTicks
			state.usage.ResponseModel = event.Response.Model
		}
	}
}
func observeQualityAnthropic(state *qualityScanState, payload []byte) {
	var typ string
	var delta []byte
	jsonpeek.ObjectFields(payload, func(key, value []byte) bool {
		switch string(key) {
		case "type":
			typ = jsonpeek.InternType(jsonpeek.UnquoteBytes(value))
		case "delta":
			delta = value
		}
		return true
	})
	switch typ {
	case "message_stop":
		noteQualityEvent(state, typ)
		state.terminal = true
	case "ping":
		noteQualityEvent(state, typ)
	case "content_block_delta":
		noteQualityEvent(state, typ)
		var kind, thinking, text, partial []byte
		jsonpeek.ObjectFields(delta, func(key, value []byte) bool {
			switch string(key) {
			case "type":
				kind = jsonpeek.UnquoteBytes(value)
			case "thinking":
				thinking = value
			case "text":
				text = value
			case "partial_json":
				partial = value
			}
			return true
		})
		switch string(kind) {
		case "thinking_delta":
			if hasVisibleThinkingBytes(jsonpeek.UnquoteBytes(thinking)) {
				state.hasThinking = true
			}
		case "text_delta":
			noteVisibleContentBytes(state, jsonpeek.UnquoteBytes(text))
		case "input_json_delta":
			if len(jsonpeek.UnquoteBytes(partial)) > 0 {
				state.semanticOutput = true
			}
		}
	default:
		observeQualityAnthropicDecoded(state, payload)
	}
}

func observeQualityAnthropicDecoded(state *qualityScanState, payload []byte) {
	event := &state.anthropicEvent
	*event = qualityAnthropicEvent{}
	if json.Unmarshal(payload, event) != nil {
		return
	}
	noteQualityEvent(state, event.Type)
	switch event.Type {
	case "content_block_start":
		noteQualityItem(state, event.ContentBlock.Type)
		// A thinking block start alone is not delivered thinking; only a
		// non-empty thinking_delta below proves streamed thinking content.
		if event.ContentBlock.Type == "thinking" {
			state.anthropicThinkingStarted = true
		} else if event.ContentBlock.Type == "redacted_thinking" {
			// redacted_thinking 是加密思考块（Anthropic 对 encrypted_content 的
			// 表达）：与密文同理不构成思考证据，也不是语义输出——只有它而没有
			// 可见思考增量的流必须维持待判/扣留，不得被标成 semanticOutput
			// 而绕过规则 2。
		} else if event.ContentBlock.Type != "" && event.ContentBlock.Type != "text" {
			// tool_use 等非文本内容块是语义输出。
			state.semanticOutput = true
		}
	case "content_block_stop":
		// 零延迟拦截（规则 2 的 Messages 形态）：thinking 块闭合（停止事件
		// 不携带类型，靠 anthropicThinkingStarted 状态识别）而未产出任何
		// thinking_delta——降智流在此 0ms 判定 QualityWithhold。
		if state.anthropicThinkingStarted && !state.hasThinking {
			state.reasoningEndedWithoutThinking = true
		}
		if event.ContentBlock.Type == "text" && event.ContentBlock.Text != "" {
			// 部分 Messages 兼容上游只在 block 结束时给出全文（聚合送达）。
			noteVisibleContent(state, event.ContentBlock.Text)
			state.semanticOutput = true
		}

	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.OutputTokens = event.Usage.OutputTokens
		state.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		state.usage.TotalTokens = event.Usage.OutputTokens
		state.usage.ResponseModel = "anthropic"
	}
}

// hasVisibleThinkingText 报告思考增量是否携带非空白内容。生产抓流实证：
// 健康 summary 尾部会补发只含换行的增量，降智流的空推理项在末尾整包
// flush 时也可能只携带空白增量。空白不含可见思考、不产生 reasoning
// token（usage 结算为 0），作为证据会在规则 1 瞬间放行整包降智响应
// （terminal_burst 事故形态：零思考答案末尾一次交付且守卫零计数）。
// 零宽/格式字符（U+200B、U+FEFF 等 Cf 类）同样不可见，一并排除。
func hasVisibleThinkingText(delta string) bool {
	return hasVisibleThinkingBytes([]byte(delta))
}

func hasVisibleThinkingBytes(delta []byte) bool {
	for i := 0; i < len(delta); {
		r, n := utf8.DecodeRune(delta[i:])
		i += n
		if !unicode.IsSpace(r) && !unicode.IsControl(r) && !unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

func noteVisibleContent(state *qualityScanState, text string) {
	noteVisibleContentBytes(state, []byte(text))
}

func noteVisibleContentBytes(state *qualityScanState, text []byte) {
	if len(text) == 0 {
		return
	}
	state.visibleRunes += utf8.RuneCount(text)
}
