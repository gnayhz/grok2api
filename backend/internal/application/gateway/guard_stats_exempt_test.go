package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// TestGuardStatsExemptsCountAndSerialize 锁定豁免计数：已知 token 计数、
// 未知 token 丢弃、快照按固定顺序平铺且 JSON 键为 exempts。
func TestGuardStatsExemptsCountAndSerialize(t *testing.T) {
	collector := newGuardStatsCollector()
	collector.recordExempt(QualityExemptMessagesNoThink)
	collector.recordExempt(QualityExemptMessagesNoThink)
	collector.recordExempt(QualityExemptModelScope)
	collector.recordExempt("totally_unknown_reason")

	snapshot := collector.Snapshot()
	byReason := make(map[string]GuardExemptStat, len(snapshot.Exempts))
	for _, stat := range snapshot.Exempts {
		byReason[stat.Reason] = stat
	}
	if len(snapshot.Exempts) != len(guardExemptOrder) {
		t.Fatalf("exempt rows = %d want %d", len(snapshot.Exempts), len(guardExemptOrder))
	}
	if got := byReason[QualityExemptMessagesNoThink].Count; got != 2 {
		t.Fatalf("messages_thinking_off count = %d", got)
	}
	if got := byReason[QualityExemptModelScope].Count; got != 1 {
		t.Fatalf("model_out_of_scope count = %d", got)
	}
	if got := byReason[QualityExemptDisabled].Count; got != 0 {
		t.Fatalf("untouched reason must stay zero, got %d", got)
	}
	if byReason[QualityExemptMessagesNoThink].LastSeen == nil {
		t.Fatal("lastSeen must be set on count")
	}
	collector.recordExempt(QualityExemptDisabled)
	if byReason[QualityExemptDisabled].Count != 0 {
		t.Fatal("snapshot must be a copy")
	}

	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(payload), "\"exempts\"") {
		t.Fatalf("snapshot JSON must carry exempts: %s", payload)
	}
	seenOrder := make([]string, 0, len(snapshot.Signals))
	for _, stat := range snapshot.Signals {
		seenOrder = append(seenOrder, stat.Signal)
	}
	want := strings.Join([]string{
		string(GuardSignalCreatedTimeout), string(GuardSignalEvidenceTimeout),
		string(GuardSignalEmptyStream), string(GuardSignalWithhold),
	}, ",")
	if strings.Join(seenOrder, ",") != want {
		t.Fatalf("signal order = %v", seenOrder)
	}
}

// TestGuardExemptOrderCoversQualityExemptTokens 锁定展示序与豁免常量同集：
// 新 token 若未进 guardExemptOrder，recordExempt 会静默丢弃，管理端看不见。
func TestGuardExemptOrderCoversQualityExemptTokens(t *testing.T) {
	t.Parallel()
	constants := []string{
		QualityExemptDisabled,
		QualityExemptSkipInput,
		QualityExemptOperation,
		QualityExemptCompaction,
		QualityExemptProvider,
		QualityExemptModelScope,
		QualityExemptMessagesNoThink,
		QualityExemptModelNoReasoning,
	}
	if len(guardExemptOrder) != len(constants) {
		t.Fatalf("guardExemptOrder len=%d constants=%d", len(guardExemptOrder), len(constants))
	}
	got := make(map[string]struct{}, len(guardExemptOrder))
	for _, reason := range guardExemptOrder {
		if _, dup := got[reason]; dup {
			t.Fatalf("duplicate %q", reason)
		}
		got[reason] = struct{}{}
	}
	for _, reason := range constants {
		if _, ok := got[reason]; !ok {
			t.Fatalf("guardExemptOrder missing %q", reason)
		}
	}
}

