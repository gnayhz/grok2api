package guard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// TestJudgeThreeRules 锚定三规则判决(移植语义基准,批6 双跑对比):
// thinking→deliver;item_done/outrun/terminal→withhold;否则 wait。
func TestJudgeThreeRules(t *testing.T) {
	cases := []struct {
		name string
		sig  Signals
		want Verdict
		rule Rule
	}{
		{"thinking delivers", Signals{HasThinking: true}, Deliver, RuleThinking},
		{"item_done withholds", Signals{ReasoningEndedWithoutThinking: true}, Withhold, RuleItemDone},
		{"outrun withholds on visible", Signals{VisibleTokens: 3}, Withhold, RuleOutrun},
		{"outrun withholds on usage output", Signals{OutputTokens: 100}, Withhold, RuleOutrun},
		{"terminal withholds", Signals{Terminal: true}, Withhold, RuleTerminal},
		{"clean start waits", Signals{}, Wait, RuleWait},
		// I2:usage 声称的推理数不算证据——大 ReasoningTokens 无思考仍 wait。
		{"usage reasoning is not evidence", Signals{OutputTokens: 0}, Wait, RuleWait},
	}
	for _, c := range cases {
		if verdict, rule := Judge(c.sig); verdict != c.want || rule != c.rule {
			t.Fatalf("%s: got %s/%s want %s/%s", c.name, verdict, rule, c.want, c.rule)
		}
	}
}

// TestFailClosedOnly 锚定 G12:耗尽策略恒 fail-closed,无任何开关。
func TestFailClosedOnly(t *testing.T) {
	service := New(DefaultConfig(), nil)
	if policy := service.Config().ExhaustionPolicy(); policy != "fail_closed" {
		t.Fatalf("耗尽策略 = %s", policy)
	}
}

// TestJurisdictionCheckboxes 锚定 G13:管辖按模型清单勾选。
func TestJurisdictionCheckboxes(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.Jurisdiction("grok_build", "grok-4.5") || cfg.Jurisdiction("grok_build", "grok-3") {
		t.Fatal("管辖勾选语义错误")
	}
}

// TestJurisdictionScopedByChannel 锚定 G13 渠道限定:条目"渠道:模型"
// 只拦该渠道;裸条目任意渠道命中。
func TestJurisdictionScopedByChannel(t *testing.T) {
	cfg := Config{Enabled: true, GuardedModels: []string{"grok_build:grok-4.5", "grok-4.6"}}
	if !cfg.Jurisdiction("grok_build", "grok-4.5") {
		t.Fatal("限定条目应命中本渠道")
	}
	if cfg.Jurisdiction("grok_console", "grok-4.5") {
		t.Fatal("限定条目不应命中其他渠道")
	}
	if !cfg.Jurisdiction("grok_console", "grok-4.6") {
		t.Fatal("裸条目应命中任意渠道")
	}
}

// TestSelfCheckVisibility 锚定 I4:守卫失效必须可见——
// 关闭/无管辖/预算非法各自报错;健康配置通过。
func TestSelfCheckVisibility(t *testing.T) {
	service := New(DefaultConfig(), nil)
	if err := service.SelfCheck(); err != nil {
		t.Fatalf("健康配置自检 = %v", err)
	}
	disabled := DefaultConfig()
	disabled.Enabled = false
	if err := New(disabled, nil).SelfCheck(); err == nil {
		t.Fatal("关闭状态必须可见(非静默)")
	}
	noJurisdiction := DefaultConfig()
	noJurisdiction.GuardedModels = nil
	// 启用+无管辖:Update 拒绝(I4 防线在前)。
	if _, err := New(noJurisdiction, nil).Update(context.TODO(), noJurisdiction); err == nil {
		t.Fatal("启用而无管辖必须拒绝更新")
	}
	lowBudget := DefaultConfig()
	lowBudget.MaxAttempts = 0
	if err := New(lowBudget, nil).SelfCheck(); err == nil {
		t.Fatal("非法预算必须可见")
	}
}

// fakeGuardStore 内存版 guard.Store(持久化往返测试用)。
type fakeGuardStore struct {
	saved map[string]Config
	fail  bool
}

func (f *fakeGuardStore) LoadGuard(ctx context.Context) (Config, bool, error) {
	if f.saved == nil {
		return Config{}, false, nil
	}
	cfg, ok := f.saved["only"]
	return cfg, ok, nil
}

func (f *fakeGuardStore) SaveGuard(ctx context.Context, cfg Config) error {
	if f.fail {
		return errors.New("store down")
	}
	if f.saved == nil {
		f.saved = map[string]Config{}
	}
	f.saved["only"] = cfg
	return nil
}

