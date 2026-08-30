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
func (f *fakeAccounts) SetAccountRiskAttribution(_ context.Context, id uint64, flagged bool, trigger string, origin uint64, detail string, checkedAt time.Time) error {
	return f.SetAccountRiskStatus(context.Background(), id, flagged)
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

func (f *fakeStore) DeleteCleanVerdictsExceptSources(_ context.Context, keepSources ...string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	keep := make(map[string]struct{}, len(keepSources))
	for _, source := range keepSources {
		keep[source] = struct{}{}
	}
	var removed int64
	for id, v := range f.verdicts {
		if v.Verdict != VerdictClean {
			continue
		}
		if _, ok := keep[v.Source]; !ok {
			delete(f.verdicts, id)
			removed++
		}
	}
	return removed, nil
}

func (f *fakeStore) MostRecentCleanVerdict(_ context.Context, source string, maxAge time.Duration) (uint64, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var bestID uint64
	var bestAt time.Time
	cutoff := time.Now().UTC().Add(-maxAge)
	for id, v := range f.verdicts {
		if v.Verdict != VerdictClean || v.Source != source {
			continue
		}
		if maxAge > 0 && v.CheckedAt.Before(cutoff) {
			continue
		}
		if v.CheckedAt.After(bestAt) {
			bestID, bestAt = id, v.CheckedAt
		}
	}
	return bestID, bestID != 0, nil
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
		Verdict:        VerdictDenied,
		BotFlagDetails: "policy=deny,risk=0.86,event=" + string(rune(36)) + "registration",
		HTTPStatus:     200,
		// 真实探针恒填 CheckedAt;缺失会让 DeniedTTL/ErrorRetry 的新鲜期
		// 判定把零值时间当作远古结论(测试夹具必须与线上一致)。
		CheckedAt: time.Now().UTC(),
	}
}

func cleanResult() CheckResult {
	return CheckResult{Verdict: VerdictClean, HTTPStatus: 200, CheckedAt: time.Now().UTC()}
}

func baseTestConfig() Config {
	// DeniedConfirmations:1 保持存量用例的"单次即定罪"语义;确认计数
	// 的新语义由 denied_confirmation_test.go 单独覆盖(默认 2)。
	return Config{Enabled: true, Concurrency: 2, Timeout: time.Second, OnDenied: "disable", PatrolInterval: 30 * 24 * time.Hour, ErrorRetry: time.Hour, DeniedConfirmations: 1}
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
	// 处置通道隔离：只停用实际降智的 Build 7；Web 90 与另一 Build 8
	// 不级联（它们的降智事件会独立归因）。
	if reason, disabled := accounts.disabled[7]; !disabled || reason != "registration risk (RSC)" {
		t.Fatalf("degraded build 7 not disabled (%v)", reason)
	}
	for _, id := range []uint64{90, 8} {
		if _, disabled := accounts.disabled[id]; disabled {
			t.Fatalf("account %d must not be cascaded", id)
		}
	}
}

// TestAttributionDeniedFlagsDegradedChannelOnly：onDenied=flag 保留启用状
// 态、仅打长期风控标记，且只作用于实际降智的通道账号——不级联身份组。
func TestAttributionDeniedFlagsDegradedChannelOnly(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedWeb[7] = 90
	accounts.linkedBack[90] = []uint64{7, 8}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	service.AttributeNow(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild})

	if !accounts.flagged[7] {
		t.Fatal("degraded build 7 must be risk-flagged")
	}
	if store.verdicts[90].Trigger != accountdomain.RiskTriggerDegrade {
		t.Fatalf("degrade verdict trigger = %q, want degrade", store.verdicts[90].Trigger)
	}
	for _, id := range []uint64{90, 8} {
		if accounts.flagged[id] {
			t.Fatalf("account %d must not be cascaded (channel-scoped flagging)", id)
		}
		if _, disabled := accounts.disabled[id]; disabled {
			t.Fatalf("account %d must stay enabled", id)
		}
	}
}

