package settings

import (
	"encoding/json"
	"strings"
	"testing"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
)

// 锁定三个新字段(probeProxyURL/deniedConfirmations/deniedTTL)在设置面的
// 双向透传。2026-08-28 事故复盘:首版实现只加了应用层,漏了传输层 DTO,
// 字段在管理 UI 上不可见、保存即丢。
func TestAccountRiskNewFieldsRoundTrip(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{AccountRisk: settingsapp.AccountRiskEditable{
		ProbeProxyURL: "socks5://127.0.0.1:1080", DeniedConfirmations: 3,
	}}})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"probeProxyURL":"socks5://127.0.0.1:1080"`, `"deniedConfirmations":3`} {
		if !strings.Contains(string(data), want) {
			t.Fatalf("settings response lost %s: %s", want, data)
		}
	}

	applied := settingsConfigDTO{AccountRisk: &accountRiskConfigDTO{
		ProbeProxyURL: "socks5://10.0.0.1:1080", DeniedConfirmations: 4, DeniedTTL: "48h",
	}}.toApplication()
	if applied.AccountRisk.ProbeProxyURL != "socks5://10.0.0.1:1080" ||
		applied.AccountRisk.DeniedConfirmations != 4 || applied.AccountRisk.DeniedTTL != "48h" {
		t.Fatalf("toApplication dropped new fields: %+v", applied.AccountRisk)
	}
	if !applied.AccountRiskProvided {
		t.Fatal("AccountRisk section presence flag not set")
	}
}
