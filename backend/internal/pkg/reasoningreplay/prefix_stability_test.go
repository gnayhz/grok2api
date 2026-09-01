package reasoningreplay

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/runtime/memory"
)

func mustBody(parts []string) []byte { return []byte(strings.Join(parts, "")) }

// Messages 协议重发的 assistant 文本是 input_text 线形态。历史回归:匹配器
// 只认 output_text 时,Messages 客户端每一轮都被判为 assistant 不匹配,回放
// 静默失效,上游缓存退化到公共头部(线上 cached 约 128 token,TTFT 放大
// 3-11 倍)。
func TestApplyMatchesMessagesInputTextAssistant(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(31)
	payload := mustBody([]string{
		`{"output":[{"type":"reasoning","encrypted_content":"`, enc, `"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"回答"}]}]}`,
	})
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "sess-input-text", payload)
	body := mustBody([]string{
		`{"input":[`,
		`{"type":"message","role":"user","content":"问题"},`,
		`{"type":"message","role":"assistant","content":[{"type":"input_text","text":"回答"}]},`,
		`{"type":"message","role":"user","content":"追问"}]}`,
	})
	out := replay.Apply(context.Background(), "grok-4.5", "sess-input-text", body)
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 4 || got.Input[1]["type"] != "reasoning" || got.Input[2]["role"] != "assistant" {
		t.Fatalf("input_text assistant must anchor replay: %s", out)
	}
	if !strings.Contains(string(out), enc) {
		t.Fatalf("reasoning missing: %s", out)
	}
}

// 工具循环轮:客户端重发了 function_call 但剥掉了 reasoning。回放必须锚定
// 在它自己的调用簇之前,而不是请求最前面。历史回归:无锚点回退曾把
// reasoning 插到第一个非 system 项之前,相邻两轮前缀在首个缓存块后分叉。
func TestApplyAnchorsBeforeResentFunctionCall(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(32)
	payload := mustBody([]string{
		`{"output":[{"type":"reasoning","encrypted_content":"`, enc, `"},`,
		`{"type":"function_call","call_id":"fc_1","name":"get_weather","arguments":"{}"}]}`,
	})
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "sess-tool-loop", payload)
	body := mustBody([]string{
		`{"input":[`,
		`{"type":"message","role":"user","content":"查天气"},`,
		`{"type":"function_call","call_id":"fc_1","name":"get_weather","arguments":"{}"},`,
		`{"type":"function_call_output","call_id":"fc_1","output":"22C"},`,
		`{"type":"message","role":"user","content":"继续"}]}`,
	})
	out := replay.Apply(context.Background(), "grok-4.5", "sess-tool-loop", body)
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Input) != 5 {
		t.Fatalf("input len=%d out=%s", len(got.Input), out)
	}
	if got.Input[0]["type"] != "message" || got.Input[0]["role"] != "user" {
		t.Fatalf("reasoning must not inject before the first user message: %s", out)
	}
	if got.Input[1]["type"] != "reasoning" || got.Input[2]["type"] != "function_call" {
		t.Fatalf("reasoning must sit right before its own call cluster: %s", out)
	}
	if !strings.Contains(string(out), enc) {
		t.Fatalf("reasoning missing: %s", out)
	}
}

// 新会话复用会话键:输入里没有任何可锚定的历史时,残留回放必须整体跳过。
// 历史回归:stale replay 被全量注入到新对话最前面,既串扰上下文又让缓存
// 从零开始。
func TestApplySkipsFreshConversationStaleReplay(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	enc := validEncrypted(33)
	payload := mustBody([]string{
		`{"output":[{"type":"reasoning","encrypted_content":"`, enc, `"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"旧会话回答"}]}]}`,
	})
	replay.StoreFromCompleted(context.Background(), "grok-4.5", "sess-stale", payload)
	body := []byte(`{"input":[{"type":"message","role":"user","content":"全新任务的第一条消息"}]}`)
	out := replay.Apply(context.Background(), "grok-4.5", "sess-stale", body)
	if strings.Contains(string(out), "旧会话回答") || strings.Contains(string(out), enc) {
		t.Fatalf("stale replay must not leak into a fresh conversation: %s", out)
	}
}

// 核心不变式:同一会话相邻两轮的上游序列满足「上一轮是本轮的纯前缀」。
// 上游提示缓存只复用严格前缀扩展;只保留最新一轮回放时,上一轮注入的
// reasoning 在本轮消失,前缀在中段分叉,每轮损失约一轮上下文的缓存。
func TestAccumulateStrictPrefixExtension(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	model, session := "grok-4.5", "sess-prefix"
	turn := 0
	respond := func() {
		turn++
		enc := validEncrypted(byte(40 + turn))
		payload := mustBody([]string{
			`{"output":[{"type":"reasoning","encrypted_content":"`, enc, `"},`,
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第`, fmt.Sprintf("%d", turn), `轮回答"}]}]}`,
		})
		replay.StoreFromCompleted(ctx, model, session, payload)
	}
	inputSequence := func(body []byte) []string {
		var got struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		seq := make([]string, 0, len(got.Input))
		for _, item := range got.Input {
			kind, _ := item["type"].(string)
			role, _ := item["role"].(string)
			if role != "" {
				kind += ":" + role
			}
			if enc, ok := item["encrypted_content"].(string); ok && enc != "" {
				kind += fmt.Sprintf(":%d", len(enc))
			}
			seq = append(seq, kind)
		}
		return seq
	}

	respond()
	u2 := mustBody([]string{
		`{"input":[`,
		`{"type":"message","role":"user","content":"u1"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第1轮回答"}]},`,
		`{"type":"message","role":"user","content":"u2"}]}`,
	})
	upstream2 := replay.Apply(ctx, model, session, u2)
	respond()
	u3 := mustBody([]string{
		`{"input":[`,
		`{"type":"message","role":"user","content":"u1"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第1轮回答"}]},`,
		`{"type":"message","role":"user","content":"u2"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"第2轮回答"}]},`,
		`{"type":"message","role":"user","content":"u3"}]}`,
	})
	upstream3 := replay.Apply(ctx, model, session, u3)

	seq2 := inputSequence(upstream2)
	seq3 := inputSequence(upstream3)
	if len(seq3) < len(seq2)+2 {
		t.Fatalf("turn3 should extend turn2: seq2=%v seq3=%v", seq2, seq3)
	}
	for i := range seq2 {
		if seq2[i] != seq3[i] {
			t.Fatalf("prefix broken at %d (accumulated replay must keep strict extension):\\nseq2=%v\\nseq3=%v\\nupstream2=%s\\nupstream3=%s", i, seq2, seq3, upstream2, upstream3)
		}
	}
	if !strings.Contains(string(upstream3), "u2") {
		t.Fatalf("unexpected upstream3: %s", upstream3)
	}
}

