package gateway

import (
	"testing"

	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
)

// fakeJurisdiction 管辖缝隙测试伪实现(条目="渠道:模型"或裸模型)。
type fakeJurisdiction struct{ models map[string]bool }

func (f fakeJurisdiction) Jurisdiction(provider, model string) bool {
	return f.models[model] || f.models[provider+":"+model]
}

// TestModelJurisdictionSeamOverridesFileList 锚定 G13:质量层管辖缝隙注入后,
// 勾选清单(而非文件白名单)成为扣留范围权威;未注入时沿用文件白名单。
func TestModelJurisdictionSeamOverridesFileList(t *testing.T) {
	input := Input{PublicModel: "grok-4.5", Streaming: true,
		Body: []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)}
	route := modeldomain.Route{Provider: "grok_build", UpstreamModel: "grok-4.5"}
	cfg := QualityRetryRuntime{Enabled: true, GuardedModels: []string{"grok-4.5"}}

	// 缝隙未注入:文件白名单生效(grok-4.5 在名单内,应介入)。
	if reason := qualityHoldExemptReason(input, nil, route, "chat", cfg, nil); reason == QualityExemptModelScope {
		t.Fatalf("未注入缝隙时文件白名单应生效, got exempt=%s", reason)
	}

	// 缝隙注入且模型不在勾选清单:豁免(勾选清单是权威)。
	seam := fakeJurisdiction{models: map[string]bool{"grok-4.6": true}}
	if reason := qualityHoldExemptReason(input, nil, route, "chat", cfg, seam); reason != QualityExemptModelScope {
		t.Fatalf("缝隙管辖外模型应豁免, got exempt=%s", reason)
	}

	// 缝隙注入且模型在勾选清单:介入(即使文件白名单不含它)。
	cfgOther := QualityRetryRuntime{Enabled: true, GuardedModels: []string{"grok-3"}}
	seamIn := fakeJurisdiction{models: map[string]bool{"grok-4.5": true}}
	if reason := qualityHoldExemptReason(input, nil, route, "chat", cfgOther, seamIn); reason == QualityExemptModelScope {
		t.Fatalf("缝隙管辖内模型应介入, got exempt=%s", reason)
	}

	// 清单为空(守卫关闭态):全部豁免。
	seamEmpty := fakeJurisdiction{models: map[string]bool{}}
	if reason := qualityHoldExemptReason(input, nil, route, "chat", cfg, seamEmpty); reason != QualityExemptModelScope {
		t.Fatalf("空管辖(守卫关闭)应全部豁免, got exempt=%s", reason)
	}
}

// TestModelJurisdictionScopedByProvider 锚定 G13 渠道限定:条目
// "grok_build:grok-4.5" 只拦 build 渠道,console 同名模型豁免。
func TestModelJurisdictionScopedByProvider(t *testing.T) {
	input := Input{PublicModel: "grok-4.5", Streaming: true,
		Body: []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)}
	cfg := QualityRetryRuntime{Enabled: true}
	seam := fakeJurisdiction{models: map[string]bool{"grok_build:grok-4.5": true}}

	buildRoute := modeldomain.Route{Provider: "grok_build", UpstreamModel: "grok-4.5"}
	if reason := qualityHoldExemptReason(input, nil, buildRoute, "chat", cfg, seam); reason == QualityExemptModelScope {
		t.Fatal("渠道限定的 build 条目应拦下 build 请求")
	}

	consoleRoute := modeldomain.Route{Provider: "grok_console", UpstreamModel: "grok-4.5"}
	if reason := qualityHoldExemptReason(input, nil, consoleRoute, "chat", cfg, seam); reason != QualityExemptModelScope {
		t.Fatal("渠道限定的 build 条目不应拦 console 同名模型")
	}
}
