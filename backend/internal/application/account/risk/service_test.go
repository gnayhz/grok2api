package risk

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"sync"
	"sync/atomic"
)

type fakeAccounts struct {
	mu            sync.Mutex
	token         map[uint64]string
	cooldown      map[uint64]bool
	cleared       []uint64
	disabled      map[uint64]string
	flagged       map[uint64]bool
	linkedWeb     map[uint64]uint64
	linkedBack    map[uint64][]uint64
	linkedConsole map[uint64][]uint64
}

func newFakeAccounts() *fakeAccounts {
	return &fakeAccounts{
		token: map[uint64]string{}, cooldown: map[uint64]bool{},
		disabled: map[uint64]string{}, flagged: map[uint64]bool{}, linkedWeb: map[uint64]uint64{}, linkedBack: map[uint64][]uint64{}, linkedConsole: map[uint64][]uint64{},
	}
}

func (f *fakeAccounts) DecryptedAccessToken(_ context.Context, id uint64) (string, error) {
	return f.token[id], nil
}
func (f *fakeAccounts) LinkedWebAccountID(_ context.Context, buildID uint64) (uint64, bool, error) {
	web, ok := f.linkedWeb[buildID]
	return web, ok, nil
}
func (f *fakeAccounts) LinkedBuildAccountIDs(_ context.Context, webID uint64) ([]uint64, error) {
	return f.linkedBack[webID], nil
}
func (f *fakeAccounts) LinkedConsoleAccountIDs(_ context.Context, webID uint64) ([]uint64, error) {
	return f.linkedConsole[webID], nil
}
func (f *fakeAccounts) SetAccountEnabled(_ context.Context, id uint64, enabled bool, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if enabled {
		delete(f.disabled, id)
		return nil
	}
	f.disabled[id] = reason
	return nil
}
func (f *fakeAccounts) SetAccountRiskStatus(_ context.Context, id uint64, flagged bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if flagged {
		f.flagged[id] = true
	} else {
		delete(f.flagged, id)
	}
	return nil
}
func (f *fakeAccounts) GetAccountRiskStatus(_ context.Context, id uint64) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.flagged[id] {
		return "rsc_denied", nil
	}
	return "", nil
}
func (f *fakeAccounts) ClearMissingThinkingCooldown(_ context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleared = append(f.cleared, id)
	f.cooldown[id] = false
	return nil
}

type fakeStore struct {
	mu       sync.Mutex
	saves    atomic.Int32
	verdicts map[uint64]StoredVerdict
}

func (f *fakeStore) GetRiskVerdict(_ context.Context, id uint64) (relationalVerdict, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if v, ok := f.verdicts[id]; ok {
		return v, nil
	}
	return StoredVerdict{}, ErrNotFound
}

func (f *fakeStore) SaveRiskVerdict(_ context.Context, id uint64, v StoredVerdict) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves.Add(1)
	f.verdicts[id] = v
	return nil
}
func (f *fakeStore) DeleteRiskVerdict(_ context.Context, id uint64) error {
	delete(f.verdicts, id)
	return nil
}

