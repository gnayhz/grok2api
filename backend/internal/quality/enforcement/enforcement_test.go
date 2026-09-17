package enforcement

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
)

type memIPSource struct {
	current  map[uint64]string
	revision uint64
}

func (s memIPSource) CurrentExitIdentity(_ context.Context, nodeID uint64) (model.ExitIdentity, uint64, bool, error) {
	ip, ok := s.current[nodeID]
	return model.ExitIdentityFromAggregate(ip), max(1, s.revision), ok && ip != "", nil
}

type memRotator struct {
	mu    chan struct{}
	calls []uint64
	fail  error
	limit int
}

func newMemRotator() *memRotator {
	return &memRotator{mu: make(chan struct{}, 1)}
}

func (r *memRotator) TriggerRotation(_ context.Context, nodeID uint64) error {
	r.mu <- struct{}{}
	defer func() { <-r.mu }()
	if r.limit > 0 && len(r.calls) >= r.limit {
		return ErrRateLimited
	}
	if r.fail != nil {
		return r.fail
	}
	r.calls = append(r.calls, nodeID)
	return nil
}

type memNodes struct {
	profiles []proxy.NodeProfile
}

func (s memNodes) ListProfiles(context.Context) ([]proxy.NodeProfile, error) { return s.profiles, nil }
func (s memNodes) Profile(_ context.Context, nodeID uint64) (proxy.NodeProfile, bool, error) {
	for _, profile := range s.profiles {
		if profile.ID == nodeID {
			return profile, true, nil
		}
	}
	return proxy.NodeProfile{}, false, nil
}

func newBench(t *testing.T) (*registry.Registry, *Service) {
	t.Helper()
	ctx := context.Background()
	qualityRegistry, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "enf.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = qualityRegistry.Close() })
	service := New(DefaultConfig(), qualityRegistry, nil, nil, nil)
	go service.Run(context.Background())
	return qualityRegistry, service
}

// TestPollEpochsEstablishesArchiveAndFlipsOnChange 锚定统一 ban 律执行:
// 首见建档→IP 变化翻篇→自动释放质量羁押(I15+G16:IP 变即放)。
func TestPollEpochsEstablishesArchiveAndFlipsOnChange(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	ipSource := memIPSource{current: map[uint64]string{5: "192.0.2.1"}}
	nodes := memNodes{profiles: []proxy.NodeProfile{{ID: 5, Enabled: true, Name: "n5"}}}
	service := New(DefaultConfig(), qualityRegistry, nil, nil, newMemRotator())
	go service.Run(context.Background())
	service.nodes, service.ipSource = nodes, ipSource
	defer service.Close(context.Background())
	if _, err := service.PollEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if qualityRegistry.CurrentEpoch(5) != 0 {
		t.Fatal("首见应建档 epoch 0")
	}
	// 质量羁押该节点(模拟 live 裁决后状态)。
	if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 5, To: model.ExitRemanded, CaseID: 1}); err != nil {
		t.Fatal(err)
	}
	// IP 变化→翻篇→自动释放。
	ipSource.current[5] = "192.0.2.2"
	ipSource.revision = 2
	service.ipSource = ipSource
	changes, err := service.PollEpochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].NewEpoch != 1 || !changes[0].Released {
		t.Fatalf("翻篇 = %+v", changes)
	}
	if !qualityRegistry.ExitEligible(5) {
		t.Fatal("IP 变化必须自动解禁(统一 ban 律,无金丝雀 G16)")
	}
}

// TestPollEpochsReconcilesPersistedArchiveAfterRestart 锚定 D8:进程重启会
// 不得依赖进程内去抖基线，也不能丢失停机期间发生的 IP 漂移。新执行所的
// 首次轮询必须比较 q_ip_epoch 最新档案并翻篇，否则旧 epoch 的 ban 会
// 永远卡住。
func TestPollEpochsReconcilesPersistedArchiveAfterRestart(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	if err := qualityRegistry.RecordExitIdentity(ctx, 5, model.ExitIdentityFromAggregate("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	for _, state := range []model.ExitState{model.ExitRemanded, model.ExitBanned} {
		if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{
			NodeID: 5, Epoch: 0, To: state, CaseID: 11,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 不传 nodes/ipSource 给 New，避免测试后台 goroutine；手动装配成
	// “重启后首次轮询”的执行所实例。
	service := New(DefaultConfig(), qualityRegistry, nil, nil, nil)
	go service.Run(context.Background())
	service.nodes = memNodes{profiles: []proxy.NodeProfile{{ID: 5, Enabled: true, Name: "n5"}}}
	service.ipSource = memIPSource{current: map[uint64]string{5: "192.0.2.2"}}

	changes, err := service.PollEpochs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].NodeID != 5 || changes[0].OldEpoch != 0 || changes[0].NewEpoch != 1 || !changes[0].Released {
		t.Fatalf("重启后的停机漂移必须翻篇: %+v", changes)
	}
	if qualityRegistry.CurrentEpoch(5) != 1 || !qualityRegistry.ExitEligible(5) {
		t.Fatalf("旧 epoch 状态必须释放: epoch=%d eligible=%t", qualityRegistry.CurrentEpoch(5), qualityRegistry.ExitEligible(5))
	}
	archive, err := qualityRegistry.ExitIPArchive(ctx, 5)
	if err != nil || len(archive) != 2 || archive[1].IP != "192.0.2.2" {
		t.Fatalf("停机漂移后的 IP 档案 = %+v, err=%v", archive, err)
	}
}

// TestManualUnban 锚定 G7:人工解禁清除当前 epoch 处置。
func TestManualUnban(t *testing.T) {
	qualityRegistry, service := newBench(t)
	ctx := context.Background()
	if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 9, To: model.ExitRemanded, CaseID: 2}); err != nil {
		t.Fatal(err)
	}
	if err := service.ManualUnban(ctx, 9); err != nil {
		t.Fatal(err)
	}
	if !qualityRegistry.ExitEligible(9) {
		t.Fatal("人工解禁必须生效(G7)")
	}
}

