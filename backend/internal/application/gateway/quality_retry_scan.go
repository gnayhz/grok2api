package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
)

const (
	qualityProtocolChat       = "chat"
	qualityProtocolResponses  = "responses"
	qualityProtocolAnthropic  = "anthropic"
	qualityHoldMaxBufferBytes = 4 << 20
	// qualityBodyPeekLimit 是非流式判决的内存上限:流式路径扣留缓冲有 4MiB 界,
	// 非流式此前无界。取 32MiB——足够容纳任何合法的完整 JSON 响应, 超过即视为
	// 异常形态, 放弃判决直接透传。
	qualityBodyPeekLimit = 32 << 20
	// 注：chat 流转换器在 reasoning item 打开时发出的时序注释
	// ": grok2api-reasoning-start"（见 chat_stream.go markChatReasoningStart）
	// 不是思考证据——降智流同样会发；扫描器按 SSE 注释行（非 data: 前缀）
	// 统一跳过，语义见 ObserveQualityChunk 行循环内的 NOTE。
)

type qualityScanState struct {
	protocol     string
	pending      []byte
	hasThinking  bool
	visibleRunes int
	// aggregateRunes 记录仅在终态聚合形式到达的文本（completed.output[].
	// content[].text，无增量 delta）：同样是真实可见输出，计入可见估计。
	aggregateRunes int
	// semanticOutput 表示流携带非文本的语义输出（工具调用参数、非文本
	// 内容块）：既不是空流，也不适合按 token 阈值扣留——直接交付。
	semanticOutput  bool
	reasoningTokens int64
	outputTokens    int64
	usage           Usage
	responseID      string
	terminal        bool
	// oversizedLine 保留给决策表/旧测试：扫描器不再用它 fail-open。
	oversizedLine bool
	// skipUntilNewline 丢弃当前超长 SSE 行的剩余分片（换行到达前的
	// encrypted_content 等），避免 1MiB 未完成行把降智流 fail-open。
	skipUntilNewline bool
	// sawDataEvent 标记已解析到至少一个 SSE data 事件（任意类型）。供首事件
	// 截止使用：keepalive 注释不算——降智排队期间上游只发注释或零字节。
	sawDataEvent bool
	// 逐帧解码 scratch：事件结构体挂在 state 上复用（见类型注释）。
	chatEvent      qualityChatEvent
	responsesEvent qualityResponsesEvent
	anthropicEvent qualityAnthropicEvent
}

type qualityReadResult struct {
	data []byte
	err  error
}

// qualityReadPump is the sole reader of the upstream body. It lets the hold
// timer win while an upstream Read is blocked, then remains the continuation
// reader for the response body after the held prefix is replayed.
type qualityReadPump struct {
	source    io.ReadCloser
	results   chan qualityReadResult
	done      chan struct{}
	closeOnce sync.Once
	pending   []byte
	finalErr  error
}

