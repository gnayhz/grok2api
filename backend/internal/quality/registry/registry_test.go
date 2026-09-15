package registry

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
)

func openTestRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, err := Open(context.Background(), Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatalf("打开测试登记处: %v", err)
	}
	t.Cleanup(func() { _ = registry.Close() })
	return registry
}

// TestOpenCreatesNineTables 锚定 B3:九张 q_ 表全部建成。
func TestOpenCreatesNineTables(t *testing.T) {
	registry := openTestRegistry(t)
	want := []string{
		"q_account_state", "q_exit_state", "q_identity_group", "q_case",
		"q_case_party", "q_observation", "q_degrade_ledger", "q_ip_epoch",
		"q_probe_task",
	}
	for _, table := range want {
		if !registry.db.Migrator().HasTable(table) {
			t.Fatalf("缺少质量层表: %s", table)
		}
	}
}

// TestDefaultEligibility 锚定 B2:未涉案账号/出口缺席即默认可调度。
func TestDefaultEligibility(t *testing.T) {
	registry := openTestRegistry(t)
	if !registry.AccountEligible(42) {
		t.Fatal("无行账号必须默认 ACTIVE 可调度")
	}
	if !registry.ExitEligible(7) {
		t.Fatal("无行出口必须默认 AVAILABLE 可调度")
	}
	if registry.CurrentEpoch(7) != 0 {
		t.Fatal("无探测档案节点 epoch 必须为 0")
	}
}

// TestExitTransitionRejectsInvisibleEpoch 锚定 I15:未建 IP 档案的节点当前
// epoch 默认为 0,不能把 epoch 1 写成调度资格谓词永远看不到的状态。
func TestExitTransitionRejectsInvisibleEpoch(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{
		NodeID: 7, Epoch: 1, To: model.ExitRemanded, CaseID: 100,
	}); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("未知节点的非零 epoch 必须被拒, got %v", err)
	}
	if !registry.ExitEligible(7) {
		t.Fatal("拒绝不可见 epoch 后节点仍应按 epoch 0 可调度")
	}
	var rows int64
	if err := registry.DB().Model(&qExitStateModel{}).Where("node_id = ?", 7).Count(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("拒绝不可见 epoch 不得写入状态行, got %d", rows)
	}
}

// TestNodeQualityArchivesUseLatestEpochWithoutGrouping 锚定节点质量投影:
// 每个节点只返回最新 epoch,且无质量状态时显式返回 available。
func TestNodeQualityArchivesUseLatestEpochWithoutGrouping(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	if err := registry.RecordExitIdentity(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.2")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := registry.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.3")); err != nil {
		t.Fatal(err)
	}
	views, err := registry.ListNodeIPArchives(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].NodeID != 7 || views[0].CurrentEpoch != 2 || views[0].CurrentIP != "198.51.100.3" {
		t.Fatalf("节点质量投影必须取最新 epoch, got %+v", views)
	}
	if views[0].State != model.ExitAvailable || views[0].DegradeDetail == nil {
		t.Fatalf("无状态节点投影必须显式 available/空数组, got %+v", views[0])
	}
}

// TestDirectPlaceholderNeverEntersExitState 锚定 B1.2 实现边界:直连节点
// 0 没有节点身份,不进入出口状态机、IP epoch 或质量台账。
func TestDirectPlaceholderNeverEntersExitState(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 0, To: model.ExitRemanded, CaseID: 100}); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("直连占位节点不得羁押, got %v", err)
	}
	if _, _, err := registry.AdvanceEpoch(ctx, 0, model.ExitIdentityFromAggregate("198.51.100.1")); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("直连占位节点不得翻 epoch, got %v", err)
	}
	if err := registry.RecordExitIdentity(ctx, 0, model.ExitIdentityFromAggregate("198.51.100.1")); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("直连占位节点不得建 IP 档案, got %v", err)
	}
	if err := registry.AppendDegrade(ctx, 0, 0, "", time.Now().UTC()); !errors.Is(err, ErrInvalidNode) {
		t.Fatalf("直连占位节点不得写质量台账, got %v", err)
	}
}

