package risk

import (
	"context"
	"testing"
	"time"
)

// 生产事故回归:首批巡检密集探测被整批降级服务,单次 denied
// 直接定罪+连坐,7 个健康身份被永久打标。新语义:连续确认(默认 2)才处置,
// 未确认 denied 在 ErrorRetry 后重探,clean 复测覆盖旧 denied 自愈。
func TestDeniedRequiresConfirmationsBeforeConsequences(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	cfg := baseTestConfig()
	cfg.DeniedConfirmations = 2 // 默认语义(基线夹具保持 1 供存量用例)
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: deniedResult()}, nil)

	service.PatrolTick(context.Background(), []uint64{90})
	if accounts.flagged[90] {
		t.Fatal("first denied must not flag before reaching confirmations")
	}
	verdict, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil || verdict.Verdict != VerdictDenied || verdict.DeniedStreak != 1 {
		t.Fatalf("after first denial verdict = %+v err=%v (want denied streak 1)", verdict, err)
	}

	service.PatrolTick(context.Background(), []uint64{90})
	if !accounts.flagged[90] {
		t.Fatal("second consecutive denied must flag")
	}
	verdict, _ = store.GetRiskVerdict(context.Background(), 90)
	if verdict.DeniedStreak != 2 {
		t.Fatalf("streak after second denial = %d (want 2)", verdict.DeniedStreak)
	}
}

// 单次误读后的 clean 复测必须覆盖 denied 并归零连击——误判自愈路径。
func TestCleanReprobeOverwritesUnconfirmedDenied(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	cfg := baseTestConfig()
	cfg.DeniedConfirmations = 2
	service := New(cfg, accounts, store, &fakeChecker{result: deniedResult()}, nil)

	service.PatrolTick(context.Background(), []uint64{90})
	service.UpdateChecker(&fakeChecker{result: cleanResult()}, "sso_probe")
	service.PatrolTick(context.Background(), []uint64{90})

	verdict, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil || verdict.Verdict != VerdictClean || verdict.DeniedStreak != 0 {
		t.Fatalf("clean must overwrite unconfirmed denied, got %+v err=%v", verdict, err)
	}
	if accounts.flagged[90] {
		t.Fatal("identity must never be flagged on this path")
	}
}

// 已确认 denied 在 DeniedTTL 过期后不再新鲜(允许重探自愈);未确认
// denied 只在 ErrorRetry 内防抖,过后可重探补确认;此前 denied 永久新鲜。
func TestDeniedFreshnessWindows(t *testing.T) {
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	cfg := baseTestConfig()
	cfg.DeniedConfirmations = 2
	cfg.DeniedTTL = 24 * time.Hour
	cfg.ErrorRetry = time.Hour
	service := New(cfg, newFakeAccounts(), store, &fakeChecker{result: cleanResult()}, nil)
	ctx := context.Background()
	now := time.Now().UTC()

	store.verdicts[90] = StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 2, CheckedAt: now.Add(-time.Hour)}
	if _, fresh := service.freshVerdict(ctx, 90); !fresh {
		t.Fatal("confirmed denied within DeniedTTL must stay fresh")
	}
	store.verdicts[90] = StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 2, CheckedAt: now.Add(-25 * time.Hour)}
	if _, fresh := service.freshVerdict(ctx, 90); fresh {
		t.Fatal("confirmed denied past DeniedTTL must allow re-probe (self-heal)")
	}
	store.verdicts[90] = StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: now.Add(-10 * time.Minute)}
	if _, fresh := service.freshVerdict(ctx, 90); !fresh {
		t.Fatal("unconfirmed denied within ErrorRetry must debounce")
	}
	store.verdicts[90] = StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: now.Add(-2 * time.Hour)}
	if _, fresh := service.freshVerdict(ctx, 90); fresh {
		t.Fatal("unconfirmed denied past ErrorRetry must be re-probeable")
	}
}

// 旧数据/单次 denied(streak 0)不得被启动对账重放处置——历史
// 误判批次在新版上清标后,重启不会再被 reconcile 重新打标。
func TestReconcileSkipsUnconfirmedLegacyDenied(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	store.verdicts[90] = StoredVerdict{Verdict: VerdictDenied, CheckedAt: time.Now().UTC()} // streak 0(旧行)
	cfg := baseTestConfig()
	cfg.DeniedConfirmations = 2
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.ReconcileRiskyVerdicts(context.Background())
	if accounts.flagged[90] {
		t.Fatal("unconfirmed legacy denied must not be re-flagged by reconcile")
	}
}

// 自愈闭环:已确认 denied 被 rsc_denied 排除调度后,OnDegraded
// 永远不会再触发;DeniedTTL 过期必须由巡检重探,且 clean 必须真正清标
// (含 SSO 连坐组成员),否则 TTL 只是 freshness 数字、误判永不恢复。
func TestCleanPatrolUnflagsConfirmedDeniedIdentityGroup(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{8}
	accounts.flagged[90] = true
	accounts.flagged[7] = true
	accounts.flagged[8] = true
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		90: {Verdict: VerdictDenied, DeniedStreak: 2, OriginAccountID: 90, Source: "sso_probe", CheckedAt: time.Now().UTC().Add(-25 * time.Hour)},
	}}
	cfg := baseTestConfig()
	cfg.DeniedConfirmations = 2
	cfg.DeniedTTL = 24 * time.Hour
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.PatrolTick(context.Background(), []uint64{90})

	verdict, err := store.GetRiskVerdict(context.Background(), 90)
	if err != nil || verdict.Verdict != VerdictClean {
		t.Fatalf("clean must overwrite confirmed denied, got %+v err=%v", verdict, err)
	}
	for _, id := range []uint64{90, 7, 8} {
		if accounts.flagged[id] {
			t.Fatalf("SSO-origin clean must unflag identity-group member %d", id)
		}
	}
}

// 请求路径通道隔离:Build 降智得到 clean 只解 Build,不得把 SSO 上的
// 既有 rsc_denied 一并摘掉(那是另一条定罪,解组必须走 SSO-origin clean)。
func TestChannelScopedCleanUnflagsOnlyDegradedAccount(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.flagged[7] = true
	accounts.flagged[90] = true
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, &fakeStore{verdicts: map[uint64]StoredVerdict{}}, &fakeChecker{result: cleanResult()}, nil)

	service.applyConsequences(context.Background(), 7, 90, StoredVerdict{
		Verdict: VerdictClean, OriginAccountID: 7, CheckedAt: time.Now().UTC(),
	}, 0)

	if accounts.flagged[7] {
		t.Fatal("channel-scoped clean must unflag the degraded account")
	}
	if !accounts.flagged[90] {
		t.Fatal("channel-scoped clean must not cascade unflag onto SSO")
	}
}
