package gateway

// 上游参照项目声称的失效模式逐项实测（怀疑探针→修复锁定）：
// 每个测试先在本仓库复现过问题（探针阶段 FAIL = 问题确认），
// 修复后断言反转为期望行为，作为回归锁定。

import (
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

// 判决语义：可见输出+零思考 → 扣留（正文抢跑，与长度/截止无关）。
// 纯 stub 流（零输出零思考）不判 missing-thinking——静默挂起由证据截止
// 的空闲路径收口。
func TestDeadlineSemanticsWithholdsOutputStream(t *testing.T) {
	t.Parallel()
	sig := qualityStreamSignals{VisibleTokens: 64}
	if v := classifyQualityHold(sig); v != QualityWithhold {
		t.Fatalf("可见输出+零思考 = %s，应扣留（降智流不因任何原因放行）", v)
	}
	stubOnly := qualityStreamSignals{}
	if v := classifyQualityHold(stubOnly); v != QualityWait {
		t.Fatalf("stub-only = %s，应继续等待（空流走 idle 路径）", v)
	}
	terminal := qualityStreamSignals{VisibleTokens: 64, Terminal: true}
	if v := classifyQualityHold(terminal); v != QualityWithhold {
		t.Fatalf("终态无证据 = %s，应扣留", v)
	}
}

// 迟到密文不是证据：流中段可见输出达到阈值即扣留，不等待流末密文。
func TestPeekWithholdsStreamBeforeLateCiphertext(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true}
	content := strings.Repeat("word ", 40)
	stream := strings.Join([]string{
		`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		`data: {"type":"response.output_text.delta","delta":"` + content + `"}`,
	}, "\n") + "\n"
	reader, writer := io.Pipe()
	go func() {
		_, _ = writer.Write([]byte(stream))
		time.Sleep(300 * time.Millisecond)
		_, _ = writer.Write([]byte(`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAA-late"}}` + "\n\n"))
		_ = writer.Close()
	}()
	_, verdict, _, peekErr := peekQualityStream(context.Background(), reader, qualityProtocolResponses, cfg)
	_ = reader.Close()
	if peekErr != nil {
		t.Fatalf("peek err = %v", peekErr)
	}
	if verdict != QualityWithhold {
		t.Fatalf("迟到密文流 = %s，应扣留（密文无判别力，尽早拦截）", verdict)
	}
}

// 工具声明不影响 hold 门控(回归锁定):质量判决只看流特征。
// 上游按"hosted 工具/在途工具结果"豁免整轮的规则已移除——扣留的响应从不
// 发给客户端,客户端无从重放;纯语义输出(工具调用形态)按流特征 Deliver。
func TestToolDeclarationsStillHeld(t *testing.T) {
	t.Parallel()
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6"}
	cfg := QualityRetryRuntime{Enabled: true}
	gate := func(body string) bool {
		return shouldHoldQualityStream(Input{Streaming: true, Body: []byte(body), PublicModel: "grok-4.6"}, nil, route, audit.OperationResponses, cfg)
	}
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "web_search_options", body: `{"web_search_options":{}}`},
		{name: "mcp_servers", body: `{"mcp_servers":[{"url":"https://mcp.example"}]}`},
		{name: "hosted shell tool", body: `{"tools":[{"type":"shell"}]}`},
		{name: "unknown tool type defaults unsafe", body: `{"tools":[{"type":"future_native_tool"}]}`},
		{name: "namespace nested hosted", body: `{"tools":[{"type":"namespace","tools":[{"type":"code_interpreter"}]}]}`},
		{name: "additional_tools in input", body: `{"input":[{"type":"additional_tools","tools":[{"type":"shell"}]}]}`},
		{name: "client function tool", body: `{"tools":[{"type":"function","function":{"name":"charge"}}]}`},
		{name: "local shell", body: `{"tools":[{"type":"shell","environment":{"type":"local"}}]}`},
		{name: "apply_patch", body: `{"tools":[{"type":"apply_patch"}]}`},
	} {
		if !gate(tc.body) {
			t.Errorf("[%s] 工具声明不得豁免 hold: %s", tc.name, tc.body)
		}
	}
}

// 转换路径的反证锁定：chat 转换的 reasoning-start 注释与 Messages 的
// signature_delta 都不是思考证据（evidence 注释已随错误吸收一并移除）。
func TestConvertedEncryptedThinkingNotEvidence(t *testing.T) {
	t.Parallel()
	content := strings.Repeat("word ", 40)
	chat := qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(&chat, []byte(sse(
		`data: {"choices":[{"delta":{"reasoning_content":"先想一步"}}]}`,
	)))
	if v := classifyQualityHold(chat.signals()); v != QualityDeliver {
		t.Fatalf("chat 可见 reasoning_content 增量应放行: %s (%#v)", v, chat.signals())
	}
	messages := qualityScanState{protocol: qualityProtocolAnthropic}
	observeQualityChunk(&messages, []byte(sse(
		`data: {"type":"content_block_start","content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"signature_delta","signature":"EqoBCkgIYj"}}`,
		`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"`+content+`"}}`,
		`data: {"type":"message_stop"}`,
	)))
	if v := classifyQualityHold(messages.signals()); v != QualityWithhold {
		t.Fatalf("signature_delta（Messages 加密思考）不是证据，应扣留: %s (%#v)", v, messages.signals())
	}
}

// 语义输出与聚合输出不是空流。
func TestSemanticAndAggregatedOutputNotEmptyStream(t *testing.T) {
	t.Parallel()
	tools := qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(&tools, []byte(sse(
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"data: [DONE]",
	)))
	if !tools.semanticOutput {
		t.Fatal("纯 tool_calls 流未标记 semanticOutput")
	}
	if !tools.semanticOutputOnly() {
		t.Fatalf("纯 tool_calls 流应判定为仅语义输出: %#v", tools.signals())
	}
	agg := qualityScanState{protocol: qualityProtocolResponses}
	observeQualityChunk(&agg, []byte(sse(
		`data: {"type":"response.completed","response":{"id":"r1","output":[{"type":"message","content":[{"type":"output_text","text":"`+strings.Repeat("word ", 40)+`"}]}],"usage":{"output_tokens":50}}}`,
	)))
	sig := agg.signals()
	if sig.VisibleTokens <= 0 {
		t.Fatalf("聚合送达文本未计入可见输出: %#v", sig)
	}
}

// 清冷却保留打击标记（二击停用不可绕过）。
func TestClearCooldownPreservesStrikeMarker(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "strike-preserve.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	service := accountapp.NewService(repo, relational.NewAuditRepository(database), nil, nil, nil, nil, nil)
	created, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "strike", SourceKey: "strike",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := repo.UpdateHealth(ctx, created.ID, accountdomain.ProviderBuild, 0, nil, accountdomain.LastErrorMissingThinkingDisabled, false); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ClearCooldown(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != 0 || stored.CooldownUntil != nil {
		t.Fatalf("冷却计数未清零: %#v", stored)
	}
	if stored.LastError != accountdomain.LastErrorMissingThinkingDisabled {
		t.Fatalf("打击标记被抹掉（下次降智将绕过二击停用）: lastError=%q", stored.LastError)
	}
}