// TestAccountRemandReleaseErase 锚定 T1/T2 + I12(未定罪即无痕):
// 羁押写行挂案件,无罪释放删行——像没发生过。
func TestAccountRemandReleaseErase(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	// I25:羁押必须挂案件号。
	err := registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 1, To: model.AccountRemanded})
	if !errors.Is(err, ErrCaseRequired) {
		t.Fatalf("无案件号羁押必须被拒, got %v", err)
	}
	if err := registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 1, To: model.AccountRemanded, CaseID: 100}); err != nil {
		t.Fatal(err)
	}
	if registry.AccountEligible(1) {
		t.Fatal("羁押账号不可调度")
	}
	if entry := registry.AccountState(1); entry.CurrentCaseID != 100 || entry.State != model.AccountRemanded {
		t.Fatalf("羁押条目 = %+v", entry)
	}
	if err := registry.ReleaseAccountErase(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if !registry.AccountEligible(1) {
		t.Fatal("无罪释放后必须回池")
	}
	var rows int64
	registry.db.Model(&qAccountStateModel{}).Count(&rows)
	if rows != 0 {
		t.Fatalf("无罪释放必须抹除状态行(未定罪即无痕), 剩 %d 行", rows)
	}
}

// TestProbeTaskPersistsDifferentialBaseline 锚定调查任务的双路径投影:
// 重启/认领/面板读取都必须保留原始基线与对比目标,不能只剩一个含义
// 不明的 DefendantNodeID。
func TestProbeTaskPersistsDifferentialBaseline(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	store := NewProbeTaskStore(registry)
	id, err := store.CreateProbeTask(ctx, model.ProbeTask{
		CaseID: 41, Direction: model.ProbeAccountDifferential, DefendantAccountID: 7,
		DefendantNodeID: 107, DefendantEpoch: 139,
		BaselineNodeID: 116, BaselineEpoch: 0,
	})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimPendingProbeTasks(ctx, 1)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("claim=%+v err=%v", claimed, err)
	}
	if claimed[0].ID != id || claimed[0].BaselineNodeID != 116 || claimed[0].BaselineEpoch != 0 ||
		claimed[0].DefendantNodeID != 107 || claimed[0].DefendantEpoch != 139 {
		t.Fatalf("双路径字段未从库恢复: %+v", claimed[0])
	}
	views, err := store.ListProbeTasks(ctx, 10)
	if err != nil || len(views) != 1 {
		t.Fatalf("views=%+v err=%v", views, err)
	}
	if views[0].BaselineNodeID != 116 || views[0].BaselineEpoch != 0 || views[0].NodeID != 107 || views[0].Epoch != 139 {
		t.Fatalf("面板双路径字段未保留: %+v", views[0])
	}
	if err := store.CompleteProbeTask(ctx, id, model.ProbeDone, model.ProbeTaskResult{
		Outcome: model.ProbeResultDegraded, VerifiedIPChange: true, Detail: "verified",
		Attempt:        attemptmeta.Identity{ID: "main/1", Revision: 5, Path: attemptmeta.Path{NodeID: 107, Epoch: 139}},
		ControlAttempt: attemptmeta.Identity{ID: "control/1", Revision: 5},
	}, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	views, err = store.ListProbeTasks(ctx, 10)
	if err != nil || len(views) != 1 || !views[0].VerifiedIPChange {
		t.Fatalf("差分 IP 验证标志必须持久化: views=%+v err=%v", views, err)
	}
	caseViews, err := store.ListProbeTasksForCase(ctx, claimed[0].CaseID)
	if err != nil || len(caseViews) != 1 || views[0].Attempt.ID != "main/1" || views[0].Attempt.Path.Epoch != 139 || caseViews[0].ControlAttempt.ID != "control/1" || caseViews[0].ControlAttempt.Revision != 5 {
		t.Fatalf("lost physical identities: %v/%v, %v", views, caseViews, err)
	}
}