// TestRotateNodeGuardsAndRateLimit 锚定 G18+限速:仅 webhook 节点可
// 主动轮换;网络执行器限额拒绝原样传递，质量不复制计数。
func TestRotateNodeGuardsAndRateLimit(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	nodes := memNodes{profiles: []proxy.NodeProfile{
		{ID: 1, Enabled: true, Name: "fixed"},
		{ID: 2, Enabled: true, Name: "hook", RotationWebhook: true},
	}}
	rotator := newMemRotator()
	rotator.limit = 2
	service := New(Config{PollInterval: time.Hour}, qualityRegistry, nil, nil, rotator)
	go service.Run(context.Background())
	service.nodes, service.ipSource = nodes, memIPSource{}
	defer service.Close(context.Background())
	if err := service.RotateNode(ctx, 1); err == nil {
		t.Fatal("固定节点不得主动轮换")
	}
	if err := service.RotateNode(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := service.RotateNode(ctx, 2); err != nil {
		t.Fatal(err)
	}
	if err := service.RotateNode(ctx, 2); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("超限必须拒绝 = %v", err)
	}
	if len(rotator.calls) != 2 {
		t.Fatalf("实际触发 = %d", len(rotator.calls))
	}
}

// TestRotateNodesBatch 锚定 G18:批量轮换,限速部分如实报告。
func TestRotateNodesBatch(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	nodes := memNodes{profiles: []proxy.NodeProfile{
		{ID: 2, Enabled: true, Name: "hook2", RotationWebhook: true},
		{ID: 3, Enabled: true, Name: "hook3", RotationWebhook: true},
		{ID: 4, Enabled: true, Name: "hook4", RotationWebhook: true},
	}}
	rotator := newMemRotator()
	rotator.limit = 2
	service := New(Config{PollInterval: time.Hour}, qualityRegistry, nil, nil, rotator)
	go service.Run(context.Background())
	service.nodes, service.ipSource = nodes, memIPSource{}
	defer service.Close(context.Background())
	triggered, rateLimited, err := service.RotateNodes(ctx, []uint64{2, 3, 4})
	if err != nil || triggered != 2 || rateLimited != 1 {
		t.Fatalf("批量 = %d/%d, %v", triggered, rateLimited, err)
	}
}

// TestRecordDegradeEventLedger 锚定 G8:降智事件入台账(epoch 匹配
// 才带 IP 明细),台账不影响调度。
func TestRecordDegradeEventLedger(t *testing.T) {
	qualityRegistry, service := newBench(t)
	ctx := context.Background()
	if _, _, err := qualityRegistry.AdvanceEpoch(ctx, 7, model.ExitIdentityFromAggregate("198.51.100.3")); err != nil {
		t.Fatal(err)
	}
	if err := service.RecordDegradeEvent(ctx, 7, 1, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	history, err := qualityRegistry.ListNodeDegradeHistory(ctx, 7, 10)
	if err != nil || len(history) != 1 || history[0].IP != "198.51.100.3" || history[0].Epoch != 1 {
		t.Fatalf("台账 = %+v, %v", history, err)
	}
	if !qualityRegistry.ExitEligible(7) {
		t.Fatal("台账不影响调度资格")
	}
}

// TestNoCanaryNoExpiryByConstruction 锚定 G16/G17:执行所无金丝雀/
// 试探期/到期回池路径——解禁仅 IP 变化与人工两条路。结构断言:
// IP 未变的人工轮换后(轮换未生效),羁押必须持续。
func TestNoCanaryNoExpiryByConstruction(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	nodes := memNodes{profiles: []proxy.NodeProfile{{ID: 6, Enabled: true, Name: "hook6", RotationWebhook: true}}}
	service := New(Config{PollInterval: time.Hour}, qualityRegistry, nodes, memIPSource{current: map[uint64]string{6: "203.0.113.1"}}, newMemRotator())
	go service.Run(context.Background())
	defer service.Close(context.Background())
	if _, err := service.PollEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 6, To: model.ExitRemanded, CaseID: 3}); err != nil {
		t.Fatal(err)
	}
	if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 6, To: model.ExitBanned, CaseID: 3}); err != nil {
		t.Fatal(err)
	}
	// 先羁押再翻篇到 ban:直接对当前 epoch 建档+ban 已完成;
	// IP 未变时轮询不得释放(G17:无到期回池)。
	if _, err := service.PollEpochs(ctx); err != nil {
		t.Fatal(err)
	}
	if qualityRegistry.ExitEligible(6) {
		t.Fatal("IP 未变化时 ban 必须持续(无到期回池 G17)")
	}
}

