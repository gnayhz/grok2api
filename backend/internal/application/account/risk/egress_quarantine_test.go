package risk

import (
	"context"
	"testing"
	"time"
)

type recordingQuarantiner struct{ calls []quarantineCall }

type quarantineCall struct {
	nodeID    uint64
	accountID uint64
}

func (r *recordingQuarantiner) ClearDegradeEvidence(uint64) {}

func (r *recordingQuarantiner) QuarantineForExitIP(_ context.Context, nodeID, degradedAccountID uint64) {
	r.calls = append(r.calls, quarantineCall{nodeID: nodeID, accountID: degradedAccountID})
}

func newQuarantineTestService(t *testing.T) *Service {
	t.Helper()
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: cleanResult()}
	return New(baseTestConfig(), accounts, store, checker, nil)
}

// RSC clean 结论 + 节点 ID → 交给出口层隔离（IP 嫌疑）。
func TestCleanVerdictQuarantinesExitIPNode(t *testing.T) {
	service := newQuarantineTestService(t)
	quarantiner := &recordingQuarantiner{}
	service.SetEgressQuarantiner(quarantiner)
	verdict := StoredVerdict{Verdict: VerdictClean, CheckedAt: time.Now().UTC()}
	service.applyConsequences(context.Background(), 7, 90, verdict, 42)
	if len(quarantiner.calls) != 1 || quarantiner.calls[0].nodeID != 42 || quarantiner.calls[0].accountID != 7 {
		t.Fatalf("quarantine calls = %+v", quarantiner.calls)
	}
}

// clean 但没有节点信息（direct/untraced）→ 不触发出口隔离。
func TestCleanVerdictWithoutNodeSkipsQuarantine(t *testing.T) {
	service := newQuarantineTestService(t)
	quarantiner := &recordingQuarantiner{}
	service.SetEgressQuarantiner(quarantiner)
	verdict := StoredVerdict{Verdict: VerdictClean, CheckedAt: time.Now().UTC()}
	service.applyConsequences(context.Background(), 7, 90, verdict, 0)
	if len(quarantiner.calls) != 0 {
		t.Fatalf("quarantine calls = %+v", quarantiner.calls)
	}
}

// RISK 结论只处置账号，不动出口节点。
func TestDeniedVerdictNeverQuarantines(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)
	quarantiner := &recordingQuarantiner{}
	service.SetEgressQuarantiner(quarantiner)
	verdict := StoredVerdict{Verdict: VerdictDenied, CheckedAt: time.Now().UTC()}
	service.applyConsequences(context.Background(), 7, 90, verdict, 42)
	if len(quarantiner.calls) != 0 {
		t.Fatalf("quarantine calls = %+v", quarantiner.calls)
	}
}
