package inference

// ThinkingEvidenceComment is the internal SSE comment written when a converter
// sees upstream thinking on a Messages stream that did not request thinking.
// The gateway scanner counts it as thinking evidence; HTTP transport strips it
// so it never reaches clients. The byte value is a compatibility contract.
const ThinkingEvidenceComment = ": grok2api-thinking-evidence"
