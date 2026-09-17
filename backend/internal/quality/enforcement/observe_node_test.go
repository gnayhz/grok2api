package enforcement

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
)

// TestObserveNodeExitAdvancesSingleIdentity 固定单节点即时观测:
// 轮换成功后组合根直接驱动本入口,同一身份的 epoch 翻篇与释放不必等
// 下一个检测节拍;停用/缺失节点与 PollEpochs 同口径跳过;迟到版本
// (revision 水位以下)不得翻篇。
func TestObserveNodeExitAdvancesSingleIdentity(t *testing.T) {
	r, s := newBench(t)
	ctx := context.Background()
	if _, _, _, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1"}, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := r.ObserveExitIdentity(ctx, 6, model.ExitIdentity{IPv4: "192.0.2.1"}, 1); err != nil {
		t.Fatal(err)
	}
	// 节点 5 在当前 epoch 上有生效 ban:即时观测翻篇时应同步释放。
	caseID, err := r.OpenInvestigation(ctx, 9, model.EpochKey{NodeID: 5, Epoch: 0}, time.Now(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := r.SettleInvestigation(ctx, caseID, model.VerdictExitGuilty, "{}", time.Now(), true); err != nil {
		t.Fatal(err)
	}
	nodes := memNodes{profiles: []proxy.NodeProfile{
		{ID: 5, Enabled: true},
		{ID: 6, Enabled: false},
	}}
	// 手动驱动观测，不启动会消费同一版本的后台首次轮询。
	s.nodes = nodes
	s.ipSource = memIPSource{current: map[uint64]string{5: "192.0.2.2"}, revision: 2}

	change, changed, err := s.ObserveNodeExit(ctx, 5)
	if err != nil || !changed {
		t.Fatalf("single observation must advance: changed=%v err=%v", changed, err)
	}
	if change.NodeID != 5 || change.NewEpoch != 1 || !change.Released {
		t.Fatalf("unexpected change: %+v", change)
	}

	// 停用节点:无变化、无错误。
	if _, changed, err := s.ObserveNodeExit(ctx, 6); err != nil || changed {
		t.Fatalf("disabled node must be skipped: changed=%v err=%v", changed, err)
	}
	// 缺失节点:ok=false,无错误。
	if _, changed, err := s.ObserveNodeExit(ctx, 99); err != nil || changed {
		t.Fatalf("missing node must be absent: changed=%v err=%v", changed, err)
	}
	// 重复驱动:版本水位已推进,同一观测不得再翻篇。
	if _, changed, err := s.ObserveNodeExit(ctx, 5); err != nil || changed {
		t.Fatalf("repeat observation must be fenced: changed=%v err=%v", changed, err)
	}
}

// TestObserveNodeExitIPv6OnlyChangeAdvances 固定双族口径:IPv4 稳定、
// 仅 IPv6 变化同样翻篇(与轮换验证同一把尺子)。
func TestObserveNodeExitIPv6OnlyChangeAdvances(t *testing.T) {
	r, s := newBench(t)
	ctx := context.Background()
	if _, _, _, err := r.ObserveExitIdentity(ctx, 5, model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::1"}, 1); err != nil {
		t.Fatal(err)
	}
	nodes := memNodes{profiles: []proxy.NodeProfile{{ID: 5, Enabled: true}}}
	s.nodes = nodes
	s.ipSource = failingIPObservation(func(context.Context, uint64) (model.ExitIdentity, uint64, bool, error) {
		return model.ExitIdentity{IPv4: "192.0.2.1", IPv6: "2001:db8::2"}, 2, true, nil
	})
	change, changed, err := s.ObserveNodeExit(ctx, 5)
	if err != nil || !changed || change.NewEpoch != 1 {
		t.Fatalf("ipv6-only change must advance: %+v changed=%v err=%v", change, changed, err)
	}
}