func (f *fakeStore) ListRiskyVerdictAccountIDs(_ context.Context) ([]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []uint64
	for id, v := range f.verdicts {
		if v.Verdict == VerdictDenied || v.Verdict == VerdictFlagged {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, nil
}

func (f *fakeStore) ListRiskyVerdictAccountIDsAfter(ctx context.Context, afterID uint64) ([]uint64, error) {
	ids, err := f.ListRiskyVerdictAccountIDs(ctx)
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		if id > afterID {
			return ids[i:], nil
		}
	}
	return nil, nil
}

type fakeChecker struct {
	result CheckResult
	calls  atomic.Int32
}

func (f *fakeChecker) Check(context.Context, string) CheckResult {
	f.calls.Add(1)
	return f.result
}

func deniedResult() CheckResult {
	return CheckResult{
		Verdict: VerdictDenied, BotFlagSource: 1,
		BotFlagDetails: "policy=deny,risk=0.86,event=" + string(rune(36)) + "registration",
		RiskScore:      0.86, HTTPStatus: 200,
	}
}

func cleanResult() CheckResult { return CheckResult{Verdict: VerdictClean, HTTPStatus: 200} }

func baseTestConfig() Config {
	return Config{Enabled: true, Concurrency: 2, Timeout: time.Second, OnDenied: "disable", PatrolInterval: 30 * 24 * time.Hour, ErrorRetry: time.Hour}
}

func TestAttributionDeniedDisablesIdentity(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7, 8}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if checker.calls.Load() != 1 {
		t.Fatalf("checker calls = %d", checker.calls.Load())
	}
	if store.verdicts[90].Verdict != VerdictDenied {
		t.Fatalf("stored verdict = %#v", store.verdicts[90])
	}
	for _, id := range []uint64{90, 7, 8} {
		if reason, disabled := accounts.disabled[id]; !disabled || reason != "registration risk (RSC)" {
			t.Fatalf("account %d not disabled (%v)", id, reason)
		}
	}
}

// TestAttributionDeniedFlagsIdentity：onDenied=flag 保留启用状态，仅打
// 长期风控标记——与 disable 不同，账号真实可用状态不被篡改。
func TestAttributionDeniedFlagsIdentity(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7, 8}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	for _, id := range []uint64{90, 7, 8} {
		if !accounts.flagged[id] {
			t.Fatalf("account %d not risk-flagged", id)
		}
		if _, disabled := accounts.disabled[id]; disabled {
			t.Fatalf("account %d must stay enabled under flag mode", id)
		}
	}
}

// TestPatrolTickAppliesConsequences：巡检发现的 clean→denied 迁移必须立即
// 落地为账号动作，而不是只写 verdict 表等下一次请求路径扣留。
func TestPatrolTickAppliesConsequences(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	service.PatrolTick(context.Background(), []uint64{90})

	if !accounts.flagged[90] || !accounts.flagged[7] {
		t.Fatalf("patrol denied must flag web identity and linked builds: web=%v build=%v", accounts.flagged[90], accounts.flagged[7])
	}
}

// TestReconcileRiskyVerdictsFlagsDrifted：verdict 表（真源）有 denied 而账
// 号未打标（旧二进制只落库未处置的窗口），启动对账必须补上处置并传播到
// 关联 Build；已打标账号的未标关联 Build 也要收敛。
func TestReconcileRiskyVerdictsFlagsDrifted(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedBack[91] = []uint64{8}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		90: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC()},
		91: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC()},
	}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	// 91 的 Web 已打标（自愈完成态），但关联 Build 8 漂移未标；90 整组未标。
	accounts.flagged[91] = true

	service.ReconcileRiskyVerdicts(context.Background())

	for _, id := range []uint64{90, 7, 8} {
		if !accounts.flagged[id] {
			t.Fatalf("drifted account %d must be reconciled", id)
		}
	}
	// Console 关联账号同样属于身份组，必须被覆盖。
	accounts2 := newFakeAccounts()
	accounts2.linkedWeb[11] = 95
	accounts2.linkedBack[95] = []uint64{11}
	accounts2.linkedConsole[95] = []uint64{12}
	store2 := &fakeStore{verdicts: map[uint64]StoredVerdict{
		95: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC()},
	}}
	service2 := New(cfg, accounts2, store2, &fakeChecker{result: cleanResult()}, nil)
	service2.attribute(context.Background(), accountdomain.Credential{ID: 11, Provider: accountdomain.ProviderBuild}, 0)
	if !accounts2.flagged[12] {
		t.Fatal("linked console account must be flagged with the identity group")
	}
	if !accounts.flagged[91] {
		t.Fatal("already-flagged identity must stay flagged")
	}
}