// TestExitRemandBanAndEpochFlip 锚定 I15 + 统一 ban 律:
// ban 后 epoch 翻篇(IP 变化)即解禁,新 epoch 不继承旧嫌疑;
// 过期 epoch 的转移被拒。
func TestExitRemandBanAndEpochFlip(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 4, To: model.ExitRemanded, CaseID: 50}); err != nil {
		t.Fatal(err)
	}
	if registry.ExitEligible(4) {
		t.Fatal("羁押出口不可调度")
	}
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 4, To: model.ExitBanned, CaseID: 50}); err != nil {
		t.Fatal(err)
	}
	if registry.ExitEligible(4) {
		t.Fatal("BANNED 出口不可调度")
	}
	// IP 变化:epoch 1 → 解禁。
	newEpoch, released, err := registry.AdvanceEpoch(ctx, 4, model.ExitIdentityFromAggregate("203.0.113.9"))
	if err != nil {
		t.Fatal(err)
	}
	if newEpoch != 1 || len(released) != 1 || released[0] != (model.EpochKey{NodeID: 4, Epoch: 0}) {
		t.Fatalf("epoch 翻篇 = %d, released = %v", newEpoch, released)
	}
	if !registry.ExitEligible(4) {
		t.Fatal("IP 变化即解禁(统一 ban 律)")
	}
	// 旧 epoch 的裁决不得追新 IP。
	err = registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 4, Epoch: 0, To: model.ExitBanned, CaseID: 50})
	if !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("过期 epoch 转移必须被拒, got %v", err)
	}
	// 档案可查。
	archive, err := registry.ExitIPArchive(ctx, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(archive) != 1 || archive[0].Epoch != 1 || archive[0].IP != "203.0.113.9" {
		t.Fatalf("IP 档案 = %+v", archive)
	}
}

// TestStickyRemandAutoReleaseOnEpoch 锚定 B1.2 决议3:粘性羁押
// epoch 翻篇自动解除(与 ban 解禁同路径)。
func TestStickyRemandAutoReleaseOnEpoch(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	// 建立 epoch 历史:首见(0)→两次翻篇(1,2)。
	if err := registry.RecordExitIdentity(ctx, 9, model.ExitIdentityFromAggregate("198.51.100.1")); err != nil {
		t.Fatal(err)
	}
	for _, ip := range []string{"198.51.100.2", "198.51.100.3"} {
		if _, _, err := registry.AdvanceEpoch(ctx, 9, model.ExitIdentityFromAggregate(ip)); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 9, Epoch: 2, To: model.ExitRemanded, CaseID: 60}); err != nil {
		t.Fatal(err)
	}
	if _, released, err := registry.AdvanceEpoch(ctx, 9, model.ExitIdentityFromAggregate("198.51.100.7")); err != nil {
		t.Fatal(err)
	} else if len(released) != 1 || released[0].Epoch != 2 {
		t.Fatalf("翻篇应释放 epoch 2 羁押, got %v", released)
	}
	if !registry.ExitEligible(9) || registry.CurrentEpoch(9) != 3 {
		t.Fatal("翻篇后出口必须可用且 epoch=3")
	}
}

// TestCurrentExitStatesProjection 锚定批8 可见性:快照投影只含当前
// epoch 的非可用出口(稀疏表示),翻篇后旧 epoch 不再出现。
// TestCurrentAccountStatesProjection 锚定账号质量徽章数据面:稀疏
// 投影只含非 ACTIVE 行(ACTIVE/缺席不占)——面板一眼只看到被裁决亭
// 动过的账号。
func TestCurrentAccountStatesProjection(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	if err := registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 7, To: model.AccountRemanded, CaseID: 60}); err != nil {
		t.Fatal(err)
	}
	if err := registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 7, To: model.AccountSentenced, CaseID: 60}); err != nil {
		t.Fatal(err)
	}
	if err := registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 8, To: model.AccountRemanded, CaseID: 61}); err != nil {
		t.Fatal(err)
	}
	states := registry.CurrentAccountStates()
	if len(states) != 2 || states[7].State != model.AccountSentenced || states[8].State != model.AccountRemanded {
		t.Fatalf("投影 = %+v", states)
	}
	if states[7].CurrentCaseID != 60 {
		t.Fatalf("服刑投影须带案件号: %+v", states[7])
	}
	if err := registry.ReleaseAccountErase(ctx, 8); err != nil {
		t.Fatal(err)
	}
	states = registry.CurrentAccountStates()
	if _, stuck := states[8]; stuck {
		t.Fatal("抹除后不得留在投影")
	}
}