// TestConfigPersistenceRoundtrip 锚定切换手册第1步:更新先落库,
// 新服务实例读回持久化配置(重启不丢——I17 同源纪律)。
func TestConfigPersistenceRoundtrip(t *testing.T) {
	store := &fakeGuardStore{}
	first := New(DefaultConfig(), store)
	updated := DefaultConfig()
	updated.MaxAttempts = 4
	updated.GuardedModels = []string{"grok-4.7"}
	if _, err := first.Update(context.Background(), updated); err != nil {
		t.Fatalf("更新失败: %v", err)
	}
	// 模拟重启:新实例从同一 store 读回。
	restarted := New(DefaultConfig(), store)
	if err := restarted.LoadPersisted(context.Background()); err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	cfg := restarted.Config()
	if cfg.MaxAttempts != 4 || len(cfg.GuardedModels) != 1 || cfg.GuardedModels[0] != "grok-4.7" {
		t.Fatalf("重启后配置丢失: %+v", cfg)
	}
}

// TestUpdateRejectedWhenStoreFails 持久化失败即拒:内存不领先于库。
func TestUpdateRejectedWhenStoreFails(t *testing.T) {
	store := &fakeGuardStore{fail: true}
	service := New(DefaultConfig(), store)
	updated := DefaultConfig()
	updated.MaxAttempts = 9
	if _, err := service.Update(context.Background(), updated); err == nil {
		t.Fatal("库失败时更新必须报错")
	}
	if got := service.Config().MaxAttempts; got != DefaultConfig().MaxAttempts {
		t.Fatalf("库失败后内存被污染: %d", got)
	}
}

func TestInvalidBudgetNeverReplacesSnapshot(t *testing.T) {
	ctx := context.Background()
	service := New(DefaultConfig(), nil)
	for _, budget := range []int{0, -1} {
		cfg := DefaultConfig()
		cfg.MaxAttempts = budget
		if _, err := service.Update(ctx, cfg); err == nil {
			t.Fatal("accepted invalid budget")
		}
		if service.Config().MaxAttempts != 2 {
			t.Fatal("failed update changed snapshot")
		}
		persisted := New(DefaultConfig(), &fakeGuardStore{saved: map[string]Config{"only": cfg}})
		if err := persisted.LoadPersisted(ctx); err == nil {
			t.Fatal("loaded invalid budget")
		}
	}
}

func TestConcurrentJurisdictionSnapshots(t *testing.T) {
	service := New(DefaultConfig(), nil)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 500 {
				snapshot := service.Config()
				snapshot.GuardedModels[0] = "caller-owned"
				if !service.Jurisdiction("grok_build", "grok-4.6") {
					t.Error("torn jurisdiction snapshot")
				}
			}
		})
	}
	for range 100 {
		cfg := service.Config()
		cfg.GuardedModels = []string{" grok-4.6 ", "grok-4.6"}
		if _, err := service.Update(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
	if len(service.Config().GuardedModels) != 1 {
		t.Fatal("models were not deduplicated")
	}
}

type cancelledGuardStore struct{}

func (cancelledGuardStore) LoadGuard(ctx context.Context) (Config, bool, error) {
	return Config{}, false, ctx.Err()
}
func (cancelledGuardStore) SaveGuard(ctx context.Context, _ Config) error { return ctx.Err() }

func TestCancelledManagementReadDoesNotInvalidateInstalledPolicy(t *testing.T) {
	service := New(DefaultConfig(), cancelledGuardStore{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := service.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read=%v", err)
	}
	if _, err := service.Snapshot(); err != nil {
		t.Fatalf("admin cancellation poisoned request policy: %v", err)
	}
}

func TestInvalidConstructorCannotBecomeReadyWithoutDocument(t *testing.T) {
	for _, store := range []Store{nil, &fakeGuardStore{}} {
		for _, invalid := range []func(*Config){
			func(c *Config) { c.GuardedModels = []string{"unknown:grok-4.6"} },
			func(c *Config) { c.Enabled = false; c.EvidenceTimeout = -time.Second },
			func(c *Config) { c.MaxAttempts = -1 },
		} {
			initial := DefaultConfig()
			invalid(&initial)
			service := New(initial, store)
			if _, err := service.Snapshot(); err == nil {
				t.Fatal("invalid constructor ready")
			}
			if err := service.LoadPersisted(context.Background()); err == nil {
				t.Fatal("missing document accepted invalid bootstrap")
			}
			if _, err := service.Snapshot(); err == nil {
				t.Fatal("missing document cleared unavailable")
			}
			valid := DefaultConfig()
			if _, err := service.Update(context.Background(), valid); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Snapshot(); err != nil {
				t.Fatalf("valid replacement failed to restore readiness: %v", err)
			}
			if _, err := service.ResetToDefaults(context.Background(), 1); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("invalid file reset=%v", err)
			}
			if current, err := service.Snapshot(); err != nil || current.Revision != 1 {
				t.Fatalf("invalid reset changed active policy: %+v %v", current, err)
			}
			// Use a fresh empty authority for the next invalid constructor.
			if store != nil {
				store = &fakeGuardStore{}
			}
		}
	}
}
