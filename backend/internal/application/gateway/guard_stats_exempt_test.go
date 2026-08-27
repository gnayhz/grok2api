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
	collector.recordExempt(QualityExemptReasoningOff)
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
	if got := byReason[QualityExemptReasoningOff].Count; got != 1 {
		t.Fatalf("reasoning_disabled count = %d", got)
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
		string(GuardSignalHeaderBudget), string(GuardSignalCreatedTimeout), string(GuardSignalEvidenceTimeout),
		string(GuardSignalEmptyStream), string(GuardSignalWithhold), string(GuardSignalTerminalBurst),
	}, ",")
	if strings.Join(seenOrder, ",") != want {
		t.Fatalf("signal order = %v", seenOrder)
	}
}

// TestQualityHoldExemptReasonPaths 锁定豁免原因与 hold 判定的一致性：
// reason=="" 当且仅当 shouldHoldQualityStream 为真，各豁免路径返回专属 token。
func TestQualityHoldExemptReasonPaths(t *testing.T) {
	cfg := QualityRetryRuntime{Enabled: true, MinOutputTokens: 8}
	route := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}
	input := Input{Streaming: true, PublicModel: "grok-4.6", Body: []byte("{}")}

	if reason := qualityHoldExemptReason(input, nil, route, audit.OperationChat, cfg); reason != "" {
		t.Fatalf("reasoning-capable chat must hold, got %q", reason)
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
	if reason := qualityHoldExemptReason(input, nil, route, audit.OperationImage, cfg); reason != QualityExemptOperation {
		t.Fatalf("image = %q", reason)
	}
	webRoute := modeldomain.Route{Provider: accountdomain.ProviderWeb, UpstreamModel: "grok-4.6"}
	if reason := qualityHoldExemptReason(input, nil, webRoute, audit.OperationChat, cfg); reason != QualityExemptProvider {
		t.Fatalf("web provider = %q", reason)
	}
	none := Input{Streaming: true, PublicModel: "grok-4.6", Body: []byte("{\"reasoning_effort\":\"none\"}")}
	if reason := qualityHoldExemptReason(none, nil, route, audit.OperationChat, cfg); reason != QualityExemptReasoningOff {
		t.Fatalf("reasoning none = %q", reason)
	}
	if reason := qualityHoldExemptReason(input, nil, route, audit.OperationMessages, cfg); reason != QualityExemptMessagesNoThink {
		t.Fatalf("messages without thinking = %q", reason)
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
