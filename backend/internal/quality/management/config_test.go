package management

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOnlyLifecycleBoundsAreConfigurable(t *testing.T) {
	base := DefaultConfig()
	if _, _, err := normalize(base); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"", "0s", "-1m", "invalid"} {
		for _, field := range []string{"retention", "evidence_window", "investigation_timeout"} {
			input := base
			switch field {
			case "retention":
				input.Retention = value
			case "evidence_window":
				input.EvidenceWindow = value
			case "investigation_timeout":
				input.InvestigationTimeout = value
			}
			if _, _, err := normalize(input); err == nil {
				t.Fatalf("accepted %s=%q", field, value)
			}
		}
	}
	value, err := decodePersisted([]byte(`{"account_need_exits":0,"retention":"","probe_budget":-1,"exit_need_k":999}`))
	if err != nil || value != base {
		t.Fatalf("retired thresholds affected current settings: %+v %v", value, err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, retired := range []string{"account_need_exits", "account_span_nodes", "exit_need_n", "exit_need_k", "differential_exits", "jurors_per_exit", "probe_budget"} {
		if strings.Contains(string(payload), retired) {
			t.Fatalf("retired field emitted: %s", retired)
		}
	}
}
func TestPersistedSettingsRejectInvalidLifecycleAndFutureSchema(t *testing.T) {
	for _, payload := range []string{`{"_schema_version":2,"retention":""}`, `{"_schema_version":99}`, `{"evidence_window":"invalid"}`} {
		if _, err := decodePersisted([]byte(payload)); err == nil {
			t.Fatalf("accepted %s", payload)
		}
	}
}