// TestPatrolTickAppliesConsequences：巡检发现的 clean→denied 迁移必须立即
// 落地为账号动作，而不是只写 verdict 表等下一次请求路径扣留。SSO 巡检
// denied 连坐同一身份组的 Web/Build/Console。
func TestPatrolTickAppliesConsequences(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	checker := &fakeChecker{result: deniedResult()}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, checker, nil)

	service.PatrolTick(context.Background(), []uint64{90})

	for _, id := range []uint64{90, 7, 55} {
		if !accounts.flagged[id] {
			t.Fatalf("patrol SSO denied must flag identity-group member %d, flagged=%v", id, accounts.flagged)
		}
	}
	if accounts.flagged[8] {
		t.Fatal("unrelated account 8 must not be cascaded")
	}
	if store.verdicts[90].Trigger != accountdomain.RiskTriggerPatrol {
		t.Fatalf("patrol verdict trigger = %q, want patrol", store.verdicts[90].Trigger)
	}
}

// TestPatrolTickDeniedDisablesIdentityGroup：onDenied=disable 时 SSO 巡检
// denied 同样连坐身份组，请求路径降智仍保持通道隔离（见 disable_console_test）。
func TestPatrolTickDeniedDisablesIdentityGroup(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7, 8}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	service := New(baseTestConfig(), accounts, store, &fakeChecker{result: deniedResult()}, nil)

	service.PatrolTick(context.Background(), []uint64{90})

	for _, id := range []uint64{90, 7, 8, 55} {
		if _, disabled := accounts.disabled[id]; !disabled {
			t.Fatalf("patrol SSO denied must disable identity-group member %d, disabled=%v", id, accounts.disabled)
		}
	}
}

// TestPatrolTickCleanDoesNotTouchSiblings：巡检 clean 只说明 SSO 身份无辜，
// 不得给关联通道打标/停用。
func TestPatrolTickCleanDoesNotTouchSiblings(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.PatrolTick(context.Background(), []uint64{90})

	if len(accounts.flagged) != 0 {
		t.Fatalf("clean patrol must flag nobody, flagged=%v", accounts.flagged)
	}
	if len(accounts.disabled) != 0 {
		t.Fatalf("clean patrol must disable nobody, disabled=%v", accounts.disabled)
	}
}

// TestWebDegradeDeniedCascadesIdentityGroup：SSO 通道自己降智且探针 denied
// 与巡检同属身份级信号，必须连坐 Build/Console；否则 SSO 已标风控后
// Build 无法再走探针归因。
func TestWebDegradeDeniedCascadesIdentityGroup(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.token[90] = "sso-token"
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: deniedResult()}, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 90, Provider: accountdomain.ProviderWeb}, 0)

	for _, id := range []uint64{90, 7, 55} {
		if !accounts.flagged[id] {
			t.Fatalf("SSO-origin denied must flag identity-group member %d, flagged=%v", id, accounts.flagged)
		}
	}
}

