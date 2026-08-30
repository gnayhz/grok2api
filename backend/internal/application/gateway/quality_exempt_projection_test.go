package gateway

import (
	"testing"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// TestExemptReasonMatchesBooleanProjection 锁定 qualityHoldExemptReason 与
// shouldHoldQualityStream 的一致性契约：后者是前者的布尔投影，二者必须
// 同步演进（源注释如此约定，这里把它变成机械保证——任何只改了一侧的
// 豁免路径都会立刻在此爆掉）。
func TestExemptReasonMatchesBooleanProjection(t *testing.T) {
	t.Parallel()
	cfg := QualityRetryRuntime{Enabled: true, MaxAttempts: 2}
	buildRoute := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}
	webRoute := modeldomain.Route{Provider: accountdomain.ProviderWeb, UpstreamModel: "grok-4.6"}
	dumbRoute := modeldomain.Route{Provider: accountdomain.ProviderBuild, UpstreamModel: "not-a-grok-model", PublicID: "not-a-grok-model"}
	owned := inferencedomain.ResponseOwnership{ResponseID: "r1", AccountID: 1}
	off := cfg
	off.Enabled = false
	compactionBody := []byte(`{"input":[{"role":"user","content":"` + tuiCompactionPrompt + `"}]}`)
	cases := []struct {
		name      string
		input     Input
		ownership *inferencedomain.ResponseOwnership
		route     modeldomain.Route
		operation audit.Operation
		cfg       QualityRetryRuntime
	}{
		{name: "hold: thinking build chat", input: Input{Streaming: true, PublicModel: "grok-4.6"}, route: buildRoute, operation: audit.OperationChat, cfg: cfg},
		{name: "hold: pinned responses", input: Input{Streaming: true, PublicModel: "grok-4.6"}, ownership: &owned, route: buildRoute, operation: audit.OperationResponses, cfg: cfg},
		{name: "disabled", input: Input{Streaming: true}, route: buildRoute, operation: audit.OperationChat, cfg: off},
		{name: "skip input", input: Input{Streaming: true, skipQualityHold: true}, route: buildRoute, operation: audit.OperationResponses, cfg: cfg},
		{name: "operation", input: Input{Streaming: true}, route: buildRoute, operation: audit.OperationImage, cfg: cfg},
		{name: "compaction body", input: Input{Streaming: true, Body: compactionBody}, route: buildRoute, operation: audit.OperationResponses, cfg: cfg},
		{name: "provider web exempt", input: Input{Streaming: true}, route: webRoute, operation: audit.OperationChat, cfg: cfg},
		{name: "hold: console provider engaged", input: Input{Streaming: true, PublicModel: "grok-4.6"}, route: modeldomain.Route{Provider: accountdomain.ProviderConsole, UpstreamModel: "grok-4.6", PublicID: "grok-4.6"}, operation: audit.OperationChat, cfg: cfg},
		{name: "reasoning off", input: Input{Streaming: true, Body: []byte(`{"reasoning_effort":"none"}`)}, route: buildRoute, operation: audit.OperationChat, cfg: cfg},
		{name: "messages no think", input: Input{Streaming: true, Body: []byte(`{"messages":[]}`)}, route: buildRoute, operation: audit.OperationMessages, cfg: cfg},
		{name: "hold: messages adaptive thinking", input: Input{Streaming: true, Body: []byte(`{"thinking":{"type":"adaptive","budget_tokens":"adaptive"},"messages":[]}`)}, route: buildRoute, operation: audit.OperationMessages, cfg: cfg},
		{name: "model no reasoning", input: Input{Streaming: true, PublicModel: "not-a-grok-model"}, route: dumbRoute, operation: audit.OperationChat, cfg: cfg},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reason := qualityHoldExemptReason(test.input, test.ownership, test.route, test.operation, test.cfg)
			hold := shouldHoldQualityStream(test.input, test.ownership, test.route, test.operation, test.cfg)
			if (reason == "") != hold {
				t.Fatalf("projection drift: reason=%q hold=%t (reason empty must equal hold)", reason, hold)
			}
			if !hold && reason == "" {
				t.Fatalf("no-hold without a reason token is unobservable in guard-stats")
			}
		})
	}
}
