package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
)

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
		Index   int `json:"index"`
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
// 扫描状态。识别失败返回 ok=false（调用方拒绝交付、记协议异常）。证据规则与流式
// 契约一致：可见思考文本是唯一健康证据，工具调用是语义输出，refusal
// 与 Responses 相同计入可见正文。只支持一个 index=0 的 Chat 选项。
func parseQualityClientJSONBody(data []byte) (*qualityScanState, bool) {
	var chat qualityChatJSONBody
	if err := json.Unmarshal(data, &chat); err == nil && chat.Choices != nil {
		state := &qualityScanState{protocol: qualityProtocolChat, responseID: chat.ID, startedAt: time.Now(), sawDataEvent: true}
		if len(chat.Choices) > 1 || len(chat.Choices) == 1 && chat.Choices[0].Index != 0 {
			state.protocolErr = errQualityChoices
			return state, true
		}
		if chat.Usage != nil {
			state.usage = Usage{
				Reported: true, InputTokens: chat.Usage.PromptTokens,
				OutputTokens:    chat.Usage.CompletionTokens,
				ReasoningTokens: chat.Usage.CompletionTokensDetails.ReasoningTokens,
				TotalTokens:     chat.Usage.TotalTokens, ResponseModel: chat.Model,
			}
		}
		for _, choice := range chat.Choices {
			message := choice.Message
			if hasVisibleThinkingText(message.Reasoning) || hasVisibleThinkingText(message.ReasoningContent) || hasVisibleThinkingText(message.ThinkingContent) {
				state.hasThinking = true
			}
			if message.Content != "" {
				state.aggregateRunes += utf8.RuneCountInString(message.Content)
			}
			state.aggregateRunes += utf8.RuneCountInString(message.Refusal)
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
		}
		for _, block := range anthropic.Content {
			switch block.Type {
			case "thinking", "redacted_thinking":
				if block.Type == "thinking" && hasVisibleThinkingText(block.Thinking) {
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
	if state.protocolErr != nil {
		return replay, QualityWait, state.usage, state.protocolErr
	}
	state.usage = boundedQualityUsage(state.usage)
	if state.emptyEvidence() {
		verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
		return replay, verdict, state.usage, verdictErr
	}
	return replay, classifyQualityHold(cfg, state.signals()), state.usage, nil
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

func peekQualityBodyReportWithBudget(body io.ReadCloser, cfg QualityRetryRuntime, budget *responsebuffer.Budget) (io.ReadCloser, QualityVerdict, Usage, qualityHoldFingerprint, error) {
	cfg = normalizeQualityRetry(cfg)
	empty := qualityScanState{protocol: qualityProtocolResponses, startedAt: time.Now()}
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityEmptyStream), errQualityEmptyStream
	}
	buffered, readErr := responsebuffer.ReadAll(body, budget, qualityBodyPeekLimit)
	_ = body.Close()
	var replay io.ReadCloser = buffered
	data, release, _ := buffered.BorrowBytes()
	defer release()
	if errors.Is(readErr, responsebuffer.ErrLimit) {
		readErr = errQualityHoldLimit
	}
	if readErr != nil {
		return replay, QualityWait, Usage{}, empty.fingerprint(QualityWait, readErr), readErr
	}
	workspace, err := responsebuffer.JSONWorkspace(budget, data)
	if err != nil {
		return replay, QualityWait, Usage{}, empty.fingerprint(QualityWait, err), err
	}
	defer workspace.Release()
	var parsed qualityResponseBody
	if err := json.Unmarshal(data, &parsed); err != nil {
		// 200 但 body 非合法 JSON：按空流处理（可重试），不猜质量。
		return replay, QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityEmptyStream), errQualityEmptyStream
	}
	if observeQualityFailure(&empty, data) {
		empty.sawDataEvent = true
		raw := jsonpeek.RootRawValue(data, "usage")
		empty.usage = usageFromPhysical(jsonpeek.TokenUsageObject(raw), Usage{})
		return replay, QualityWait, empty.usage, empty.fingerprint(QualityWait, empty.protocolErr), empty.protocolErr
	}
	if parsed.Output == nil {
		// 合法 JSON 但非 Responses 形状：非流式 chat/messages 请求的 body 已被
		// adapter 转换成客户端形态（ForwardResponse 内完成）。按转换后的形状
		// 判决——证据规则与流式契约一致（rounds 18/19 锁定的发射面）。
		if clientState, ok := parseQualityClientJSONBody(data); ok {
			clientState.startedAt = empty.startedAt
			replay, verdict, usage, err := verdictForBodyState(replay, clientState, cfg)
			return replay, verdict, usage, clientState.fingerprint(verdict, err), err
		}
		// Unknown shapes are protocol errors, not healthy quality observations.
		return replay, QualityWait, Usage{}, empty.fingerprint(QualityWait, errQualityBodyShape), errQualityBodyShape
	}
	state := qualityScanState{kernel: cfg.Kernel(), protocol: qualityProtocolResponses, startedAt: empty.startedAt, sawDataEvent: true}
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
				if hasVisibleThinkingText(summary.Text) {
					state.hasThinking = true
					seenThinkingText = true
				}
			}
			for _, content := range item.Content {
				if hasVisibleThinkingText(content.Text) {
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
	usage = boundedQualityUsage(usage)
	state.usage = usage
	state.terminal = true
	// 空响应判定与 finishQualityPeek 同语义：零内容零思考且非纯语义输出 = 空流。
	if state.emptyEvidence() {
		verdict, verdictErr := state.emptyStreamVerdict(cfg.ReasoningExpected)
		return replay, verdict, usage, state.fingerprint(verdict, verdictErr), verdictErr
	}
	// body 已完整到达：恒为终态证据。
	sig := state.signals()
	sig.Terminal = true
	verdict := classifyQualityHold(cfg, sig)
	return replay, verdict, usage, state.fingerprint(verdict, nil), nil
}