// TestAutoRotateBannedWebhookNodes 锚定统一 ban 律自愈闭环:BANNED 的
// webhook 节点被自动触发轮换;固定型不触发;限速耗尽安静截止。
func TestAutoRotateBannedWebhookNodes(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	nodes := memNodes{profiles: []proxy.NodeProfile{
		{ID: 1, Enabled: true, Name: "fixed"},
		{ID: 2, Enabled: true, Name: "hook-a", RotationWebhook: true},
		{ID: 3, Enabled: true, Name: "hook-b", RotationWebhook: true},
	}}
	rotator := newMemRotator()
	rotator.limit = 1
	service := New(Config{PollInterval: time.Hour}, qualityRegistry, nil, nil, rotator)
	go service.Run(context.Background())
	service.nodes, service.ipSource = nodes, memIPSource{}
	defer service.Close(context.Background())
	for _, id := range []uint64{1, 2, 3} {
		if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: id, To: model.ExitRemanded, CaseID: 9}); err != nil {
			t.Fatal(err)
		}
		if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: id, To: model.ExitBanned, CaseID: 9}); err != nil {
			t.Fatal(err)
		}
	}
	rotated := service.rotateBannedWebhooks(ctx)
	if rotated != 1 {
		t.Fatalf("限速 1/h 内只应轮换一个 = %d", rotated)
	}
	seen := map[uint64]bool{}
	for _, id := range rotator.calls {
		seen[id] = true
	}
	if seen[1] {
		t.Fatal("固定节点不得自动轮换")
	}
	if !seen[2] && !seen[3] {
		t.Fatal("被 ban 的 webhook 节点必须自动轮换")
	}
	// IP 未变化:ban 持续(G17),待轮换生效后由检测翻篇解禁。
	if qualityRegistry.ExitEligible(2) || qualityRegistry.ExitEligible(3) {
		t.Fatal("IP 未变化时 ban 必须持续")
	}
}

// TestAutoRotateBackoffPreventsHammering 锚定自动轮换退避:轮换后
// IP 未变化的节点在退避窗内不得重复触发(否则每拍重烧全局限速槽,
// 排序在后的被禁节点饿死);退避过期后恢复触发;人工入口不受约束。
func TestAutoRotateBackoffPreventsHammering(t *testing.T) {
	qualityRegistry, _ := newBench(t)
	ctx := context.Background()
	nodes := memNodes{profiles: []proxy.NodeProfile{
		{ID: 2, Enabled: true, Name: "hook", RotationWebhook: true},
	}}
	rotator := newMemRotator()
	service := New(Config{PollInterval: time.Hour}, qualityRegistry, nil, nil, rotator)
	go service.Run(context.Background())
	service.nodes, service.ipSource = nodes, memIPSource{}
	defer service.Close(context.Background())
	if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 2, To: model.ExitRemanded, CaseID: 9}); err != nil {
		t.Fatal(err)
	}
	if err := qualityRegistry.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 2, To: model.ExitBanned, CaseID: 9}); err != nil {
		t.Fatal(err)
	}
	if rotated := service.rotateBannedWebhooks(ctx); rotated != 1 {
		t.Fatalf("首次扫掠应轮换 1 次, got %d", rotated)
	}
	if rotated := service.rotateBannedWebhooks(ctx); rotated != 0 {
		t.Fatalf("退避窗内不得重复触发, got %d", rotated)
	}
	if len(rotator.calls) != 1 {
		t.Fatalf("webhook 不得被重复打扰, calls=%v", rotator.calls)
	}
	// 退避过期(人为回拨)后恢复触发。
	service.lastRotateMu.Lock()
	service.lastRotate[2] = time.Now().UTC().Add(-3 * time.Hour)
	service.lastRotateMu.Unlock()
	if rotated := service.rotateBannedWebhooks(ctx); rotated != 1 {
		t.Fatalf("退避过期后应恢复触发, got %d", rotated)
	}
	// 人工入口不受退避约束。
	if err := service.RotateNode(ctx, 2); err != nil {
		t.Fatalf("人工轮换不受退避约束: %v", err)
	}
}
