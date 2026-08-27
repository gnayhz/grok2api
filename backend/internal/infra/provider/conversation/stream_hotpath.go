package conversation

import (
	"bytes"

	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
)

func (c *streamConverter) handleHugeOutputItemDone(data []byte) error {
	head := data
	if len(head) > 8192 {
		head = data[:8192]
	}
	if !bytes.Contains(head, []byte(`"type":"reasoning"`)) && !bytes.Contains(head, []byte(`"type": "reasoning"`)) {
		return nil
	}
	item := responseItem{Type: "reasoning", ID: jsonpeek.StringField(head, "id")}
	if c.reasoningOutputEnabled() {
		if err := c.reasoningDone(item); err != nil {
			return err
		}
	}
	if c.operation == OperationMessages && c.options.AnthropicThinking {
		if raw := jsonpeek.RawValue(data, "encrypted_content"); len(raw) > 0 {
			return c.thinkingDoneJSONSignature(raw)
		}
	}
	return c.thinkingDone(item)
}

func (c *streamConverter) thinkingDoneJSONSignature(signatureJSON []byte) error {
	if c.operation != OperationMessages || !c.options.AnthropicThinking || !c.thinkingStarted || c.thinkingClosed {
		return nil
	}
	if len(bytes.TrimSpace(signatureJSON)) > 0 && !bytes.Equal(bytes.TrimSpace(signatureJSON), []byte("null")) && !bytes.Equal(signatureJSON, []byte(`""`)) {
		if err := c.writeSignatureDelta(signatureJSON); err != nil {
			return err
		}
	}
	c.thinkingClosed = true
	return c.writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": c.thinkingIndex})
}

func (c *streamConverter) handleHugeCompleted(data []byte, typeName string) error {
	head := jsonpeek.Prefix(data, 8192)
	tail := jsonpeek.Suffix(data, 8192)
	var response responseEnvelope
	response.ID = jsonpeek.StringField(head, "id")
	response.Model = jsonpeek.StringField(head, "model")
	response.Status = jsonpeek.StringField(head, "status")
	if response.Status == "" {
		// 字母序下 status 位于 output 之后：帧形态把大对象前置时 status
		// 会掉出 8KB 头窗，而从尾部窗口兜底（status 全帧只出现一次，
		// 且与 usage 同侧，误中面为零）。
		response.Status = jsonpeek.StringField(tail, "status")
	}
	usage := jsonpeek.TokenUsageFrom(tail)
	if usage.Found {
		response.Usage.InputTokens = usage.Input
		response.Usage.OutputTokens = usage.Output
		response.Usage.TotalTokens = usage.Total
		response.Usage.CostInUSDTicks = usage.CostTicks
		response.Usage.NumSourcesUsed = usage.Sources
		response.Usage.NumServerSideToolsUsed = usage.ServerTools
		response.Usage.OutputTokensDetails.ReasoningTokens = usage.Reasoning
		response.Usage.InputTokensDetails.CachedTokens = usage.Cached
		response.Usage.ContextDetails.InputTokens = usage.ContextInput
		response.Usage.ContextDetails.OutputTokens = usage.ContextOutput
	}
	c.setResponse(response)
	status := response.Status
	if status == "" && typeName == "response.incomplete" {
		status = "incomplete"
	}
	return c.done(status)
}
