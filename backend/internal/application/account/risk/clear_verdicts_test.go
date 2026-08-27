package risk

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

// 通道隔离下的清标语义:清关联 Build 的标志时,webID 键上的 verdict 只有
// 在 origin==该 Build(其降智触发的 SSO 探测)时才删除;属于 Web 自身的
// 结论必须保留——否则清一次 Build 标就把 Web 的 SSO 缓存一并丢掉,下次
// Web 降智白白多烧一条消息额度。
func TestClearLinkedBuildKeepsWebOwnedVerdict(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedWeb[91] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	// web 90 自己的 SSO 结论(origin==webID),以及 build 91 的原生结论。
	mustSave(t, store, 90, StoredVerdict{Verdict: VerdictDenied, DeniedStreak: 2, Source: "sso_probe", OriginAccountID: 90, CheckedAt: time.Now().UTC()})
	mustSave(t, store, 91, StoredVerdict{Verdict: VerdictDenied, Source: buildProbeSourceTag, OriginAccountID: 91, CheckedAt: time.Now().UTC()})
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)

	accounts.linkedBack[90] = []uint64{91}
	accounts.flagged[91] = false

	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetRiskVerdict(context.Background(), 90); err != nil {
		t.Fatal("SSO-origin denied must survive a linked-build clear so patrol/reconcile still see the identity")
	}
	if _, err := store.GetRiskVerdict(context.Background(), 91); err == nil {
		t.Fatal("build's own native verdict must be removed")
	}

	service.ReconcileRiskyVerdicts(context.Background())
	if !accounts.flagged[91] {
		t.Fatal("kept SSO-origin denied must re-flag the cleared build on reconcile; clear Web to unflag the identity")
	}
}

// 反向:verdict 由该 Build 的降智产生(origin==build)时,清 Build 标必须
// 连同该 verdict 删除,否则启动对账按 origin 重放会把刚清的标打回来。
func TestClearLinkedBuildRemovesBuildOriginatedVerdict(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedWeb[91] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	mustSave(t, store, 90, StoredVerdict{Verdict: VerdictDenied, Source: "sso_probe", OriginAccountID: 91, CheckedAt: time.Now().UTC()})
	mustSave(t, store, 91, StoredVerdict{Verdict: VerdictDenied, Source: buildProbeSourceTag, OriginAccountID: 91, CheckedAt: time.Now().UTC()})
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)

	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetRiskVerdict(context.Background(), 90); err == nil {
		t.Fatal("build-originated verdict (origin==build) must be removed with the flag")
	}
	if _, err := store.GetRiskVerdict(context.Background(), 91); err == nil {
		t.Fatal("build's own native verdict must be removed")
	}
}

// 删除失败必须上抛:denied 永久新鲜,静默吞错会让"清标"在下次降智时被
// 原样打回,操作者却以为已经清干净。
func TestClearLinkedBuildPropagatesDeleteFailure(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedWeb[91] = 90
	store := &failingDeleteStore{fakeStore{verdicts: map[uint64]StoredVerdict{}, mu: sync.Mutex{}}}
	mustSave(t, &store.fakeStore, 90, StoredVerdict{Verdict: VerdictDenied, Source: "sso_probe", OriginAccountID: 91, CheckedAt: time.Now().UTC()})
	mustSave(t, &store.fakeStore, 91, StoredVerdict{Verdict: VerdictDenied, Source: buildProbeSourceTag, OriginAccountID: 91, CheckedAt: time.Now().UTC()})
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)

	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}); err == nil {
		t.Fatal("delete failure must propagate, not be swallowed")
	}
}

// legacy verdict(origin==0)键在 webID:重放退回 webID 语义,不针对该
// Build,清 Build 标时保留。
func TestClearLinkedBuildKeepsLegacyVerdict(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedWeb[91] = 90
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	mustSave(t, store, 90, StoredVerdict{Verdict: VerdictDenied, Source: "sso_probe", CheckedAt: time.Now().UTC()})
	service := New(baseBuildTestConfig(), accounts, store, &fakeChecker{}, nil)

	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 91, Provider: accountdomain.ProviderBuild}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRiskVerdict(context.Background(), 90); err != nil {
		t.Fatal("legacy verdict (origin=0) replays onto webID only and must survive a build-flag clear")
	}
}

func TestClearWebUnflagsIdentityGroup(t *testing.T) {
	accounts := newFakeAccounts()
	accounts.linkedBack[90] = []uint64{7}
	accounts.linkedConsole[90] = []uint64{55}
	accounts.flagged[90] = true
	accounts.flagged[7] = true
	accounts.flagged[55] = true
	store := &fakeStore{verdicts: map[uint64]StoredVerdict{}}
	mustSave(t, store, 90, StoredVerdict{Verdict: VerdictDenied, OriginAccountID: 90, CheckedAt: time.Now().UTC()})
	service := New(baseTestConfig(), accounts, store, &fakeChecker{}, nil)

	if err := service.ClearIdentityVerdicts(context.Background(), accountdomain.Credential{ID: 90, Provider: accountdomain.ProviderWeb}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []uint64{7, 55} {
		if accounts.flagged[id] {
			t.Fatalf("clearing web must unflag identity-group member %d", id)
		}
	}
	if _, err := store.GetRiskVerdict(context.Background(), 90); err == nil {
		t.Fatal("web verdict must be deleted")
	}
}

func mustSave(t *testing.T, store *fakeStore, id uint64, verdict StoredVerdict) {
	t.Helper()
	if err := store.SaveRiskVerdict(context.Background(), id, verdict); err != nil {
		t.Fatal(err)
	}
}

// failingDeleteStore 让删除 webID 键失败,其余委托内嵌 fakeStore。
type failingDeleteStore struct {
	fakeStore
}

func (f *failingDeleteStore) GetRiskVerdict(ctx context.Context, id uint64) (relationalVerdict, error) {
	return f.fakeStore.GetRiskVerdict(ctx, id)
}
func (f *failingDeleteStore) SaveRiskVerdict(ctx context.Context, id uint64, v StoredVerdict) error {
	return f.fakeStore.SaveRiskVerdict(ctx, id, v)
}
func (f *failingDeleteStore) DeleteRiskVerdict(_ context.Context, id uint64) error {
	if id == 90 {
		return errors.New("store down")
	}
	return f.fakeStore.DeleteRiskVerdict(context.Background(), id)
}
func (f *failingDeleteStore) ListRiskyVerdictAccountIDs(ctx context.Context) ([]uint64, error) {
	return f.fakeStore.ListRiskyVerdictAccountIDs(ctx)
}
func (f *failingDeleteStore) ListRiskyVerdictAccountIDsAfter(ctx context.Context, afterID uint64) ([]uint64, error) {
	return f.fakeStore.ListRiskyVerdictAccountIDsAfter(ctx, afterID)
}
func (f *failingDeleteStore) DeleteCleanVerdictsExceptSources(ctx context.Context, keepSources ...string) (int64, error) {
	return f.fakeStore.DeleteCleanVerdictsExceptSources(ctx, keepSources...)
}
func (f *failingDeleteStore) MostRecentCleanVerdict(ctx context.Context, source string, maxAge time.Duration) (uint64, bool, error) {
	return f.fakeStore.MostRecentCleanVerdict(ctx, source, maxAge)
}
