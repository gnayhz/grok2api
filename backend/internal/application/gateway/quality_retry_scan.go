package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

const (
	qualityProtocolChat       = "chat"
	qualityProtocolResponses  = "responses"
	qualityProtocolAnthropic  = "anthropic"
	qualityHoldMaxBufferBytes = 4 << 20
	qualityHugeLineBytes      = 1 << 20
	qualitySkipHeadBytes      = 4096
	qualitySkipTailBytes      = 8192
	qualityReadChunkBytes     = 32 << 10
	// qualityBodyPeekLimit 是非流式判决的内存上限:流式路径扣留缓冲有 4MiB 界,
	// 非流式此前无界。取 32MiB——足够容纳任何合法的完整 JSON 响应, 超过即视为
	// 异常形态, 放弃判决直接透传。
	qualityBodyPeekLimit = 32 << 20
	// 注：SSE 注释行（": " 前缀，含上游 keepalive）不是思考证据——降智
	// 流同样会发；扫描器按非 data: 行统一跳过。历史上转换器发过私有时序
	// 注释 ": grok2api-reasoning-start"，该注入/剥除链路已随私有注释清理
	// （蓝图 #14）整体移除。
)

type qualityScanState struct {
	protocol string
	pending  []byte
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
	semanticOutput  bool
	reasoningTokens int64
	outputTokens    int64
	usage           Usage
	responseID      string
	terminal        bool
	// skipUntilNewline 丢弃当前超长 SSE 行的中间分片（换行到达前的
	// encrypted_content 等），避免 1MiB 未完成行把降智流 fail-open。
	// skipHead/skipTail 保留行首与滚动行尾，这样 2MiB completed 仍能
	// 读到 type 与 usage。
	skipUntilNewline bool
	skipHead         []byte
	skipTail         []byte
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
		Protocol:        s.protocol,
		Verdict:         string(verdict),
		Rule:            qualityHoldRule(sig, err),
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
	// Double-buffer so the consumer can keep result.data without a copy:
	// send is synchronous, and the next Read uses the other backing array.
	bufs := [2][]byte{
		make([]byte, qualityReadChunkBytes),
		make([]byte, qualityReadChunkBytes),
	}
	which := 0
	for {
		buf := bufs[which]
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		result := qualityReadResult{err: err}
		if n > 0 {
			result.data = buf[:n]
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
		which ^= 1
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

func (s *qualityScanState) signals() qualityStreamSignals {
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
	// but never streams reasoning text, while usage still claims reasoning tokens;
	// treating item headers, SSE comments, or the usage claim as evidence
	// delivered those streams to clients (observed live).
	// usage.ReasoningTokens stays recorded for audit but never flips the
	// verdict.
	return qualityStreamSignals{
		HasThinking:                   s.hasThinking,
		ReasoningEndedWithoutThinking: s.reasoningEndedWithoutThinking,
		VisibleTokens:                 visible,
		ReasoningTokens:               max(s.reasoningTokens, s.usage.ReasoningTokens),
		OutputTokens:                  output,
		Terminal:                      s.terminal,
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

// observeQualityChunk feeds one SSE chunk into the hold classifier state.
// This is the shipped scanner used by peekQualityStream.
func observeQualityChunk(state *qualityScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	if cap(state.pending) == 0 {
		state.pending = make([]byte, 0, qualityReadChunkBytes)
	}
	state.pending = append(state.pending, chunk...)
	for {
		if state.skipUntilNewline {
			index := bytes.IndexByte(state.pending, '\n')
			if index < 0 {
				state.skipTail = keepQualityTail(state.skipTail, state.pending, qualitySkipTailBytes)
				state.pending = state.pending[:0]
				return
			}
			state.skipTail = keepQualityTail(state.skipTail, state.pending[:index], qualitySkipTailBytes)
			state.pending = state.pending[index+1:]
			state.skipUntilNewline = false
			observeSkippedQualityLine(state)
			continue
		}
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > qualityHugeLineBytes {
				// 换行前已超过 1MiB：生产 grok-4.6 xhigh 降智是
				// encrypted_content 单行。丢掉中间密文，但保留行首/行尾。
				classifyHugeQualityLine(state, state.pending)
				state.skipHead = append(state.skipHead[:0], state.pending[:min(len(state.pending), qualitySkipHeadBytes)]...)
				state.skipTail = keepQualityTail(state.skipTail[:0], state.pending, qualitySkipTailBytes)
				state.skipUntilNewline = true
				state.pending = state.pending[:0]
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 {
			continue
		}
		// NOTE: SSE 注释行（keepalive 等）不构成思考证据——降智流同样
		// 会发。仅有可见的推理文本增量（reasoning_text /
		// reasoning_summary_text.delta 等）证明思考。唯一例外：
		// ThinkingEvidenceComment 由转换器在「客户端未请求 thinking 的
		// Messages 请求」看到可见思考文本时写入（conversation/
		// stream.go markThinkingEvidence），语义等同 thinking_delta。
		if bytes.HasPrefix(line, []byte(conversation.ThinkingEvidenceComment)) {
			state.hasThinking = true
			continue
		}
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

// classifyHugeQualityLine 处理换行前已超过 1MiB 的 SSE 前缀。
// 生产：grok-4.6 xhigh 降智在 output_item.done 上推数 MiB
// encrypted_content、零可见思考；旧 fail-open 把后续答案原样交给客户端。
// 思考增量不会到 1MiB，超长行按类型分流后丢掉本行。
func classifyHugeQualityLine(state *qualityScanState, line []byte) {
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
	// 三协议的可见正文标记对称覆盖：Responses 的 output_text.delta、Chat 的
	// delta.content、Anthropic 的 text_delta。真实增量不会到 1MiB，这里只是
	// 防御性对称——超长正文行不得因协议差异被静默丢弃成"零输出"。
	if bytes.Contains(line, []byte("output_text.delta")) ||
		bytes.Contains(line, []byte("text_delta")) ||
		bytes.Contains(line, []byte(`"content":"`)) {
		state.visibleRunes += 32
	}
	// 工具调用 item（联网搜索等）在超长行形态下同样是语义输出进行中
	// ——与上方 item.added 的修复同源，防御性对称。item 的 type 字段位于
	// 行首：只扫头部窗口，避免在数 MiB 密文行上增加三次全行扫描
	//（规模轮 5 基准：全行扫描使 256KiB 路径回归 +13-19%）。
	head := line
	if len(head) > 512 {
		head = head[:512]
	}
	if bytes.Contains(head, []byte("web_search_call")) ||
		bytes.Contains(head, []byte("function_call")) ||
		bytes.Contains(head, []byte("mcp_call")) {
		state.semanticOutput = true
	}
}

func keepQualityTail(dst, src []byte, n int) []byte {
	if n <= 0 {
		return dst[:0]
	}
	dst = append(dst, src...)
	if len(dst) <= n {
		return dst
	}
	copy(dst, dst[len(dst)-n:])
	return dst[:n]
}

func observeSkippedQualityLine(state *qualityScanState) {
	if state == nil {
		return
	}
	head := append([]byte(nil), state.skipHead...)
	tail := append([]byte(nil), state.skipTail...)
	state.skipHead = state.skipHead[:0]
	state.skipTail = state.skipTail[:0]
	if len(head) == 0 && len(tail) == 0 {
		return
	}
	if bytes.Contains(head, []byte("data:")) {
		state.sawDataEvent = true
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(head), []byte("data:")))
	payload = append(payload, tail...)
	if len(bytes.TrimSpace(payload)) == 0 {
		return
	}
	observeHugeQualityPayload(state, payload)
}

func observeHugeQualityPayload(state *qualityScanState, payload []byte) {
	if state == nil {
		return
	}
	classifyHugeQualityLine(state, payload)
	head := jsonpeek.Prefix(payload, 4096)
	tail := jsonpeek.Suffix(payload, 8192)
	typ := jsonpeek.StringField(head, "type")
	noteQualityEvent(state, typ)
	noteQualityEncrypted(state, payload)
	switch typ {
	case "response.completed", "response.incomplete", "response.failed", "message_stop":
		state.terminal = true
	case "response.output_item.done":
		// 数 MiB encrypted_content 密文正是以超长 output_item.done 行到达：
		// 零延迟拦截同样适用于超长行形态（保留首尾即可识别类型与 ID）。
		isReasoningItem := bytes.Contains(head, []byte(`"type":"reasoning"`)) ||
			bytes.Contains(head, []byte(`"type": "reasoning"`)) ||
			bytes.Contains(head, []byte(`"id":"rs_`))
		if isReasoningItem && !state.hasThinking {
			state.reasoningEndedWithoutThinking = true
		}
	}
	if jsonpeek.HasKey(head, "finish_reason") || jsonpeek.HasKey(tail, "finish_reason") {
		state.terminal = true
	}
	if id := jsonpeek.StringField(head, "id"); id != "" && state.responseID == "" {
		state.responseID = id
	}
	usage := jsonpeek.TokenUsageFrom(tail)
	if usage.Found {
		state.usage.Reported = true
		if usage.Input > 0 {
			state.usage.InputTokens = usage.Input
		}
		if usage.Output > 0 {
			state.usage.OutputTokens = usage.Output
			state.outputTokens = usage.Output
		}
		if usage.Total > 0 {
			state.usage.TotalTokens = usage.Total
		}
		if usage.Reasoning > 0 {
			state.usage.ReasoningTokens = usage.Reasoning
			state.reasoningTokens = usage.Reasoning
		}
		if usage.CostTicks > 0 {
			state.usage.CostInUSDTicks = usage.CostTicks
		}
		if usage.Sources > 0 {
			state.usage.NumSourcesUsed = usage.Sources
		}
		if usage.ServerTools > 0 {
			state.usage.NumServerSideToolsUsed = usage.ServerTools
		}
		if usage.ContextInput > 0 {
			state.usage.ContextInputTokens = usage.ContextInput
		}
		if usage.ContextOutput > 0 {
			state.usage.ContextOutputTokens = usage.ContextOutput
		}
	}
	if jsonpeek.HasKey(tail, "output_text") {
		if text := jsonpeek.StringField(tail, "text"); text != "" {
			state.aggregateRunes = max(state.aggregateRunes, utf8.RuneCountInString(text))
		} else {
			state.aggregateRunes = max(state.aggregateRunes, 32)
		}
	}
}

func observeQualityPayload(state *qualityScanState, payload []byte) {
	if len(payload) > 64<<10 {
		observeHugeQualityPayload(state, payload)
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
}

func observeQualityChat(state *qualityScanState, payload []byte) {
	// 高频增量帧只有 delta.content / reasoning_* ；usage 与 tool_calls
	// 仍走完整解码。finish_reason 是字符串才算终态（null 不是）。
	if jsonpeek.HasKey(payload, "usage") || jsonpeek.HasKey(payload, "tool_calls") || jsonpeek.HasKey(payload, "function_call") {
		observeQualityChatDecoded(state, payload)
		return
	}
	noteQualityEvent(state, "chat.chunk")
	if state.responseID == "" {
		if id := jsonpeek.StringField(payload, "id"); id != "" {
			state.responseID = id
		}
	}
	if hasVisibleThinkingBytes(jsonpeek.UnquotedBytes(payload, "reasoning")) ||
		hasVisibleThinkingBytes(jsonpeek.UnquotedBytes(payload, "reasoning_content")) ||
		hasVisibleThinkingBytes(jsonpeek.UnquotedBytes(payload, "thinking_content")) {
		state.hasThinking = true
	}
	if content := jsonpeek.UnquotedBytes(payload, "content"); len(content) > 0 {
		noteVisibleContentBytes(state, content)
	}
	if fr := jsonpeek.StringField(payload, "finish_reason"); fr != "" {
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
		state.outputTokens = event.Usage.CompletionTokens
		state.reasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if hasVisibleThinkingText(delta.Reasoning) || hasVisibleThinkingText(delta.ReasoningContent) || hasVisibleThinkingText(delta.ThinkingContent) {
			state.hasThinking = true
		}
		// 注意：refusal 增量有意不计入可见正文。合法的即时拒绝可能没有思考，
		// 若按规则 3 扣留会给账号记 12h missing-thinking——refusal-only 流走
		// 空流路径（15m 空闲冷却）是更安全的归类，勿把 Refusal 加进 noteVisible。
		if delta.Content != "" {
			noteVisibleContent(state, delta.Content)
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
	typ := jsonpeek.InternType(jsonpeek.RootStringBytes(payload, "type"))
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
		if hasVisibleThinkingBytes(jsonpeek.UnquotedBytes(payload, "delta")) {
			state.hasThinking = true
		}
		return
	case "response.output_text.delta":
		noteQualityEvent(state, typ)
		if delta := jsonpeek.UnquotedBytes(payload, "delta"); len(delta) > 0 {
			noteVisibleContentBytes(state, delta)
		}
		return
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
		noteQualityEvent(state, typ)
		if len(jsonpeek.UnquotedBytes(payload, "delta")) > 0 {
			state.semanticOutput = true
		}
		return
	case "response.output_item.added":
		noteQualityEvent(state, typ)
		item := jsonpeek.RawValue(payload, "item")
		itemType := string(jsonpeek.RootStringBytes(item, "type"))
		noteQualityItem(state, itemType)
		switch itemType {
		case "reasoning", "message", "":
		default:
			state.semanticOutput = true
		}
		return
	case "response.output_item.done":
		noteQualityEvent(state, typ)
		item := jsonpeek.RawValue(payload, "item")
		itemType := string(jsonpeek.RootStringBytes(item, "type"))
		itemID := string(jsonpeek.RootStringBytes(item, "id"))
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
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if hasVisibleThinkingText(event.Delta) {
			state.hasThinking = true
		}
	case "response.output_item.added":
		noteQualityItem(state, event.Item.Type)
		// A reasoning item header alone is not delivered thinking: degraded
		// streams open the item while never emitting visible reasoning text
		// deltas.
		//
		// 工具 item 头（web_search_call/function_call/mcp_call 等）是语义输出
		// 进行中：服务端联网搜索期间流会静默数秒（生产复现：创作
		// 控制台开联网搜索，搜索静默 >3.5s 被证据截止误杀，504 连环）。与
		// Anthropic 分支 content_block_start 的 tool_use 语义对称——置
		// semanticOutput 让证据截止不把搜索等待当降智静默。reasoning/message
		// 头不置：前者保持规则 2 的零延迟语义，后者随后即有文本增量。
		switch event.Item.Type {
		case "reasoning", "message", "":
		default:
			state.semanticOutput = true
		}
	case "response.output_item.done":
		// 零延迟拦截（规则 2）：reasoning item 闭合（type=reasoning 或
		// rs_/reasoning_ 前缀 ID，常携数 MiB encrypted_content）而本流未
		// 产出任何思考增量——降智包的确切签名，0ms 判定 QualityWithhold，
		// 无需等待证据超时。
		if (event.Item.Type == "reasoning" || strings.HasPrefix(event.Item.ID, "rs_") || strings.HasPrefix(event.Item.ID, "reasoning_")) && !state.hasThinking {
			state.reasoningEndedWithoutThinking = true
		}
		// item.done 也能携带聚合形式的 message 全文（部分上游不发增量、
		// 也不在 completed 帧重放 output）——与 completed 帧的聚合语义一致，
		// 计入可见估计；工具调用等非文本 item 计入语义输出。
		state.aggregateRunes = max(state.aggregateRunes, noteResponsesAggregateOutput(state, event.Item))
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
			state.usage.CostInUSDTicks = event.Response.Usage.CostInUSDTicks
			state.usage.ResponseModel = event.Response.Model
			state.outputTokens = event.Response.Usage.OutputTokens
			state.reasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
		}
	}
}
func observeQualityAnthropic(state *qualityScanState, payload []byte) {
	// 高频帧是 content_block_delta（thinking/text/partial_json）。
	// start/stop 需要嵌套 content_block.type，仍走完整解码。
	typ := jsonpeek.InternType(jsonpeek.RootStringBytes(payload, "type"))
	switch typ {
	case "message_stop":
		noteQualityEvent(state, typ)
		state.terminal = true
		return
	case "ping":
		noteQualityEvent(state, typ)
		return
	case "content_block_delta":
		noteQualityEvent(state, typ)
		if hasVisibleThinkingBytes(jsonpeek.UnquotedBytes(payload, "thinking")) {
			state.hasThinking = true
		}
		if text := jsonpeek.UnquotedBytes(payload, "text"); len(text) > 0 {
			noteVisibleContentBytes(state, text)
		}
		if len(jsonpeek.UnquotedBytes(payload, "partial_json")) > 0 {
			state.semanticOutput = true
		}
		return
	}
	observeQualityAnthropicDecoded(state, payload)
}

func observeQualityAnthropicDecoded(state *qualityScanState, payload []byte) {
	event := &state.anthropicEvent
	*event = qualityAnthropicEvent{}
	if json.Unmarshal(payload, event) != nil {
		return
	}
	noteQualityEvent(state, event.Type)
	switch event.Type {
	case "message_stop":
		state.terminal = true
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
	case "content_block_delta":
		if event.Delta.Type == "thinking_delta" && hasVisibleThinkingText(event.Delta.Thinking) {
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
		if !unicode.IsSpace(r) && !unicode.Is(unicode.Cf, r) {
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

func peekQualityStream(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	replay, verdict, usage, _, err := peekQualityStreamReport(ctx, body, protocol, cfg)
	return replay, verdict, usage, err
}

func peekQualityStreamReport(ctx context.Context, body io.ReadCloser, protocol string, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
	cfg = normalizeQualityRetry(cfg)
	empty := qualityScanState{protocol: protocol, startedAt: time.Now()}
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityEmptyStream), errQualityEmptyStream
	}
	pump := newQualityReadPump(body)
	state := qualityScanState{protocol: protocol, startedAt: time.Now()}
	var held bytes.Buffer
	// 零证据截止（唯一的防死锁预算，蓝图规则 4）：静默期超过该时长仍无
	// 思考证据且无可见输出即中止该次尝试（走空闲路径：短冷却+RSC 归因+
	// 重试）。降智流已被 item.done 零延迟拦截截胡，干净流首思考增量 2.1s
	// 即达，3.5s 有安全边际。旧的 hold 窗口已删：零延迟状态机下任何判决
	// 性信号在主循环即刻生效，窗口到期无信号可翻盘。
	evidenceTimer := time.NewTimer(cfg.EvidenceTimeout)
	defer evidenceTimer.Stop()
	// 首事件截止：零 data 事件（keepalive 不算）。仅当本截止短于证据截止
	// 时更早触发；默认 5s>3.5s 时空流由证据截止先赢。
	createdTimer := time.NewTimer(cfg.CreatedTimeout)
	defer createdTimer.Stop()
	emit := func(replay io.ReadCloser, verdict QualityVerdict, usage Usage, err error) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
		return replay, verdict, usage, state.fingerprint(verdict, err), err
	}
	for {
		// 空流短路（与 finishQualityPeek 同语义）：terminal 已到而零内容零
		// 推理时，usage 声明（含 output_tokens>0 的形式）不得把空流洗成可
		// 扣留的“有内容”流——那会走扣留重试而不是空流冷却路径。
		// 纯语义输出（工具调用等）不是空流：思考期望内按扣留收口，
		// 其余交付，均不按空流冷却惩罚。
		if state.terminal && state.emptyEvidence() {
			verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
			return emit(newPrefixReplay(&held, pump), verdict, state.usage, verdictErr)
		}
		if verdict := classifyQualityHold(state.signals()); verdict != QualityWait {
			return emit(newPrefixReplay(&held, pump), verdict, state.usage, nil)
		}

		select {
		case <-ctx.Done():
			_ = pump.Close()
			return emit(io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, qualityPeekAbortError(ctx, ctx.Err()))
		case <-evidenceTimer.C:
			// 截止触发时仍零思考证据、零可见/聚合输出、非语义输出流：该次
			// 尝试按零证据超时中止（服务端走空闲冷却+RSC 归因+重试）。已有
			// 任何输出或证据的流不受影响（证据提前放行/输出达到阈值扣留）。
			if !state.hasThinking && state.visibleRunes == 0 && state.aggregateRunes == 0 && !state.semanticOutput {
				_ = pump.Close()
				return emit(io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, errQualityEvidenceTimeout)
			}
		case <-createdTimer.C:
			// 首事件截止：连一个 data 事件都没到（keepalive 注释不算）。降智
			// 排队期间的真实形态（直连复测 68-125s 零 data 事件）。中止该次
			// 尝试走空闲路径；任何 data 事件已到达则本截止失效（由证据截止接管）。
			if !state.sawDataEvent {
				_ = pump.Close()
				return emit(io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, errQualityCreatedTimeout)
			}
		case result, ok := <-pump.results:
			if !ok {
				replay, verdict, usage, err := finishQualityPeek(&held, pump, &state, cfg)
				return emit(replay, verdict, usage, err)
			}
			if len(result.data) > 0 {
				if held.Len()+len(result.data) > qualityHoldMaxBufferBytes {
					_, _ = held.Write(result.data)
					observeQualityChunk(&state, result.data)
					if state.hasThinking {
						return emit(newPrefixReplay(&held, pump), QualityDeliver, state.usage, nil)
					}
					// 无思考证据的超大前缀几乎一定是降智密文堆，扣留而不是放行。
					return emit(newPrefixReplay(&held, pump), QualityWithhold, state.usage, nil)
				}
				_, _ = held.Write(result.data)
				observeQualityChunk(&state, result.data)
			}
			if result.err == io.EOF {
				replay, verdict, usage, err := finishQualityPeek(&held, pump, &state, cfg)
				return emit(replay, verdict, usage, err)
			}
			if result.err != nil {
				_ = pump.Close()
				return emit(io.NopCloser(bytes.NewReader(held.Bytes())), QualityWait, state.usage, qualityPeekAbortError(ctx, result.err))
			}
		}
	}
}

func finishQualityPeek(held *bytes.Buffer, pump *qualityReadPump, state *qualityScanState, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	if state == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, errQualityEmptyStream
	}
	if len(state.pending) > 0 || state.skipUntilNewline {
		// Process a final valid SSE data line even when the upstream omitted its
		// trailing newline. Also flush a skipped huge line that never saw '\n'.
		observeQualityChunk(state, []byte{'\n'})
	}
	state.terminal = true
	signals := state.signals()
	// 空流判定只看流证据：只有 usage 帧（声称 reasoning tokens）而零内容零
	// 推理事件的流同样是空流——usage 声明不能把空 200 洗成可投递响应。
	// 纯语义输出（工具调用等）不是空流：思考期望内按扣留收口，其余交付。
	if state.emptyEvidence() {
		verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
		return newPrefixReplay(held, pump), verdict, state.usage, verdictErr
	}
	return newPrefixReplay(held, pump), classifyQualityHold(signals), state.usage, nil
}

// qualityChatJSONBody 是非流式 chat 请求经 adapter 转换后的客户端形态。
type qualityChatJSONBody struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	Choices []struct {
		Message struct {
			Content          string            `json:"content"`
			Reasoning        string            `json:"reasoning"`
			ReasoningContent string            `json:"reasoning_content"`
			ThinkingContent  string            `json:"thinking_content"`
			Refusal          string            `json:"refusal"`
			ToolCalls        []json.RawMessage `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
}

// qualityAnthropicJSONBody 是非流式 Messages 请求经 adapter 转换后的形态。
type qualityAnthropicJSONBody struct {
	ID    string `json:"id"`
	Model string `json:"model"`
	Usage *struct {
		InputTokens         int64 `json:"input_tokens"`
		OutputTokens        int64 `json:"output_tokens"`
		OutputTokensDetails struct {
			ThinkingTokens int64 `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	} `json:"usage"`
	Content []struct {
		Type     string `json:"type"`
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	} `json:"content"`
}

// parseQualityClientJSONBody 识别转换后的客户端 JSON 形态并把证据映射进
// 扫描状态。识别失败返回 ok=false（调用方 fail-open）。证据规则与流式
// 契约一致：可见思考文本是唯一健康证据，工具调用是语义输出，refusal
// 不计可见（流式侧 round 34 已文档化同一取舍）。
func parseQualityClientJSONBody(data []byte) (*qualityScanState, bool) {
	var chat qualityChatJSONBody
	if err := json.Unmarshal(data, &chat); err == nil && chat.Choices != nil {
		state := &qualityScanState{protocol: qualityProtocolChat, responseID: chat.ID, startedAt: time.Now(), sawDataEvent: true}
		if chat.Usage != nil {
			state.usage = Usage{
				Reported: true, InputTokens: chat.Usage.PromptTokens,
				OutputTokens:    chat.Usage.CompletionTokens,
				ReasoningTokens: chat.Usage.CompletionTokensDetails.ReasoningTokens,
				TotalTokens:     chat.Usage.TotalTokens, ResponseModel: chat.Model,
			}
			state.outputTokens = chat.Usage.CompletionTokens
			state.reasoningTokens = chat.Usage.CompletionTokensDetails.ReasoningTokens
		}
		for _, choice := range chat.Choices {
			message := choice.Message
			if strings.TrimSpace(message.Reasoning) != "" || strings.TrimSpace(message.ReasoningContent) != "" || strings.TrimSpace(message.ThinkingContent) != "" {
				state.hasThinking = true
			}
			if message.Content != "" {
				state.aggregateRunes += utf8.RuneCountInString(message.Content)
			}
			if len(message.ToolCalls) > 0 {
				state.semanticOutput = true
			}
		}
		state.terminal = true
		return state, true
	}
	var anthropic qualityAnthropicJSONBody
	if err := json.Unmarshal(data, &anthropic); err == nil && anthropic.Content != nil {
		state := &qualityScanState{protocol: qualityProtocolAnthropic, responseID: anthropic.ID, startedAt: time.Now(), sawDataEvent: true}
		if anthropic.Usage != nil {
			state.usage = Usage{
				Reported: true, InputTokens: anthropic.Usage.InputTokens,
				OutputTokens:    anthropic.Usage.OutputTokens,
				ReasoningTokens: anthropic.Usage.OutputTokensDetails.ThinkingTokens,
				TotalTokens:     anthropic.Usage.OutputTokens, ResponseModel: anthropic.Model,
			}
			state.outputTokens = anthropic.Usage.OutputTokens
			state.reasoningTokens = anthropic.Usage.OutputTokensDetails.ThinkingTokens
		}
		for _, block := range anthropic.Content {
			switch block.Type {
			case "thinking", "redacted_thinking":
				if block.Type == "thinking" && strings.TrimSpace(block.Thinking) != "" {
					state.hasThinking = true
				}
				// redacted_thinking 是密文：既非证据也非语义输出（round 32）。
			case "text":
				if block.Text != "" {
					state.aggregateRunes += utf8.RuneCountInString(block.Text)
				}
			default:
				if block.Type != "" {
					state.semanticOutput = true
				}
			}
		}
		state.terminal = true
		return state, true
	}
	return nil, false
}

// verdictForBodyState 收口客户端形态 body 的终态判决（与 Responses 形状
// 的收口同语义：空证据走空流，纯语义输出交付，其余按分类器）。
func verdictForBodyState(replay io.ReadCloser, state *qualityScanState, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	if state.emptyEvidence() {
		verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
		return replay, verdict, state.usage, verdictErr
	}
	return replay, classifyQualityHold(state.signals()), state.usage, nil
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
// encrypted_content 与 usage 声明不构成证据（实测：降智响应
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
// （读完即判，无窗口/阈值等待）。证据规则与流式扫描器一致：可见思考
// 文本是唯一健康证据。无法识别的响应形状（无 output 数组的合法 JSON）
// fail-open 放行——不猜测质量。
func peekQualityBody(body io.ReadCloser, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, error) {
	replay, verdict, usage, _, err := peekQualityBodyReport(body, cfg)
	return replay, verdict, usage, err
}

func peekQualityBodyReport(body io.ReadCloser, cfg QualityRetryRuntime) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
	cfg = normalizeQualityRetry(cfg)
	empty := qualityScanState{protocol: qualityProtocolResponses, startedAt: time.Now()}
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityEmptyStream), errQualityEmptyStream
	}
	data, readErr := io.ReadAll(io.LimitReader(body, qualityBodyPeekLimit+1))
	if int64(len(data)) > qualityBodyPeekLimit {
		// 超预算的响应不再判决:重放已读字节并继续透传剩余 body(fail-open, 与
		// "无法识别的响应形状"同语义——不猜测质量)。此前 io.ReadAll 无上限, 异常/
		// 被劫持上游的超大 200 body 会在判决前全量驻留内存, 并发下 OOM。
		// Close 必须传导给原始 body: Build 路径的 egressResponseBody.Close 同时
		// 释放上游连接与 egress 租约, NopCloser 会两者都泄漏。
		return &chainedBody{Reader: io.MultiReader(bytes.NewReader(data), body), closer: body}, QualityDeliver, Usage{}, qualityHoldFingerprint{Verdict: string(QualityDeliver), Rule: "unbounded"}, nil
	}
	_ = body.Close()
	replay := io.NopCloser(bytes.NewReader(data))
	if readErr != nil {
		return replay, QualityWait, Usage{}, empty.fingerprint(QualityWait, readErr), readErr
	}
	var parsed qualityResponseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		// 200 但 body 非合法 JSON：按空流处理（可重试），不猜质量。
		return replay, QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityEmptyStream), errQualityEmptyStream
	}
	if parsed.Output == nil {
		// 合法 JSON 但非 Responses 形状：非流式 chat/messages 请求的 body 已被
		// adapter 转换成客户端形态（ForwardResponse 内完成）。按转换后的形状
		// 判决——证据规则与流式契约一致（rounds 18/19 锁定的发射面）。
		if clientState, ok := parseQualityClientJSONBody(data); ok {
			replay, verdict, usage, err := verdictForBodyState(replay, clientState, cfg)
			return replay, verdict, usage, clientState.fingerprint(verdict, err), err
		}
		// 仍无法识别的形状：fail-open，不猜质量。
		return replay, QualityDeliver, Usage{}, qualityHoldFingerprint{Verdict: string(QualityDeliver), Rule: "unrecognized"}, nil
	}
	state := qualityScanState{protocol: qualityProtocolResponses, startedAt: time.Now(), sawDataEvent: true}
	noteQualityEncrypted(&state, data)
	noteQualityEvent(&state, "response.completed")
	for _, item := range parsed.Output {
		itemType := strings.ToLower(strings.TrimSpace(item.Type))
		noteQualityItem(&state, itemType)
		noteQualityEvent(&state, itemType)
		switch itemType {
		case "reasoning":
			// 与流式一致：reasoning item 的可见文本（summary/内容）是思考
			// 证据；仅携带密文的 reasoning item 不是。
			seenThinkingText := false
			for _, summary := range item.Summary {
				if strings.TrimSpace(summary.Text) != "" {
					state.hasThinking = true
					seenThinkingText = true
				}
			}
			for _, content := range item.Content {
				if strings.TrimSpace(content.Text) != "" {
					state.hasThinking = true
					seenThinkingText = true
				}
			}
			// 差分一致性（流式规则 2 的 body 形态）：reasoning item 闭合而零
			// 可见思考文本——流式路径在 item.done 上判 reasoningEndedWithoutThinking，
			// body 路径此前把它漏成空流。密文有无不改变判定（流式同此）。
			if !seenThinkingText {
				state.reasoningEndedWithoutThinking = true
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
	if state.emptyEvidence() {
		verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
		return replay, verdict, usage, state.fingerprint(verdict, verdictErr), verdictErr
	}
	// body 已完整到达：恒为终态证据。
	sig := state.signals()
	sig.Terminal = true
	verdict := classifyQualityHold(sig)
	return replay, verdict, usage, state.fingerprint(verdict, nil), nil
}