func newQualityReadPump(source io.ReadCloser) *qualityReadPump {
	pump := &qualityReadPump{
		source:  source,
		results: make(chan qualityReadResult),
		done:    make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *qualityReadPump) run() {
	defer close(p.results)
	buf := make([]byte, 4096)
	for {
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		result := qualityReadResult{err: err}
		if n > 0 {
			result.data = append([]byte(nil), buf[:n]...)
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *qualityReadPump) Read(dst []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.finalErr != nil {
			return 0, p.finalErr
		}
		result, ok := <-p.results
		if !ok {
			p.finalErr = io.EOF
			return 0, io.EOF
		}
		p.pending = result.data
		p.finalErr = result.err
		if len(p.pending) == 0 && p.finalErr != nil {
			return 0, p.finalErr
		}
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *qualityReadPump) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.source.Close()
	})
	return err
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
// 没有任何文本、思考或聚合内容。这样的流既不是空流也不适合按 token
// 阈值扣留——调用决策的重放收益低而不确定性非零，直接交付。
func (s *qualityScanState) semanticOutputOnly() bool {
	return s.semanticOutput && s.visibleRunes == 0 && s.aggregateRunes == 0 && !s.hasThinking
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
	output := s.outputTokens
	if s.usage.Reported && (visibleRunes > 0 || s.hasThinking) && s.usage.OutputTokens > output {
		output = s.usage.OutputTokens
	}
	// Thinking evidence is stream events only: non-empty reasoning text deltas
	// across all three protocols. A degraded upstream opens the reasoning item
	// (and the converter emits the SSE reasoning-start comment) but never
	// streams reasoning text, while usage still claims reasoning tokens;
	// treating item headers, the marker, or the usage claim as evidence
	// delivered those streams to clients (observed live).
	// usage.ReasoningTokens stays recorded for audit but never flips the
	// verdict.
	return QualityStreamSignals{
		HasThinking:     s.hasThinking,
		VisibleTokens:   visible,
		ReasoningTokens: max(s.reasoningTokens, s.usage.ReasoningTokens),
		OutputTokens:    output,
		Terminal:        s.terminal,
		OversizedLine:   s.oversizedLine,
	}
}

type qualityChatChoice struct {
	Delta struct {
		Content          string `json:"content"`
		Reasoning        string `json:"reasoning"`
		ReasoningContent string `json:"reasoning_content"`
		ThinkingContent  string `json:"thinking_content"`
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
// encrypted_content 不在观测面内：2026-08-20 实测降智流（RSC risk）与
// clean 流都携带密文，它对降智判定毫无判别力，不是思考证据。
type qualityReasoningItem struct {
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
	Delta    string               `json:"delta"`
	Item     qualityReasoningItem `json:"item"`
	Response *struct {
		ID     string                 `json:"id"`
		Model  string                 `json:"model"`
		Output []qualityReasoningItem `json:"output"`
		Usage  *struct {
			OutputTokens        int64 `json:"output_tokens"`
			InputTokens         int64 `json:"input_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
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
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens        int64 `json:"output_tokens"`
		OutputTokensDetails struct {
			ThinkingTokens int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// ObserveQualityChunk feeds one SSE chunk into the hold classifier state.
// This is the shipped scanner used by peekQualityStream.
func ObserveQualityChunk(state *qualityScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	state.pending = append(state.pending, chunk...)
	for {
		if state.skipUntilNewline {
			index := bytes.IndexByte(state.pending, '\n')
			if index < 0 {
				state.pending = nil
				return
			}
			state.pending = state.pending[index+1:]
			state.skipUntilNewline = false
			continue
		}
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > 1<<20 {
				// 换行前已超过 1MiB：生产 grok-4.6 xhigh 降智是
				// encrypted_content 单行。丢弃本行剩余分片并继续扫后续事件。
				classifyOversizedQualityLine(state, state.pending)
				state.skipUntilNewline = true
				state.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 {
			continue
		}
		// NOTE: SSE comments（如转换器发出的 ": grok2api-reasoning-start"）
		// 不构成思考证据——降智流同样会发。仅有可见的推理文本增量
		// （reasoning_text / reasoning_summary_text.delta 等）证明思考。
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		state.sawDataEvent = true
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			state.terminal = true
			continue
		}
		observeQualityPayload(state, payload)
	}
}

// classifyOversizedQualityLine 处理换行前已超过 1MiB 的 SSE 前缀。
// 生产 2026-08-27：grok-4.6 xhigh 降智在 output_item.done 上推数 MiB
// encrypted_content、零可见思考；旧 fail-open 把后续答案原样交给客户端。
// 思考增量不会到 1MiB，超长行按类型分流后丢掉本行。
func classifyOversizedQualityLine(state *qualityScanState, line []byte) {
	if state == nil || len(line) == 0 {
		return
	}
	if bytes.Contains(line, []byte("encrypted_content")) {
		return
	}
	if bytes.Contains(line, []byte("reasoning_text.delta")) ||
		bytes.Contains(line, []byte("reasoning_summary_text.delta")) ||
		bytes.Contains(line, []byte("thinking_delta")) ||
		bytes.Contains(line, []byte("reasoning_content")) {
		state.hasThinking = true
		return
	}
	if bytes.Contains(line, []byte("output_text.delta")) {
		state.visibleRunes += 32
	}
}

func observeQualityPayload(state *qualityScanState, payload []byte) {
	switch state.protocol {
	case qualityProtocolChat:
		observeQualityChat(state, payload)
	case qualityProtocolAnthropic:
		observeQualityAnthropic(state, payload)
	default:
		observeQualityResponses(state, payload)
	}
}

func observeQualityChat(state *qualityScanState, payload []byte) {
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
		state.outputTokens = event.Usage.CompletionTokens
		state.reasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if delta.Reasoning != "" || delta.ReasoningContent != "" || delta.ThinkingContent != "" {
			state.hasThinking = true
		}
		if delta.Content != "" {
			noteVisibleContent(state, delta.Content)
		}
		// 工具调用参数是语义输出：纯 tool_calls 流不是空流，也不按 token
		// 阈值扣留（调用决策的重放收益低、不确定性非零，直接交付）。
		if len(delta.ToolCalls) > 0 || delta.FunctionCall != nil {
			state.semanticOutput = true
		}
		if choice.FinishReason != "" {
			state.terminal = true
		}
	}
}
func observeQualityResponses(state *qualityScanState, payload []byte) {
	event := &state.responsesEvent
	*event = qualityResponsesEvent{}
	if json.Unmarshal(payload, event) != nil {
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		state.terminal = true
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if event.Delta != "" {
			state.hasThinking = true
		}
	case "response.output_item.added", "response.output_item.done":
		// A reasoning item header alone is not delivered thinking: degraded
		// streams open the item (and even deliver encrypted_content ciphertext
		// on item.done) while never emitting visible reasoning text deltas.
	case "response.output_text.delta":
		if event.Delta != "" {
			noteVisibleContent(state, event.Delta)
		}
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
		if event.Delta != "" {
			state.semanticOutput = true
		}
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
			state.usage.ResponseModel = event.Response.Model
			state.outputTokens = event.Response.Usage.OutputTokens
			state.reasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
}
func observeQualityAnthropic(state *qualityScanState, payload []byte) {
	event := &state.anthropicEvent
	*event = qualityAnthropicEvent{}
	if json.Unmarshal(payload, event) != nil {
		return
	}
	switch event.Type {
	case "message_stop":
		state.terminal = true
	case "content_block_start":
		// A thinking block start alone is not delivered thinking; only a
		// non-empty thinking_delta below proves streamed thinking content.
		if event.ContentBlock.Type != "" && event.ContentBlock.Type != "text" && event.ContentBlock.Type != "thinking" {
			// tool_use 等非文本内容块是语义输出。
			state.semanticOutput = true
		}
	case "content_block_stop":
		if event.ContentBlock.Type == "text" && event.ContentBlock.Text != "" {
			// 部分 Messages 兼容上游只在 block 结束时给出全文（聚合送达）。
			noteVisibleContent(state, event.ContentBlock.Text)
			state.semanticOutput = true
		}
	case "content_block_delta":
		if event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "" {
			state.hasThinking = true
		}
		// signature_delta（Messages 对 encrypted_content 的表达）不是思考
		// 证据：与 Responses 密文同理，降智流同样携带签名。
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			noteVisibleContent(state, event.Delta.Text)
		}
		if event.Delta.Type == "input_json_delta" && event.Delta.PartialJSON != "" {
			// 工具调用参数增量是语义输出。
			state.semanticOutput = true
		}
	}
	if event.Usage != nil {
		state.usage.Reported = true
		state.usage.OutputTokens = event.Usage.OutputTokens
		state.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
		state.usage.TotalTokens = event.Usage.OutputTokens
		state.usage.ResponseModel = "anthropic"
		state.outputTokens = event.Usage.OutputTokens
		state.reasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
	}
}
func noteVisibleContent(state *qualityScanState, text string) {
	if text == "" {
		return
	}
	state.visibleRunes += utf8.RuneCountInString(text)
}

func peekQualityStream(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, string, error) {
	cfg = normalizeQualityRetry(cfg)
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, "", errQualityEmptyStream
	}
	pump := newQualityReadPump(body)
	state := qualityScanState{protocol: protocol}
	var held bytes.Buffer
	holdTimer := time.NewTimer(cfg.HoldTimeout)
	defer holdTimer.Stop()
	// 零证据截止：复杂提示词的降智流首输出静默期实测 75-121s（2026-08-21
	// 魔法球实测），干净流首思考增量 2.1s 即达。截止内无证据且无输出即中止
	// 该次尝试（走空闲路径重试），把降智流式尝试从“完整静默期”压到预算内。
	evidenceTimer := time.NewTimer(cfg.EvidenceTimeout)
	defer evidenceTimer.Stop()
	// 首事件截止：直连复测证实降智排队期间上游零 data 事件（仅 keepalive
	// 注释或零字节，response.created 等 68-125s），clean 恒定 0.8-2.2s。
	// 比证据截止更早一档（5s vs 15s），把降智尝试成本再压低 2/3。
	createdTimer := time.NewTimer(cfg.CreatedTimeout)
	defer createdTimer.Stop()
	// holdExpired 粘性：timer 只触发一次，之后到达的小输出也必须按“hold
	// 已超时”立即扣留，而不是退回 Wait 等待 EOF——否则慢速降智流会把首
	// 字节无限推迟到流关闭。
	holdExpired := false
	for {
		// 空流短路（与 finishQualityPeek 同语义）：terminal 已到而零内容零
		// 推理时，usage 声明（含 output_tokens>0 的形式）不得把空流洗成可
		// 扣留的“有内容”流——那会走扣留重试而不是空流冷却路径。
		// 纯语义输出（工具调用等）不是空流：交付，不按空流冷却惩罚。
		// oversizedLine 时跳过：超长行证据不可靠应 fail-open，而非空流。
		if state.terminal && !state.hasThinking && state.visibleRunes == 0 && state.aggregateRunes == 0 && !state.oversizedLine {
			if state.semanticOutputOnly() {
				return newPrefixReplay(&held, pump), QualityDeliver, state.usage, state.responseID, nil
			}
			return newPrefixReplay(&held, pump), QualityWait, state.usage, state.responseID, errQualityEmptyStream
		}
		sig := state.signals()
		sig.HoldExpired = holdExpired
		if verdict := ClassifyQualityHold(sig, cfg.MinOutputTokens); verdict != QualityWait {
			return newPrefixReplay(&held, pump), verdict, state.usage, state.responseID, nil
		}

		select {
		case <-ctx.Done():
			_ = pump.Close()
			return io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, state.responseID, qualityPeekAbortError(ctx, ctx.Err())
		case <-holdTimer.C:
			holdExpired = true
			sig.HoldExpired = true
			if verdict := ClassifyQualityHold(sig, cfg.MinOutputTokens); verdict != QualityWait {
				return newPrefixReplay(&held, pump), verdict, state.usage, state.responseID, nil
			}
		case <-evidenceTimer.C:
			// 截止触发时仍零思考证据、零可见/聚合输出、非语义输出流：该次
			// 尝试按零证据超时中止（服务端走空闲冷却+RSC 归因+重试）。已有
			// 任何输出或证据的流不受影响（证据提前放行/输出达到阈值扣留）。
			if !state.hasThinking && state.visibleRunes == 0 && state.aggregateRunes == 0 && !state.semanticOutput && !state.oversizedLine {
				_ = pump.Close()
				return io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, state.responseID, errQualityEvidenceTimeout
			}
		case <-createdTimer.C:
			// 首事件截止：连一个 data 事件都没到（keepalive 注释不算）。降智
			// 排队期间的真实形态（直连复测 68-125s 零 data 事件）。中止该次
			// 尝试走空闲路径；任何 data 事件已到达则本截止失效（由证据截止接管）。
			if !state.sawDataEvent {
				_ = pump.Close()
				return io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, state.responseID, errQualityCreatedTimeout
			}
		case result, ok := <-pump.results:
			if !ok {
				return finishQualityPeek(&held, pump, &state, cfg)
			}
			if len(result.data) > 0 {
				if held.Len()+len(result.data) > qualityHoldMaxBufferBytes {
					_, _ = held.Write(result.data)
					ObserveQualityChunk(&state, result.data)
					if state.hasThinking {
						return newPrefixReplay(&held, pump), QualityDeliver, state.usage, state.responseID, nil
					}
					// 无思考证据的超大前缀几乎一定是降智密文堆，扣留而不是放行。
					return newPrefixReplay(&held, pump), QualityWithhold, state.usage, state.responseID, nil
				}
				_, _ = held.Write(result.data)
				ObserveQualityChunk(&state, result.data)
			}
			if result.err == io.EOF {
				return finishQualityPeek(&held, pump, &state, cfg)
			}
			if result.err != nil {
				_ = pump.Close()
				return io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, state.responseID, qualityPeekAbortError(ctx, result.err)
			}
		}
	}
}

func finishQualityPeek(held *bytes.Buffer, pump *qualityReadPump, state *qualityScanState, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, string, error) {
	if state == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, "", errQualityEmptyStream
	}
	if len(state.pending) > 0 {
		// Process a final valid SSE data line even when the upstream omitted its
		// trailing newline.
		ObserveQualityChunk(state, []byte{'\n'})
	}
	state.terminal = true
	signals := state.signals()
	// 空流判定只看流证据：只有 usage 帧（声称 reasoning tokens）而零内容零
	// 推理事件的流同样是空流——usage 声明不能把空 200 洗成可投递响应。
	// 纯语义输出（工具调用等）不是空流：交付，不按空流冷却惩罚。
	// oversizedLine 优先走 fail-open（调用方 ClassifyQualityHold 已处理）。
	if !state.hasThinking && state.visibleRunes == 0 && state.aggregateRunes == 0 && !state.oversizedLine {
		if state.semanticOutput {
			return newPrefixReplay(held, pump), QualityDeliver, state.usage, state.responseID, nil
		}
		return newPrefixReplay(held, pump), QualityWait, state.usage, state.responseID, errQualityEmptyStream
	}
	return newPrefixReplay(held, pump), ClassifyQualityHold(signals, cfg.MinOutputTokens), state.usage, state.responseID, nil
}

func newPrefixReplay(held *bytes.Buffer, rest io.ReadCloser) io.ReadCloser {
	if rest == nil {
		rest = io.NopCloser(bytes.NewReader(nil))
	}
	if held == nil || held.Len() == 0 {
		return rest
	}
	return &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(held.Bytes()), rest), source: rest}
}

// qualityResponseBody 覆盖非流式上游响应（Build/Console 均为 Responses 形状）
// 的观测面。summary/content 的可见思考文本与流式扫描器同语义；
// encrypted_content 与 usage 声明不构成证据（2026-08-20 实测：降智响应
// 密文与非零 reasoning_tokens 照常存在，无判别力）。
type qualityResponseBody struct {
	ID     string `json:"id"`
	Model  string `json:"model"`
	Output []struct {
		Type    string `json:"type"`
		Summary []struct {
			Text string `json:"text"`
		} `json:"summary"`
		Content []struct {
			Type    string `json:"type"`
			Text    string `json:"text"`
			Refusal string `json:"refusal"`
		} `json:"content"`
	} `json:"output"`
	Usage *struct {
		OutputTokens        int64 `json:"output_tokens"`
		InputTokens         int64 `json:"input_tokens"`
		TotalTokens         int64 `json:"total_tokens"`
		OutputTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
}

// chainedBody 把 Close 传导给底层 body 的重放 Reader, 用于需要继续透传
// 原始 body 的返回路径。
type chainedBody struct {
	Reader io.Reader
	closer io.Closer
}

func (c *chainedBody) Read(p []byte) (int, error) { return c.Reader.Read(p) }
func (c *chainedBody) Close() error               { return c.closer.Close() }

// peekQualityBody 非流式响应的完整 body 判决。客户端本就要等完整 JSON 才能
// 收到任何字节，读完再判的扣留附加延迟为零：不存在流式路径的时序复杂度
// （无 hold 窗口/阈值/早断/前缀重放）。证据规则与流式扫描器一致：可见思考
// 文本是唯一健康证据。无法识别的响应形状（无 output 数组的合法 JSON）
// fail-open 放行，与 oversizedLine 同语义——不猜测质量。
func peekQualityBody(body io.ReadCloser, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, string, error) {
	cfg = normalizeQualityRetry(cfg)
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, "", errQualityEmptyStream
	}
	data, readErr := io.ReadAll(io.LimitReader(body, qualityBodyPeekLimit+1))
	if int64(len(data)) > qualityBodyPeekLimit {
		// 超预算的响应不再判决:重放已读字节并继续透传剩余 body(fail-open, 与
		// "无法识别的响应形状"同语义——不猜测质量)。此前 io.ReadAll 无上限, 异常/
		// 被劫持上游的超大 200 body 会在判决前全量驻留内存, 并发下 OOM。
		// Close 必须传导给原始 body: Build 路径的 egressResponseBody.Close 同时
		// 释放上游连接与 egress 租约, NopCloser 会两者都泄漏。
		return &chainedBody{Reader: io.MultiReader(bytes.NewReader(data), body), closer: body}, QualityDeliver, Usage{}, "", nil
	}
	_ = body.Close()
	replay := io.NopCloser(bytes.NewReader(data))
	if readErr != nil {
		return replay, QualityWait, Usage{}, "", readErr
	}
	var parsed qualityResponseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		// 200 但 body 非合法 JSON：按空流处理（可重试），不猜质量。
		return replay, QualityWait, Usage{}, "", errQualityEmptyStream
	}
	if parsed.Output == nil {
		// 合法 JSON 但非 Responses 形状：fail-open，不猜质量。
		return replay, QualityDeliver, Usage{}, parsed.ID, nil
	}
	state := qualityScanState{protocol: qualityProtocolResponses}
	for _, item := range parsed.Output {
		itemType := strings.ToLower(strings.TrimSpace(item.Type))
		switch itemType {
		case "reasoning":
			// 与流式一致：reasoning item 的可见文本（summary/内容）是思考
			// 证据；仅携带密文的 reasoning item 不是。
			for _, summary := range item.Summary {
				if strings.TrimSpace(summary.Text) != "" {
					state.hasThinking = true
				}
			}
			for _, content := range item.Content {
				if strings.TrimSpace(content.Text) != "" {
					state.hasThinking = true
				}
			}
		case "message":
			for _, content := range item.Content {
				text := content.Text
				if text == "" {
					text = content.Refusal
				}
				if text != "" {
					state.aggregateRunes += utf8.RuneCountInString(text)
					continue
				}
				if content.Type != "" && content.Type != "output_text" && content.Type != "refusal" {
					state.semanticOutput = true
				}
			}
		default:
			if itemType != "" {
				// function_call / web_search_call 等调用 item 是语义输出。
				state.semanticOutput = true
			}
		}
	}
	usage := Usage{}
	if parsed.Usage != nil {
		usage = Usage{
			Reported:        true,
			InputTokens:     parsed.Usage.InputTokens,
			OutputTokens:    parsed.Usage.OutputTokens,
			ReasoningTokens: parsed.Usage.OutputTokensDetails.ReasoningTokens,
			TotalTokens:     parsed.Usage.TotalTokens,
			ResponseModel:   parsed.Model,
		}
	}
	// 空响应判定与 finishQualityPeek 同语义：零内容零思考且非纯语义输出 = 空流。
	if !state.hasThinking && state.aggregateRunes == 0 {
		if state.semanticOutput {
			return replay, QualityDeliver, usage, parsed.ID, nil
		}
		return replay, QualityWait, usage, parsed.ID, errQualityEmptyStream
	}
	// body 已完整到达：恒为终态证据。
	sig := state.signals()
	sig.Terminal = true
	return replay, ClassifyQualityHold(sig, cfg.MinOutputTokens), usage, parsed.ID, nil
}
