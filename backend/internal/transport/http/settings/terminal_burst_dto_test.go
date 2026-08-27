package settings

import (
	"encoding/json"
	"strings"
	"testing"

	settingsapp "github.com/chenyme/grok2api/backend/internal/application/settings"
)

func TestRequestRetryTerminalBurstRoundTrip(t *testing.T) {
	response := newSettingsResponse(settingsapp.Snapshot{Config: settingsapp.EditableConfig{RequestRetry: settingsapp.RequestRetryEditable{
		TerminalBurstThreshold: 5,
	}}})
	data, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	want := "\"terminalBurstThreshold\":5"
	if !strings.Contains(string(data), want) {
		t.Fatalf("settings response lost terminalBurstThreshold: %s", data)
	}

	applied := settingsConfigDTO{RequestRetry: &requestRetryConfigDTO{
		TerminalBurstThreshold: 7,
	}}.toApplication()
	if applied.RequestRetry.TerminalBurstThreshold != 7 {
		t.Fatalf("toApplication dropped terminalBurstThreshold: %+v", applied.RequestRetry)
	}
	if !applied.RequestRetryProvided {
		t.Fatal("RequestRetry section presence flag not set")
	}
}
