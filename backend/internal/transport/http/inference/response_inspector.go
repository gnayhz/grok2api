package inference

import (
	"bytes"
	"encoding/json"
	"unicode/utf8"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

type responseInspector struct {
	protocol        streamProtocol
	pending         []byte
	metadata        responseMetadata
	onFirstToken    func()
	firstTokenSeen  bool
	firstTokenReady bool
	terminalSuccess bool
	terminalFailure bool
	// deltaScanDone 在首个生成增量命中后置位:OutputObserved 与首 token
	// 相位都已被该命中永久定局,后续帧的 containsGeneratedDelta 解析是
	// 纯浪费(蓝图 #15:消除重复按行解析,长流从逐帧解码变为 1 次)。
	deltaScanDone bool
}

func (i *responseInspector) Inspect(chunk []byte) {
	if cap(i.pending) == 0 && len(chunk) > 0 {
		i.pending = make([]byte, 0, responseCopyBufferBytes)
	}
	i.pending = append(i.pending, chunk...)
	for {
		index := bytes.IndexByte(i.pending, '\n')
		if index < 0 {
			if len(i.pending) > maxStreamEventInspectionBytes {
				// The line has already been forwarded by copyStream. Treat an
				// oversized SSE data line as observed output conservatively so a
				// later idle timeout cannot misclassify a non-empty response and
				// apply the long empty-stream cooldown.
				if bytes.HasPrefix(bytes.TrimSpace(i.pending), []byte("data:")) {
					i.metadata.Usage.OutputObserved = true
					i.metadata.DeliveredEvents++
				}
				i.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(i.pending[:index])
		i.pending = i.pending[index+1:]
		if bytes.HasPrefix(line, []byte("data:")) {
			i.metadata.DeliveredEvents++
			value := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			i.inspectDataPayload(value)
		}
	}
}

func (i *responseInspector) markFirstTokenForwarded() {
	if i.firstTokenSeen || !i.firstTokenReady || i.onFirstToken == nil {
		return
	}
	i.firstTokenReady = false
	i.firstTokenSeen = true
	i.onFirstToken()
	i.onFirstToken = nil
}

func containsGeneratedDelta(data []byte, protocol streamProtocol) bool {
	switch protocol {
	case streamProtocolResponses:
		return responsesContainsGeneratedDelta(data)
	case streamProtocolChat:
		var event struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					Reasoning        string `json:"reasoning"`
					ReasoningContent string `json:"reasoning_content"`
					ThinkingContent  string `json:"thinking_content"`
					Refusal          string `json:"refusal"`
					ToolCalls        []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false
		}
		for _, choice := range event.Choices {
			delta := choice.Delta
			if delta.Content != "" || delta.Reasoning != "" || delta.ReasoningContent != "" || delta.ThinkingContent != "" || delta.Refusal != "" {
				return true
			}
			for _, call := range delta.ToolCalls {
				if call.Function.Arguments != "" {
					return true
				}
			}
		}
	case streamProtocolAnthropic:
		var event struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal(data, &event) != nil {
			return false
		}
		if event.Type == "content_block_start" {
			return event.ContentBlock.Type == "thinking"
		}
		if event.Type != "content_block_delta" {
			return false
		}
		switch event.Delta.Type {
		case "text_delta":
			return event.Delta.Text != ""
		case "thinking_delta":
			return event.Delta.Thinking != ""
		case "input_json_delta":
			return event.Delta.PartialJSON != ""
		}
	}
	return false
}

func (i *responseInspector) Metadata() responseMetadata {
	return normalizeMetadataUsage(i.metadata, i.protocol)
}

func normalizeMetadataUsage(metadata responseMetadata, protocol streamProtocol) responseMetadata {
	if protocol != streamProtocolAnthropic {
		return metadata
	}
	// Anthropic 输入重算口径(缓存读/写计入 input)归 gateway。
	metadata.Usage = metadata.Usage.RecomputeAnthropicInput(metadata.cacheCreationInputTokens)
	return metadata
}

func (i *responseInspector) TerminalError() error {
	if i.terminalFailure {
		return errUpstreamStreamFailed
	}
	if !i.terminalSuccess {
		return errUpstreamStreamIncomplete
	}
	return nil
}

func (i *responseInspector) observeTerminal(data []byte) {
	if bytes.Equal(data, []byte("[DONE]")) {
		if i.protocol == streamProtocolChat {
			i.terminalSuccess = true
		}
		return
	}
	if bytes.Contains(data, []byte(`"error"`)) && json.Valid(data) {
		if raw := bytes.TrimSpace(jsonpeek.RootRawValue(data, "error")); len(raw) > 0 && !bytes.Equal(raw, []byte("null")) {
			i.markTerminalFailure(data)
			return
		}
	}
	typ := sseEventType(data)
	if typ == "response.completed" {
		response := jsonpeek.RootRawValue(data, "response")
		status := jsonpeek.RootStringFieldScan(response, "status")
		if status != "" && status != "completed" {
			i.markTerminalFailure(data)
			return
		}
	}
	if terminal, success := streamTerminalOutcome(i.protocol, typ); terminal {
		if success {
			i.terminalSuccess = true
		} else {
			i.markTerminalFailure(data)
		}
	}
}

func (i *responseInspector) markTerminalFailure(data []byte) {
	i.terminalFailure = true
	if i.metadata.StreamFailure != nil {
		return
	}
	diagnostic := projectStreamFailureDiagnostic(data)
	if len(diagnostic.Body) > 0 {
		i.metadata.StreamFailure = &diagnostic
	}
}

func projectStreamFailureDiagnostic(data []byte) gateway.StreamFailureDiagnostic {
	var root map[string]json.RawMessage
	if json.Unmarshal(data, &root) != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, root, "type", "status", "code", "message", "param")
	if raw := projectSafeErrorValue(root["error"]); len(raw) > 0 {
		projected["error"] = raw
	}
	if responseRaw := root["response"]; len(responseRaw) > 0 {
		var response map[string]json.RawMessage
		if json.Unmarshal(responseRaw, &response) == nil {
			safeResponse := make(map[string]json.RawMessage)
			copySafeDiagnosticFields(safeResponse, response, "id", "status", "code", "message")
			if raw := projectSafeErrorValue(response["error"]); len(raw) > 0 {
				safeResponse["error"] = raw
			}
			if raw := projectSafeErrorValue(response["incomplete_details"]); len(raw) > 0 {
				safeResponse["incomplete_details"] = raw
			}
			if len(safeResponse) > 0 {
				if encoded, err := json.Marshal(safeResponse); err == nil {
					projected["response"] = encoded
				}
			}
		}
	}
	if len(projected) == 0 {
		return gateway.StreamFailureDiagnostic{}
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return gateway.StreamFailureDiagnostic{}
	}
	diagnostic := gateway.StreamFailureDiagnostic{Body: encoded}
	if len(diagnostic.Body) > maxStreamFailureDiagnosticBytes {
		bounded := diagnostic.Body[:maxStreamFailureDiagnosticBytes]
		for len(bounded) > 0 && !utf8.Valid(bounded) {
			bounded = bounded[:len(bounded)-1]
		}
		diagnostic.Body = append([]byte(nil), bounded...)
		diagnostic.BodyTruncated = true
	} else {
		diagnostic.Body = append([]byte(nil), diagnostic.Body...)
	}
	return diagnostic
}

func copySafeDiagnosticFields(destination, source map[string]json.RawMessage, fields ...string) {
	for _, field := range fields {
		if raw := projectSafeScalar(source[field]); len(raw) > 0 {
			destination[field] = raw
		}
	}
}

func projectSafeErrorValue(raw json.RawMessage) json.RawMessage {
	if scalar := projectSafeScalar(raw); len(scalar) > 0 {
		return scalar
	}
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	projected := make(map[string]json.RawMessage)
	copySafeDiagnosticFields(projected, value, "type", "status", "code", "message", "param", "reason")
	if len(projected) == 0 {
		return nil
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return nil
	}
	return encoded
}

func projectSafeScalar(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	switch value.(type) {
	case nil, string, bool, float64:
		return append(json.RawMessage(nil), raw...)
	default:
		return nil
	}
}

func (i *responseInspector) Finish() {
	if len(i.pending) == 0 {
		return
	}
	i.pending = append(i.pending, '\n')
	i.Inspect(nil)
}

func extractMetadata(data []byte) responseMetadata {
	var root responsePayloadDTO
	if json.Unmarshal(data, &root) != nil {
		return responseMetadata{}
	}
	metadata := responseMetadata{ResponseID: root.ID, Model: root.Model, SequenceNumber: root.SequenceNumber}
	usage := root.Usage
	if root.Response != nil {
		if metadata.ResponseID == "" {
			metadata.ResponseID = root.Response.ID
		}
		if metadata.Model == "" {
			metadata.Model = root.Response.Model
		}
		if metadata.SequenceNumber == 0 {
			metadata.SequenceNumber = root.Response.SequenceNumber
		}
		if usage == nil {
			usage = root.Response.Usage
		}
	}
	metadata.NativeResponseID = metadata.ResponseID
	if usage == nil {
		return metadata
	}
	metadata.Usage = usage.toGatewayUsage(metadata.Model)
	metadata.cacheCreationInputTokens = usage.CacheCreationInputTokens
	return metadata
}

type responsePayloadDTO struct {
	ID             string              `json:"id"`
	Model          string              `json:"model"`
	SequenceNumber int64               `json:"sequence_number"`
	Usage          *responseUsageDTO   `json:"usage"`
	Response       *responsePayloadDTO `json:"response"`
}

type responseUsageDTO struct {
	InputTokens            int64 `json:"input_tokens"`
	InputTokensCamel       int64 `json:"inputTokens"`
	OutputTokens           int64 `json:"output_tokens"`
	OutputTokensCamel      int64 `json:"outputTokens"`
	TotalTokens            int64 `json:"total_tokens"`
	TotalTokensCamel       int64 `json:"totalTokens"`
	CostInUSDTicks         int64 `json:"cost_in_usd_ticks"`
	NumSourcesUsed         int64 `json:"num_sources_used"`
	NumServerSideToolsUsed int64 `json:"num_server_side_tools_used"`
	// Responses protocol: input_tokens_details.cached_tokens
	InputTokensDetails responseInputDetailsDTO `json:"input_tokens_details"`
	// OpenAI Chat Completions protocol: prompt_tokens_details.cached_tokens
	PromptTokensDetails responseInputDetailsDTO `json:"prompt_tokens_details"`
	// Anthropic Messages protocol: top-level cache_read_input_tokens
	CacheReadInputTokens     *int64                   `json:"cache_read_input_tokens"`
	CachedTokens             *int64                   `json:"cached_tokens"`
	CacheCreationInputTokens int64                    `json:"cache_creation_input_tokens"`
	OutputTokensDetails      responseOutputDetailsDTO `json:"output_tokens_details"`
	// OpenAI Chat Completions protocol: completion_tokens_details.reasoning_tokens
	CompletionTokensDetails responseOutputDetailsDTO  `json:"completion_tokens_details"`
	ContextDetails          responseContextDetailsDTO `json:"context_details"`
	PromptTokens            int64                     `json:"prompt_tokens"`
	CompletionTokens        int64                     `json:"completion_tokens"`
}

type responseInputDetailsDTO struct {
	CachedTokens *int64 `json:"cached_tokens"`
}

type responseOutputDetailsDTO struct {
	ReasoningTokens int64 `json:"reasoning_tokens"`
	ThinkingTokens  int64 `json:"thinking_tokens"`
}

type responseContextDetailsDTO struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func (value responseUsageDTO) toGatewayUsage(responseModel string) gateway.Usage {
	input := value.InputTokens
	if input == 0 {
		input = value.InputTokensCamel
	}
	if input == 0 {
		input = value.PromptTokens
	}
	output := value.OutputTokens
	if output == 0 {
		output = value.OutputTokensCamel
	}
	if output == 0 {
		output = value.CompletionTokens
	}
	total := value.TotalTokens
	if total == 0 {
		total = value.TotalTokensCamel
	}
	// Unified cache hits: Responses / Chat Completions / Anthropic Messages
	var cached int64
	cacheReported := false
	for _, count := range []*int64{value.InputTokensDetails.CachedTokens, value.PromptTokensDetails.CachedTokens, value.CacheReadInputTokens, value.CachedTokens} {
		if count != nil {
			cached, cacheReported = *count, true
			break
		}
	}
	reasoning := value.OutputTokensDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = value.CompletionTokensDetails.ReasoningTokens
	}
	if reasoning == 0 {
		reasoning = value.OutputTokensDetails.ThinkingTokens
	}
	usage := gateway.Usage{
		Reported:                  true,
		CachedInputTokensReported: cacheReported,
		InputTokens:               input, CachedInputTokens: cached,
		OutputTokens: output, ReasoningTokens: reasoning,
		TotalTokens: total, CostInUSDTicks: value.CostInUSDTicks,
		NumSourcesUsed: value.NumSourcesUsed, NumServerSideToolsUsed: value.NumServerSideToolsUsed,
		ContextInputTokens: value.ContextDetails.InputTokens, ContextOutputTokens: value.ContextDetails.OutputTokens,
		ResponseModel: responseModel,
	}
	// total 回退口径归 gateway(M19 计量 owner)。
	return usage.WithTotalFallback()
}