func TestAttributionCleanClearsCooldown(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	accounts.cooldown[7] = true
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	service := New(baseTestConfig(), accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if len(accounts.cleared) != 1 || accounts.cleared[0] != 7 {
		t.Fatalf("cleared = %v", accounts.cleared)
	}
	if len(accounts.disabled) != 0 {
		t.Fatalf("clean verdict must disable nothing, disabled=%v", accounts.disabled)
	}
}

func TestAttributionUnlinkedSkipsCheck(t *testing.T) {
	accounts := newFakeAccounts()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if checker.calls.Load() != 0 {
		t.Fatal("unlinked account must not trigger an RSC check")
	}
	if len(accounts.disabled) != 0 {
		t.Fatal("unlinked account must not be disabled by attribution")
	}
}

func TestCachedRiskySkipsCheck(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		90: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC()},
	}}
	checker := &fakeChecker{result: cleanResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if checker.calls.Load() != 0 {
		t.Fatal("fresh risky verdict must not re-check")
	}
	if _, disabled := accounts.disabled[90]; !disabled {
		t.Fatal("cached denied verdict must still disable")
	}
}

func TestFreshVerdictExpiryMatrix(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		1: {Verdict: VerdictDenied, CheckedAt: now.Add(-1000 * time.Hour)},
		2: {Verdict: VerdictClean, CheckedAt: now.Add(-time.Hour)},
		3: {Verdict: VerdictClean, CheckedAt: now.Add(-1000 * time.Hour)},
		4: {Verdict: VerdictError, CheckedAt: now.Add(-time.Minute)},
		5: {Verdict: VerdictError, CheckedAt: now.Add(-2 * time.Hour)},
	}}
	service := New(baseTestConfig(), newFakeAccounts(), store, &fakeChecker{}, nil)
	cases := []struct {
		id   uint64
		want bool
	}{{1, true}, {2, true}, {3, false}, {4, true}, {5, false}}
	for _, testCase := range cases {
		if _, fresh := service.freshVerdict(context.Background(), testCase.id); fresh != testCase.want {
			t.Fatalf("id=%d fresh=%v want=%v", testCase.id, fresh, testCase.want)
		}
	}
}

func TestRiskyVerdictMatrix(t *testing.T) {
	if !(StoredVerdict{Verdict: VerdictDenied}).Risky() || !(StoredVerdict{Verdict: VerdictFlagged}).Risky() {
		t.Fatal("denied/flagged must be risky")
	}
	if (StoredVerdict{Verdict: VerdictClean}).Risky() || (StoredVerdict{Verdict: VerdictError}).Risky() {
		t.Fatal("clean/error must not be risky")
	}
	_ = errors.New
}

// 人工解除闭环:清除 risk_status 时必须级联删除身份组 verdict——
// denied/flagged 永久 fresh, 不删会被启动对账与后续降智事件自动回滚
// (此前全仓库无任何删除路径, 运维只能手工改库)。
func TestClearIdentityVerdictsRemovesRiskyVerdict(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		90: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC()},
	}}
	service := New(baseTestConfig(), accounts, store, &fakeChecker{}, nil)

	// Build 账号入口:身份解析到 webID=90 后删除其 verdict。
	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRiskVerdict(context.Background(), 90); err != ErrNotFound {
		t.Fatalf("verdict not deleted: %v", err)
	}
	// 删除后不再 fresh:下一次降智事件会重新检测而不是重放旧结论。
	if _, fresh := service.freshVerdict(context.Background(), 90); fresh {
		t.Fatalf("deleted verdict still reported fresh")
	}
	// Web 账号入口同样生效(直接身份)。
	store.verdicts[90] = StoredVerdict{Verdict: VerdictFlagged, CheckedAt: time.Now().UTC()}
	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 90, Provider: accountdomain.ProviderWeb}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRiskVerdict(context.Background(), 90); err != ErrNotFound {
		t.Fatalf("web-entry verdict not deleted: %v", err)
	}
	// 未链接账号(Console 单独身份)是无害 no-op。
	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 11, Provider: accountdomain.ProviderConsole}); err != nil {
		t.Fatal(err)
	}
}
