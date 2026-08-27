package inference

import (
	"bytes"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func responsesContainsGeneratedDelta(data []byte) bool {
	typ := jsonpeek.RootStringField(data, "type")
	switch typ {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return jsonpeek.StringField(data, "delta") != ""
	case "response.output_item.added":
		if jsonpeek.StringField(data, "id") == "" {
			return false
		}
		return bytes.Contains(data, []byte(`"type":"reasoning"`)) || bytes.Contains(data, []byte(`"type": "reasoning"`))
	default:
		return false
	}
}

func sseEventType(data []byte) string {
	return jsonpeek.RootStringField(jsonpeek.Prefix(data, 4096), "type")
}

func (i *responseInspector) inspectDataPayload(value []byte) {
	if bytes.Equal(value, []byte("[DONE]")) {
		i.observeTerminal(value)
		return
	}
	if len(value) > maxParsedSSEJSONBytes {
		i.observeHugeSSEPayload(value)
		return
	}
	if containsGeneratedDelta(value, i.protocol) {
		i.metadata.Usage.OutputObserved = true
		i.markFirstTokenReady()
	}
	i.observeTerminal(value)
	i.applyPeekedFrameMetadata(value)
}

func (i *responseInspector) markFirstTokenReady() {
	if i.firstTokenSeen || i.firstTokenReady || i.onFirstToken == nil {
		return
	}
	i.firstTokenReady = true
}

func (i *responseInspector) observeHugeSSEPayload(value []byte) {
	// Complete huge lines still count as observed output so an idle timeout
	// cannot treat a ciphertext-bearing stream as empty. Incomplete lines
	// that overflow the pending cap are handled in Inspect.
	i.metadata.Usage.OutputObserved = true
	if i.protocol == streamProtocolResponses && responsesContainsGeneratedDelta(value) {
		i.markFirstTokenReady()
	}
	typ := sseEventType(value)
	switch i.protocol {
	case streamProtocolResponses:
		switch typ {
		case "response.completed":
			i.terminalSuccess = true
		case "response.failed", "response.incomplete", "response.error", "error":
			i.terminalFailure = true
		}
	case streamProtocolChat:
		if typ == "error" {
			i.terminalFailure = true
		}
	case streamProtocolAnthropic:
		switch typ {
		case "message_stop":
			i.terminalSuccess = true
		case "error":
			i.terminalFailure = true
		}
	case streamProtocolImage:
		switch typ {
		case "image_generation.completed":
			i.terminalSuccess = true
		case "image_generation.failed", "error":
			i.terminalFailure = true
		}
	}
	i.applyPeekedFrameMetadata(value)
}

func peekRootOrResponseString(head []byte, key string) string {
	if v := jsonpeek.RootStringField(head, key); v != "" {
		return v
	}
	if raw := jsonpeek.RawValue(head, "response"); len(raw) > 0 {
		return jsonpeek.RootStringField(raw, key)
	}
	switch jsonpeek.RootStringField(head, "type") {
	case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete":
		// Huge completed/failed frames truncate the nested response object in
		// the 4KB head; id/model still sit before encrypted_content.
		return jsonpeek.StringField(head, key)
	default:
		return ""
	}
}

func (i *responseInspector) applyPeekedFrameMetadata(value []byte) {
	head := jsonpeek.Prefix(value, 4096)
	// Root / nested response only — item.id must not win first-and-stick.
	if id := peekRootOrResponseString(head, "id"); id != "" {
		i.metadata.ResponseID = id
	}
	if model := peekRootOrResponseString(head, "model"); model != "" {
		i.metadata.Model = model
		i.metadata.Usage.ResponseModel = model
	}
	if seq, ok := jsonpeek.IntField(head, "sequence_number"); ok && seq > i.metadata.SequenceNumber {
		i.metadata.SequenceNumber = seq
	}
	src := value
	if len(value) > 8192 {
		src = jsonpeek.Suffix(value, 8192)
	}
	if !bytes.Contains(src, []byte(`"usage"`)) && !bytes.Contains(head, []byte(`"usage"`)) &&
		!bytes.Contains(src, []byte(`"prompt_tokens"`)) &&
		!bytes.Contains(src, []byte(`"completion_tokens"`)) {
		return
	}
	usage := jsonpeek.TokenUsageFrom(src)
	if !usage.Found {
		usage = jsonpeek.TokenUsageFrom(head)
	}
	applyPeekedUsage(&i.metadata, usage)
}

func applyPeekedUsage(meta *responseMetadata, usage jsonpeek.TokenUsage) {
	if meta == nil || !usage.Found {
		return
	}
	meta.Usage.Reported = true
	if usage.Input > 0 {
		meta.Usage.InputTokens = usage.Input
	}
	if usage.Output > 0 {
		meta.Usage.OutputTokens = usage.Output
	}
	if usage.Total > 0 {
		meta.Usage.TotalTokens = usage.Total
	}
	if usage.Reasoning > 0 {
		meta.Usage.ReasoningTokens = usage.Reasoning
	}
	if usage.Cached > 0 {
		meta.Usage.CachedInputTokens = usage.Cached
	}
	if usage.CacheCreation > 0 {
		meta.cacheCreationInputTokens = usage.CacheCreation
	}
	if usage.CostTicks > 0 {
		meta.Usage.CostInUSDTicks = usage.CostTicks
	}
	if usage.Sources > 0 {
		meta.Usage.NumSourcesUsed = usage.Sources
	}
	if usage.ServerTools > 0 {
		meta.Usage.NumServerSideToolsUsed = usage.ServerTools
	}
	if usage.ContextInput > 0 {
		meta.Usage.ContextInputTokens = usage.ContextInput
	}
	if usage.ContextOutput > 0 {
		meta.Usage.ContextOutputTokens = usage.ContextOutput
	}
	if meta.Usage.TotalTokens == 0 {
		sum := meta.Usage.InputTokens + meta.Usage.OutputTokens
		if sum > 0 {
			meta.Usage.TotalTokens = sum
		}
	}
}

func ssePayloadHasEncryptedContent(payload []byte) bool {
	return jsonpeek.HasKey(jsonpeek.Prefix(payload, 8192), "encrypted_content")
}