// TestReconcileSSOOriginDeniedFansOut：启动对账重放 origin=webID 的 denied
// 必须把身份组补齐（巡检当时连坐过、或进程重启后新关联的通道）。
func TestReconcileSSOOriginDeniedFansOut(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		90: {Verdict: VerdictDenied, DeniedStreak: 1, OriginAccountID: 90, CheckedAt: time.Now().UTC()},
	}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	service.ReconcileRiskyVerdicts(context.Background())

	for _, id := range []uint64{90, 7, 55} {
		if !accounts.flagged[id] {
			t.Fatalf("reconcile of SSO-origin denied must flag %d, flagged=%v", id, accounts.flagged)
		}
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
		90: {Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: time.Now().UTC()},
		91: {Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: time.Now().UTC()},
	}}
	cfg := baseTestConfig()
	cfg.OnDenied = "flag"
	service := New(cfg, accounts, store, &fakeChecker{result: cleanResult()}, nil)

	// 91 的 Web 已打标（自愈完成态），但关联 Build 8 漂移未标；90 整组未标。
	accounts.flagged[91] = true

	service.ReconcileRiskyVerdicts(context.Background())

	// 对账只收敛 Web 账号本身的标志(通道隔离): 关联 Build 的标志由各自
	// 降智事件独立产生,人工清掉的 Build 标志不会被对账回滚。
	if !accounts.flagged[90] {
		t.Fatal("drifted web 90 must be reconciled")
	}
	for _, id := range []uint64{7, 8} {
		if accounts.flagged[id] {
			t.Fatalf("build %d must not be re-flagged by reconcile (channel-scoped)", id)
		}
	}
	// Console 关联账号同样属于身份组，必须被覆盖。
	accounts2 := newFakeAccounts()
	accounts2.linkedWeb[11] = 95
	accounts2.linkedBack[95] = []uint64{11}
	accounts2.linkedConsole[95] = []uint64{12}
	store2 := &fakeStore{verdicts: map[uint64]StoredVerdict{
		95: {Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: time.Now().UTC()},
	}}
	service2 := New(cfg, accounts2, store2, &fakeChecker{result: cleanResult()}, nil)
	service2.attribute(context.Background(), accountdomain.Credential{ID: 11, Provider: accountdomain.ProviderBuild}, 0)
	if !accounts2.flagged[11] {
		t.Fatal("degraded build 11 must be flagged")
	}
	if accounts2.flagged[12] {
		t.Fatal("linked console must not be cascaded (channel-scoped)")
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
		90: {Verdict: VerdictDenied, DeniedStreak: 1, CheckedAt: time.Now().UTC()},
	}}
	checker := &fakeChecker{result: cleanResult()}
	service := New(baseTestConfig(), accounts, store, checker, nil)

	service.attribute(context.Background(), accountdomain.Credential{ID: 7, Provider: accountdomain.ProviderBuild}, 0)

	if checker.calls.Load() != 0 {
		t.Fatal("fresh risky verdict must not re-check")
	}
	// 通道隔离：缓存结论处置实际降智的 Build 7，不级联 Web 90。
	if _, disabled := accounts.disabled[7]; !disabled {
		t.Fatal("cached denied verdict must still disable the degraded build")
	}
	if _, disabled := accounts.disabled[90]; disabled {
		t.Fatal("cached denied verdict must not cascade onto the web identity")
	}
}

func TestFreshVerdictExpiryMatrix(t *testing.T) {
	now := time.Now().UTC()
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{
		1: {Verdict: VerdictDenied, DeniedStreak: 2, CheckedAt: now.Add(-time.Hour)},
		6: {Verdict: VerdictDenied, DeniedStreak: 2, CheckedAt: now.Add(-1000 * time.Hour)},
		7: {Verdict: VerdictDenied, DeniedStreak: 0, CheckedAt: now.Add(-time.Minute)},
		8: {Verdict: VerdictDenied, DeniedStreak: 0, CheckedAt: now.Add(-2 * time.Hour)},
		2: {Verdict: VerdictClean, CheckedAt: now.Add(-time.Hour)},
		3: {Verdict: VerdictClean, CheckedAt: now.Add(-1000 * time.Hour)},
		4: {Verdict: VerdictError, CheckedAt: now.Add(-time.Minute)},
		5: {Verdict: VerdictError, CheckedAt: now.Add(-2 * time.Hour)},
	}}
	service := New(baseTestConfig(), newFakeAccounts(), store, &fakeChecker{}, nil)
	cases := []struct {
		id   uint64
		want bool
	}{{1, true}, {2, true}, {3, false}, {4, true}, {5, false}, {6, false}, {7, true}, {8, false}}
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
		// 通道隔离语义:该 verdict 由 Build 7 的降智触发(origin=7),清 Build
		// 标必须删除它;否则对账按 origin 重放会把刚清的标打回来。
		90: {Verdict: VerdictDenied, CheckedAt: time.Now().UTC(), OriginAccountID: 7},
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