// 历史回归:累计去重曾按整条目字节比较,两轮 assistant 文本相同(如重复的
// 简短答复)时第二条消息被丢弃,拆散 [reasoning, message] 轮次结构,孤儿
// reasoning 失去锚点被跳过,上游前缀在该轮分叉。去重必须只看 reasoning 密文。
func TestAccumulateKeepsDuplicateTextMessages(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	model, session := "grok-4.5", "sess-dup-text"
	respond := func(seed byte, text string) {
		enc := validEncrypted(seed)
		payload := mustBody([]string{
			`{"output":[{"type":"reasoning","encrypted_content":"`, enc, `"},`,
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"`, text, `"}]}]}`,
		})
		replay.StoreFromCompleted(ctx, model, session, payload)
	}
	respond(61, "完成。")
	respond(62, "完成。")
	items, ok, err := store.Get(ctx, model, session, time.Now().UTC(), time.Hour)
	if err != nil || !ok {
		t.Fatalf("store miss: ok=%v err=%v", ok, err)
	}
	// 两轮各 2 条(re+msg),重复文本的第二条消息不得被去重丢弃。
	if len(items) != 4 {
		t.Fatalf("items=%d want 4 (duplicate-text message must survive): %v", len(items), items)
	}
	body := mustBody([]string{
		`{"input":[`,
		`{"type":"message","role":"user","content":"u1"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"完成。"}]},`,
		`{"type":"message","role":"user","content":"u2"},`,
		`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"完成。"}]},`,
		`{"type":"message","role":"user","content":"u3"}]}`,
	})
	out := replay.Apply(ctx, model, session, body)
	var got struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	reasonings := 0
	for _, item := range got.Input {
		if item["type"] == "reasoning" {
			reasonings++
		}
	}
	if reasonings != 2 {
		t.Fatalf("both turns must replay reasoning, got %d: %s", reasonings, out)
	}
}

// 历史回归:文本锚点曾按「从尾向前找最后一条同文消息」绑定,重复文本
// (循环样式的简短回答)会让旧轮次的 reasoning 漂移到最新重复消息之前,
// 相邻两轮上游序列在该位置分叉,缓存部分失效。顺序对齐后每轮绑定到
// 自己的匹配项,序列保持纯前缀扩展。
func TestAccumulateAnchorsDuplicateTextsInOrder(t *testing.T) {
	store := memory.NewReasoningReplayStore(64)
	replay := New(store, Config{Enabled: true, TTL: time.Hour}, slog.Default())
	ctx := context.Background()
	model, session := "grok-4.5", "sess-dup-order"
	turn := 0
	respond := func() {
		turn++
		enc := validEncrypted(byte(70 + turn))
		payload := mustBody([]string{
			`{"output":[{"type":"reasoning","encrypted_content":"`, enc, `"},`,
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"同样的回答"}]}]}`,
		})
		replay.StoreFromCompleted(ctx, model, session, payload)
	}
	inputSequence := func(body []byte) []string {
		var got struct {
			Input []map[string]any `json:"input"`
		}
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		seq := make([]string, 0, len(got.Input))
		for _, item := range got.Input {
			kind, _ := item["type"].(string)
			if enc, ok := item["encrypted_content"].(string); ok && enc != "" {
				kind += fmt.Sprintf(":%d", len(enc))
			}
			seq = append(seq, kind)
		}
		return seq
	}
	build := func(n int) []byte {
		items := []string{`{"type":"message","role":"user","content":"u1"}`}
		for i := 2; i <= n; i++ {
			items = append(items, `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"同样的回答"}]}`, `{"type":"message","role":"user","content":"u"}`)
		}
		return mustBody([]string{`{"input":[`, strings.Join(items, ","), `]}`})
	}
	respond()
	up2 := build(2)
	seq2 := inputSequence(replay.Apply(ctx, model, session, up2))
	respond()
	seq3 := inputSequence(replay.Apply(ctx, model, session, build(3)))
	respond()
	seq4 := inputSequence(replay.Apply(ctx, model, session, build(4)))
	for i := range seq3 {
		if i < len(seq2) && seq2[i] != seq3[i] {
			t.Fatalf("duplicate-text turns must keep strict prefix extension at %d: seq2=%v seq3=%v", i, seq2, seq3)
		}
	}
	for i := range seq4 {
		if i < len(seq3) && seq3[i] != seq4[i] {
			t.Fatalf("duplicate-text turns must keep strict prefix extension at %d: seq3=%v seq4=%v", i, seq3, seq4)
		}
	}
	_ = up2
}
