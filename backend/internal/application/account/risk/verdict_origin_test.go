package risk

import (
	"context"
	"testing"
	"time"
)

// 对账重放必须遵守通道隔离:Build 降智产生的 verdict(OriginAccountID=build)
// 重放后果只打到该 Build,绝不连坐 verdict 键所在的 Web 身份。
// 此前对账传 degradedID=webID,每次重启都把 Web 补标成风控(线上 7 个 Web
// 被误标的根因)。
func TestReconcileRespectsVerdictOrigin(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	// Build 91 降智产生的 verdict,键在 web 90。
	if err := store.SaveRiskVerdict(context.Background(), 90, StoredVerdict{
		Verdict: VerdictDenied, Source: "sso_probe", OriginAccountID: 91,
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.ReconcileRiskyVerdicts(context.Background())

	if !accounts.flagged[91] {
		t.Fatal("origin build 91 must be flagged by reconcile")
	}
	if accounts.flagged[90] {
		t.Fatal("web 90 must NOT be flagged: verdict origin is build 91 (channel-scoped replay)")
	}
}

// 旧数据(origin=0)保持原语义:重放到 webID。
func TestReconcileLegacyVerdictFallsBackToWebID(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	if err := store.SaveRiskVerdict(context.Background(), 90, StoredVerdict{
		Verdict: VerdictDenied, Source: "sso_probe",
		CheckedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.ReconcileRiskyVerdicts(context.Background())

	if !accounts.flagged[90] {
		t.Fatal("legacy verdict (origin=0) must fall back to flagging the web identity")
	}
}

// 事件路径写入的 verdict 必须携带触发源:web 90 的 Build 91 降智探测后,
// verdict.OriginAccountID = 91。
func TestCheckNowStampsOriginAccount(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)

	verdict := service.checkNow(context.Background(), 90, 91, "")

	if verdict.OriginAccountID != 91 {
		t.Fatalf("verdict.OriginAccountID = %d, want 91", verdict.OriginAccountID)
	}
	stored, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil || stored.OriginAccountID != 91 {
		t.Fatalf("stored origin = %+v err=%v, want 91", stored, err)
	}
}
