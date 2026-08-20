package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
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
)

type qualityScanState struct {
	protocol        string
	pending         []byte
	hasThinking     bool
	visibleRunes    int
	reasoningTokens int64
	outputTokens    int64
	usage           Usage
	responseID      string
	terminal        bool
	// oversizedLine 标记出现过 >1MiB 的单行：内容无法可靠解析，按与缓冲
	// 上限一致的 fail-open 处理。
	oversizedLine bool
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

func (s *qualityScanState) signals() QualityStreamSignals {
	visible := int64((s.visibleRunes + 3) / 4)
	// usage 声明仅在流本身已有内容/推理证据时才补充 output 估计：零内容
	// 零推理的流（含 usage 帧先于 terminal 到达的合并形态）不能靠 usage
	// 声明变成"有输出"——那会把 R5 空流误判成可扣留流（外部复核发现）。
	if s.usage.Reported && (s.visibleRunes > 0 || s.hasThinking) {
		fromUsage := s.usage.OutputTokens - s.usage.ReasoningTokens
		if fromUsage > visible {
			visible = fromUsage
		}
	}
	output := s.outputTokens
	if s.usage.Reported && (s.visibleRunes > 0 || s.hasThinking) && s.usage.OutputTokens > output {
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

type qualityResponsesEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta"`
	Item  struct {
		ID   string `json:"id"`
		Type string `json:"type"`
	} `json:"item"`
	Response *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
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
	} `json:"content_block"`
	Delta struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
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
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > 1<<20 {
				// 超长单行无法可靠解析：标记 fail-open（与 4MiB 缓冲上限同
				// 语义），而不是丢弃缓冲——丢弃会丢掉这条行内可能存在的推理
				// 证据，把健康流误判为降智。
				state.oversizedLine = true
				state.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 {
			continue
		}
		// NOTE: the ": grok2api-reasoning-start" SSE comment alone must NOT
		// set hasThinking. The converter emits it on response.output_item.added
		// {type:"reasoning"}, and degraded (B-form) streams also open that item
		// while never streaming reasoning text — only non-empty reasoning deltas
		// prove delivered thinking content.
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			state.terminal = true
			continue
		}
		observeQualityPayload(state, payload)
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
	case "response.output_item.added":
		// A reasoning item header alone is not delivered thinking: B-form
		// degraded streams open the item but never emit reasoning text deltas.
	case "response.output_text.delta":
		if event.Delta != "" {
			noteVisibleContent(state, event.Delta)
		}
	}
	if event.Response != nil {
		if state.responseID == "" {
			state.responseID = event.Response.ID
		}
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
	case "content_block_delta":
		if event.Delta.Type == "thinking_delta" && event.Delta.Thinking != "" {
			state.hasThinking = true
		}
		if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
			noteVisibleContent(state, event.Delta.Text)
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
	// holdExpired 粘性：timer 只触发一次，之后到达的小输出也必须按“hold
	// 已超时”立即扣留，而不是退回 Wait 等待 EOF——否则慢速降智流会把首
	// 字节无限推迟到流关闭。
	holdExpired := false
	for {
		// 空流短路（与 finishQualityPeek 同语义）：terminal 已到而零内容零
		// 推理时，usage 声明（含 output_tokens>0 的形式）不得把空流洗成可
		// 扣留的“有内容”流——那会走扣留重试而不是空流冷却路径。
		// oversizedLine 时跳过：超长行证据不可靠应 fail-open，而非空流。
		if state.terminal && !state.hasThinking && state.visibleRunes == 0 && !state.oversizedLine {
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
		case result, ok := <-pump.results:
			if !ok {
				return finishQualityPeek(&held, pump, &state, cfg)
			}
			if len(result.data) > 0 {
				if held.Len()+len(result.data) > qualityHoldMaxBufferBytes {
					_, _ = held.Write(result.data)
					return newPrefixReplay(&held, pump), QualityDeliver, state.usage, state.responseID, nil
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
	// oversizedLine 优先走 fail-open（调用方 ClassifyQualityHold 已处理）。
	if !state.hasThinking && state.visibleRunes == 0 && !state.oversizedLine {
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