// TestQualityHoldExemptReasonPaths 锁定豁免原因与 hold 判定的一致性：
// reason=="" 当且仅当 shouldHoldQualityStream 为真，各豁免路径返回专属 token。
func TestQualityHoldExemptReasonPaths(t *testing.T) {
	cfg := QualityRetryRuntime{Enabled: true}
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}
	input := Input{Streaming: true, PublicModel: "grok-4.6", Body: []byte("{}")}

	if reason := qualityHoldExemptReason(input, nil, route, audit.OperationChat, cfg); reason != "" {
		t.Fatalf("reasoning-capable chat must hold, got %q", reason)
	}
	injected := input
	injected.Body = []byte(`{"tools":[{"type":"web_search"},{"type":"x_search"}],"tool_choice":"none"}`)
	if reason := qualityHoldExemptReason(injected, nil, route, audit.OperationChat, cfg); reason != "" {
		t.Fatalf("injected search tools must still hold, got %q", reason)
	}
	off := cfg
	off.Enabled = false
	if reason := qualityHoldExemptReason(input, nil, route, audit.OperationChat, off); reason != QualityExemptDisabled {
		t.Fatalf("disabled = %q", reason)
	}
	skip := input
	skip.skipQualityHold = true
	if reason := qualityHoldExemptReason(skip, nil, route, audit.OperationChat, cfg); reason != QualityExemptSkipInput {
		t.Fatalf("skip input = %q", reason)
	}
	for _, op := range []audit.Operation{
		audit.OperationImage, audit.OperationImageEdit, audit.OperationVideo,
		audit.OperationTTS, audit.OperationSTT, audit.OperationRealtime, audit.OperationVoice,
		audit.OperationCompaction,
	} {
		if reason := qualityHoldExemptReason(input, nil, route, op, cfg); reason != QualityExemptOperation {
			t.Fatalf("non-reasoning op %q = %q, want operation", op, reason)
		}
	}
	tui := input
	tui.Body = []byte(`{"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`)
	if reason := qualityHoldExemptReason(tui, nil, route, audit.OperationResponses, cfg); reason != QualityExemptCompaction {
		t.Fatalf("tui compaction body = %q", reason)
	}
	tuiSkip := tui
	tuiSkip.skipQualityHold = true
	if reason := qualityHoldExemptReason(tuiSkip, nil, route, audit.OperationResponses, cfg); reason != QualityExemptSkipInput {
		t.Fatalf("CreateResponse TUI skip must win over body token, got %q", reason)
	}
	webRoute := modeldomain.Route{Provider: accountdomain.ProviderWeb, UpstreamModel: "grok-4.6"}
	if reason := qualityHoldExemptReason(input, nil, webRoute, audit.OperationChat, cfg); reason != QualityExemptProvider {
		t.Fatalf("web provider = %q", reason)
	}
	// reasoning_disabled 豁免已删除：白名单内模型（4.5/4.6）
	// 不支持 none，显式关闭是非法组合——不再豁免，照常进守卫（上游会以
	// 400 拒绝该请求，守卫判决无从发生，也不会误罚账号）。
	none := Input{Streaming: true, PublicModel: "grok-4.6", Body: []byte("{\"reasoning_effort\":\"none\"}")}
	if reason := qualityHoldExemptReason(none, nil, route, audit.OperationChat, cfg); reason != "" {
		t.Fatalf("explicit none on none-incapable model must stay gated, got %q", reason)
	}
	// 模型白名单：名单外模型整体豁免（model_out_of_scope），先于其他判定。
	scoped := cfg
	scoped.GuardedModels = []string{"grok-4.5", "grok-4.6"}
	if reason := qualityHoldExemptReason(none, nil, route, audit.OperationChat, scoped); reason != "" {
		t.Fatalf("in-scope model must stay gated, got %q", reason)
	}
	outOfScope := Input{Streaming: true, PublicModel: "grok-4.3", Body: []byte("{}")}
	route43 := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.3", PublicID: "grok-4.3"}
	if reason := qualityHoldExemptReason(outOfScope, nil, route43, audit.OperationChat, scoped); reason != QualityExemptModelScope {
		t.Fatalf("out-of-scope model = %q", reason)
	}
	// 档位后缀别名按前缀归入基模型（grok-4.6 覆盖 grok-4.6-xhigh）。
	alias := Input{Streaming: true, PublicModel: "grok-4.6-xhigh", Body: []byte("{}")}
	if reason := qualityHoldExemptReason(alias, nil, route, audit.OperationChat, scoped); reason != "" {
		t.Fatalf("effort-suffixed alias of in-scope model = %q, want gated", reason)
	}
	// 修正：流式 messages 未请求 thinking 照常 hold（转换器以
	// ThinkingEvidenceComment 保留证据）；仅非流式保留豁免。
	if reason := qualityHoldExemptReason(input, nil, route, audit.OperationMessages, cfg); reason != "" {
		t.Fatalf("streaming messages without thinking = %q, want gated", reason)
	}
	nonStream := Input{Streaming: false, PublicModel: "grok-4.6", Body: input.Body}
	if reason := qualityHoldExemptReason(nonStream, nil, route, audit.OperationChat, cfg); reason != "" {
		t.Fatalf("non-stream chat must hold, got %q", reason)
	}
	if reason := qualityHoldExemptReason(nonStream, nil, route, audit.OperationMessages, cfg); reason != QualityExemptMessagesNoThink {
		t.Fatalf("non-stream messages without thinking = %q", reason)
	}
	nonReasoningInput := Input{Streaming: true, PublicModel: "not-a-grok-model", Body: []byte("{}")}
	nonReasoning := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "not-a-grok-model", PublicID: "not-a-grok-model"}
	if reason := qualityHoldExemptReason(nonReasoningInput, nil, nonReasoning, audit.OperationChat, cfg); reason != QualityExemptModelNoReasoning {
		t.Fatalf("non-reasoning model = %q", reason)
	}
	if shouldHoldQualityStream(input, nil, route, audit.OperationChat, cfg) != (qualityHoldExemptReason(input, nil, route, audit.OperationChat, cfg) == "") {
		t.Fatal("bool projection must mirror the reason function")
	}
}