func TestCurrentExitStatesProjection(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 5, To: model.ExitRemanded, CaseID: 60}); err != nil {
		t.Fatal(err)
	}
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 6, To: model.ExitRemanded, CaseID: 61}); err != nil {
		t.Fatal(err)
	}
	if err := registry.TransitionExit(ctx, ExitTransitionRequest{NodeID: 6, To: model.ExitBanned, CaseID: 61}); err != nil {
		t.Fatal(err)
	}
	states := registry.CurrentExitStates()
	if len(states) != 2 || states[5].State != model.ExitRemanded || states[6].State != model.ExitBanned {
		t.Fatalf("投影 = %+v", states)
	}
	if _, _, err := registry.AdvanceEpoch(ctx, 5, model.ExitIdentityFromAggregate("198.51.100.1")); err != nil {
		t.Fatal(err)
	}
	states = registry.CurrentExitStates()
	if _, stuck := states[5]; stuck {
		t.Fatal("翻篇后旧羁押不得留在投影")
	}
	if _, banned := states[6]; !banned {
		t.Fatal("未翻篇节点应保留在投影")
	}
}

// TestRegistryRestartRebuildsCache 锚定 I17:库为真相源——重启后
// 热缓存从库重建,已提交状态不丢。
func TestRegistryRestartRebuildsCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.db")
	ctx := context.Background()
	first, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatal(err)
	}
	if err := first.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 77, To: model.AccountRemanded, CaseID: 900}); err != nil {
		t.Fatal(err)
	}
	if err := first.TransitionExit(ctx, ExitTransitionRequest{NodeID: 88, To: model.ExitRemanded, CaseID: 900}); err != nil {
		t.Fatal(err)
	}
	if err := first.TransitionExit(ctx, ExitTransitionRequest{NodeID: 88, To: model.ExitBanned, CaseID: 900}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := first.AdvanceEpoch(ctx, 12, model.ExitIdentityFromAggregate("192.0.2.4")); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := Open(ctx, Options{Driver: "sqlite", SQLitePath: path})
	if err != nil {
		t.Fatalf("重启重开: %v", err)
	}
	defer second.Close()
	if second.AccountEligible(77) {
		t.Fatal("重启后羁押状态必须保留")
	}
	if second.ExitEligible(88) {
		t.Fatal("重启后 ban 状态必须保留")
	}
	if second.CurrentEpoch(12) != 1 {
		t.Fatal("重启后 epoch 档案必须保留")
	}
}

