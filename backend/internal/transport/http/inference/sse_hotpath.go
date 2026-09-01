package inference

import (
	"bytes"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func responsesContainsGeneratedDelta(data []byte) bool {
	typ := jsonpeek.InternType(jsonpeek.RootStringBytes(data, "type"))
	switch typ {
	case "response.output_text.delta", "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.refusal.delta", "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		raw := jsonpeek.RawValue(data, "delta")
		return len(raw) > 2
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
	if typ := jsonpeek.InternType(jsonpeek.RootStringBytes(jsonpeek.Prefix(data, 4096), "type")); typ != "" {
		return typ
	}
	// 兼容层重写过的帧经 map[string]any 重排键序（字母序 "response" 在
	// "type" 之前），根层 type 可被多 KB 的 response 对象推到 4KB 头窗之外。
	// 头窗未命中时对完整帧做零分配的根层扫描（线上全部
	// upstream_stream_incomplete 回归根因）。
	return jsonpeek.InternType([]byte(jsonpeek.RootStringFieldScan(data, "type")))
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
	if !i.deltaScanDone && containsGeneratedDelta(value, i.protocol) {
		i.metadata.Usage.OutputObserved = true
		i.markFirstTokenReady()
		i.deltaScanDone = true
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
		i.deltaScanDone = true
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

func peekRootOrResponseString(value, head []byte, key string) string {
	if v := jsonpeek.RootStringField(head, key); v != "" {
		return v
	}
	if raw := jsonpeek.RawValue(head, "response"); len(raw) > 0 {
		if v := jsonpeek.RootStringField(raw, key); v != "" {
			return v
		}
	}
	switch sseEventType(value) {
	case "response.created", "response.in_progress", "response.completed", "response.failed", "response.incomplete":
		// Huge or key-sorted completed/failed frames truncate the nested
		// response object in the 4KB head; id/model still sit before any
		// encrypted_content payload. sseEventType must see the whole frame
		// here: on re-marshaled frames the root type sits past the head.
		// 先做根层整帧扫描（限定根层，item.id 不会误中），再退回头窗内
		// 的嵌套 response 早段字段（字母序 id/model 在 output 之前）。
		if v := jsonpeek.RootStringFieldScan(value, key); v != "" {
			return v
		}
		return jsonpeek.StringField(head, key)
	default:
		return ""
	}
}

func (i *responseInspector) applyPeekedFrameMetadata(value []byte) {
	head := jsonpeek.Prefix(value, 4096)
	// id/model 首次命中后即停止逐帧探测：RootStringField 的命中返回
	// （jsonpeek.go:101 的 string 转换）是流基准 ~97% 的分配来源，而
	// 这两个字段是 first-and-stick 语义——后续帧不会再改写它们。
	if i.metadata.ResponseID == "" {
		if id := peekRootOrResponseString(value, head, "id"); id != "" {
			i.metadata.ResponseID = id
		}
	}
	if i.metadata.Model == "" {
		if model := peekRootOrResponseString(value, head, "model"); model != "" {
			i.metadata.Model = model
			i.metadata.Usage.ResponseModel = model
		}
	}
	if seq, ok := jsonpeek.IntField(head, "sequence_number"); ok {
		if seq > i.metadata.SequenceNumber {
			i.metadata.SequenceNumber = seq
		}
	} else if seq, ok := jsonpeek.RootIntFieldScan(value, "sequence_number"); ok && seq > i.metadata.SequenceNumber {
		// 重排键序的大帧上 sequence_number 按字母序位于 response 对象
		// 之后，头窗取不到；整帧根层扫描兜底（合成终止事件的序号依赖它）。
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
