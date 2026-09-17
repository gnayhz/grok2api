package court

import (
	"context"
	"encoding/json"
	"net"
	"regexp"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

// TestVerdictEvidenceExplainableReadback 锚定 I25(每次羁押/定罪必须
// 挂案件号且证据可解释):定罪结案后 q_case.evidence_json 可读回且
// 含规则标识与单元计数;同时验证 I24(键控引用,无 IP 明文/账号名)。
func TestVerdictEvidenceExplainableReadback(t *testing.T) {
	evidenceConfig := model.DefaultEvidenceConfig()
	bench := newBenchWithEvidenceConfig(t, evidenceConfig)
	cfg := DefaultConfig()
	probeStore := registry.NewProbeTaskStore(bench.registry)
	for _, node := range []uint64{1, 2, 3} {
		for account := uint64(2); account <= 5; account++ {
			recordObs(t, bench, account, node, model.OutcomeDelivered)
		}
		recordObs(t, bench, 1, node, model.OutcomeDegraded)
	}
	courtService := newFixtureCourt(cfg, bench.registry, storeSource{store: bench.evidence}, simpleTaskDispatcher{store: probeStore})
	defer courtService.Close(context.Background())
	caseID := openSimpleTestCase(t, courtService, bench.registry)
	settleSimpleTestTasks(t, bench.registry, caseID, func(task model.ProbeTaskView) model.ProbeTaskResult {
		if task.Direction == model.ProbeExitJury {
			return model.ProbeTaskResult{Outcome: model.ProbeResultClean, Detail: "jury_clean"}
		}
		return model.ProbeTaskResult{Outcome: model.ProbeResultDegraded, VerifiedIPChange: true, Detail: "differential_degraded"}
	})
	if _, err := courtService.Evaluate(context.Background(), time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Status       string
		Verdict      string
		EvidenceJSON string `gorm:"column:evidence_json"`
	}
	if err := bench.registry.DB().Table("q_case").
		Where("verdict <> ''").Order("id").Take(&row).Error; err != nil {
		t.Fatalf("定罪案件必须留档: %v", err)
	}
	evidenceJSON := row.EvidenceJSON
	if row.Verdict != string(model.VerdictAccountGuilty) {
		t.Fatalf("裁决 = %s", row.Verdict)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(evidenceJSON), &payload); err != nil {
		t.Fatalf("证据链必须可读回: %v", err)
	}
	// 可解释性:规则标识+去重单元计数+组范围。
	if payload["rule"] != "account_quality_pattern" {
		t.Fatalf("证据缺规则标识: %v", payload)
	}
	if _, ok := payload["assessment"].(map[string]any); !ok {
		t.Fatalf("证据缺降智出口计数: %v", payload)
	}
	// I24:证据链不含 IP 明文(键控引用)——原始字符串无点分十进制。
	if ipLike(evidenceJSON) {
		t.Fatalf("证据链不得含 IP 明文: %s", evidenceJSON)
	}
}

// ipLike 粗检点分十进制 IPv4 形态。
func ipLike(s string) bool {
	for _, candidate := range regexp.MustCompile(`(?:[0-9]{1,3}\.){3}[0-9]{1,3}`).FindAllString(s, -1) {
		if net.ParseIP(candidate) != nil {
			return true
		}
	}
	return false
}
