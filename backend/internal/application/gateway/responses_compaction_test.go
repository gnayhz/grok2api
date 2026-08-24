package gateway

import "testing"

const tuiCompactionPrompt = "Your task is to produce a faithful, concise summary of the conversation so far. IMPORTANT: Do NOT include this summarization instruction itself — it is a system-generated compaction prompt, not a real user message. Output the final summary inside a single <summary> block."

func TestIsResponsesCompactionRequest(t *testing.T) {
	const dshCompactionInstruction = "You are now acting as a compaction engine for this AI coding assistant. Condense the conversation ABOVE into a structured checkpoint that lets another model resume the work with no loss of essential"
	const codexLocalCompact = "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM that will resume the task."
	tests := []struct {
		name string
		body string
		want responsesCompactionKind
	}{
		{name: "remote v2", body: `{"input":[{"role":"user","content":"continue"},{"type":"compaction_trigger"}]}`, want: responsesCompactionTrigger},
		{name: "not terminal", body: `{"input":[{"type":"compaction_trigger"},{"role":"user","content":"continue"}]}`, want: responsesCompactionTrigger},
		{name: "tui last user string", body: `{"input":[{"role":"user","content":"continue"},{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`, want: responsesCompactionTUI},
		{name: "tui last user parts", body: `{"input":[{"role":"user","content":[{"type":"input_text","text":"` + tuiCompactionPrompt + `"}]}]}`, want: responsesCompactionTUI},
		{name: "tui chat messages", body: `{"messages":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`, want: responsesCompactionTUI},
		{name: "prompt only in history", body: `{"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"},{"role":"user","content":"continue the task"}]}`, want: responsesCompactionNone},
		{name: "assistant prompt", body: `{"input":[{"role":"assistant","content":"` + tuiCompactionPrompt + `"}]}`, want: responsesCompactionNone},
		{name: "system prompt", body: `{"messages":[{"role":"system","content":"` + tuiCompactionPrompt + `"}]}`, want: responsesCompactionNone},
		{name: "missing role", body: `{"input":[{"content":"` + tuiCompactionPrompt + `"}]}`, want: responsesCompactionNone},
		// 客户端本地 compact 以普通 user message 重放历史后追加指令:
		// 必须命中检测, 否则摘要响应(无思考输出)会被质量守卫误扣留。
		{name: "dsh harness replayed history plus instruction", body: `{"messages":[{"role":"user","content":"do the task"},{"role":"assistant","content":"done"},{"role":"user","content":"` + dshCompactionInstruction + `"}]}`, want: responsesCompactionTUI},
		{name: "dsh harness input form", body: `{"input":[{"role":"user","content":"` + dshCompactionInstruction + `"}]}`, want: responsesCompactionTUI},
		{name: "dsh harness parts form", body: `{"messages":[{"role":"user","content":[{"type":"text","text":"` + dshCompactionInstruction + `"}]}]}`, want: responsesCompactionTUI},
		{name: "codex local compact prompt", body: `{"messages":[{"role":"user","content":"` + codexLocalCompact + `"}]}`, want: responsesCompactionTUI},
		{name: "compaction phrase inside longer last message", body: `{"messages":[{"role":"user","content":"before\n` + codexLocalCompact + `\nafter"}]}`, want: responsesCompactionTUI},
		{name: "normal responses", body: `{"input":[{"role":"user","content":"continue"}]}`, want: responsesCompactionNone},
		{name: "string input", body: `{"input":"continue"}`, want: responsesCompactionNone},
		{name: "invalid json", body: `{`, want: responsesCompactionNone},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := classifyResponsesCompactionRequest([]byte(test.body)); got != test.want {
				t.Fatalf("classifyResponsesCompactionRequest() = %d, want %d", got, test.want)
			}
			if got := isResponsesCompactionRequest([]byte(test.body)); got != (test.want != responsesCompactionNone) {
				t.Fatalf("isResponsesCompactionRequest() = %t, want %t", got, test.want != responsesCompactionNone)
			}
		})
	}
}