// TestIdentityGroups 锚定 I14(身份组=连坐作用域)+ B3 决议2:
// 关联账号共享组号;未关联账号自成一组;组号确定性。
func TestIdentityGroups(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	// Graph behavior is tested through facts supplied by the account owner.
	registry.accountLinks = identityLinksStub{{AccountID: 1, RelatedAccountID: 2}, {AccountID: 1, RelatedAccountID: 3}}
	if err := registry.RefreshIdentityGroups(ctx); err != nil {
		t.Fatal(err)
	}
	groupID, members := registry.IdentityGroupOf(1)
	if len(members) != 3 {
		t.Fatalf("连通分量必须合并 {1,2,3}, got %v", members)
	}
	for _, member := range []uint64{2, 3} {
		if other, otherMembers := registry.IdentityGroupOf(member); other != groupID || len(otherMembers) != 3 {
			t.Fatalf("成员 %d 组号不一致: %d vs %d", member, other, groupID)
		}
	}
	soloID, soloMembers := registry.IdentityGroupOf(7)
	if soloID != 7 || len(soloMembers) != 1 {
		t.Fatalf("未关联账号必须自成一组, got %d %v", soloID, soloMembers)
	}
	// 组号确定性:同成员集合重算不变。
	if err := registry.RefreshIdentityGroups(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := registry.IdentityGroupOf(1); again != groupID {
		t.Fatal("组号必须确定性(重启稳定)")
	}
	// 持久化检查。
	var rows int64
	registry.db.Model(&qIdentityGroupModel{}).Count(&rows)
	if rows != 3 {
		t.Fatalf("多成员组应持久化 3 行, got %d", rows)
	}
}

// TestIdentityGroupHashStable 组号哈希对成员集合稳定、对顺序不敏感。
func TestIdentityGroupHashStable(t *testing.T) {
	a := identityGroupID([]uint64{3, 1, 2})
	b := identityGroupID([]uint64{1, 2, 3})
	if a != b || a == 0 {
		t.Fatalf("组号必须非零且稳定: %d vs %d", a, b)
	}
	if identityGroupID([]uint64{1, 2}) == identityGroupID([]uint64{1, 3}) {
		t.Fatal("不同成员集合必须不同组号")
	}
}

// TestDegradeLedger 锚定 G8/B3 决议3:台账按 (节点,epoch,IP) 聚合,
// 永久保留,不影响调度。
func TestDegradeLedger(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	at := time.Now().UTC()
	for i := 0; i < 3; i++ {
		if err := registry.AppendDegrade(ctx, 6, 1, "192.0.2.1", at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.AppendDegrade(ctx, 6, 2, "192.0.2.2", at); err != nil {
		t.Fatal(err)
	}
	total, err := registry.NodeDegradeTotal(ctx, 6)
	if err != nil || total != 4 {
		t.Fatalf("节点总降智 = %d, %v", total, err)
	}
	history, err := registry.ListNodeDegradeHistory(ctx, 6, 10)
	if err != nil || len(history) != 2 {
		t.Fatalf("IP 明细行数 = %d, %v", len(history), err)
	}
	if history[0].Count != 3 || history[1].Count != 1 {
		t.Fatalf("聚合计数错误: %+v", history)
	}
	// 台账不影响调度资格。
	if !registry.ExitEligible(6) {
		t.Fatal("台账是流行病学数据,不影响调度资格")
	}
}

// TestCaseStorage 案件与当事方存取原语。
func TestCaseStorage(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	caseID, err := registry.CreateCase(ctx, time.Now().UTC(), "{}")
	if err != nil || caseID == 0 {
		t.Fatalf("立案 = %d, %v", caseID, err)
	}
	if err := registry.UpsertParty(ctx, PartyRecord{
		CaseID: caseID, Kind: model.PartyAccount, AccountID: 10, Role: model.RoleDefendant,
		Disposition: model.DispositionRemanded,
	}); err != nil {
		t.Fatal(err)
	}
	// 幂等 upsert。
	if err := registry.UpsertParty(ctx, PartyRecord{
		CaseID: caseID, Kind: model.PartyAccount, AccountID: 10, Role: model.RoleDefendant,
		Disposition: model.DispositionReleased,
	}); err != nil {
		t.Fatal(err)
	}
	parties, err := registry.ListParties(ctx, caseID)
	if err != nil || len(parties) != 1 || parties[0].Disposition != model.DispositionReleased {
		t.Fatalf("当事方 = %+v, %v", parties, err)
	}
	if err := registry.CloseCase(ctx, caseID, model.CaseExitGuilty, model.VerdictExitGuilty, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	record, ok, err := registry.GetCase(ctx, caseID)
	if err != nil || !ok || record.Status != model.CaseExitGuilty || record.ClosedAt == nil {
		t.Fatalf("案件 = %+v, %v, %v", record, ok, err)
	}
}

// TestConcurrentEligibilityReadsDuringTransition 转移进行中的并发读
// 必须始终有效(不可变快照,无锁读)。
func TestConcurrentEligibilityReadsDuringTransition(t *testing.T) {
	registry := openTestRegistry(t)
	ctx := context.Background()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = registry.TransitionAccount(ctx, AccountTransitionRequest{AccountID: 99, To: model.AccountRemanded, CaseID: uint64(i + 1)})
			_ = registry.ReleaseAccountErase(ctx, 99)
		}
	}()
	for i := 0; i < 2000; i++ {
		if !registry.AccountEligible(99) && registry.AccountState(99).State != model.AccountRemanded {
			t.Fatal("读到了不一致状态")
		}
	}
	<-done
}

type identityLinksStub []account.IdentityLink

func (links identityLinksStub) ListIdentityLinks(context.Context) ([]account.IdentityLink, error) {
	return links, nil
}
