// Package guard 是守卫(B4):流判定(移植零分配解析的判决语义)。
//
// 三规则(I1/I3:降智内容永不进上下文;判定毫秒级早断):
//
//	规则 1 thinking → deliver(可见思考增量满足准入);
//	规则 2 item_done → withhold(推理阶段闭合零思考——降智确切签名,
//	         零延迟拦截);
//	规则 3 outrun → withhold(无思考出正文,与长度无关);
//	终态兜底 terminal → withhold;否则 wait(等待证据)。
//
// fail-closed 是唯一耗尽策略(G12:fail-open 废除——降智不进上下文
// 是铁律)。管辖模型用户勾选(G13:不再按"推理型"隐式推导)。
// usage 声称的推理数永不算证据(I2:上游撒谎事故)。
//
// The guard is the sole request-path classifier; its withhold result feeds
// the direct attribution loop asynchronously.
package guard

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"

	guardpolicy "github.com/chenyme/grok2api/backend/internal/domain/guard"
)

// RuleVersion identifies the shipped scanner evidence and single-choice policy.
// Changing its semantics requires a new version for persisted measurements.
const RuleVersion = "visible-thinking/v2-single-choice"

// Verdict 是守卫判决。
type Verdict string

const (
	// Deliver 满足当前准入规则,不代表完整交付或内容正确。
	Deliver Verdict = "deliver"
	// Withhold 扣留(降智,不达客户端)。
	Withhold Verdict = "withhold"
	// Wait 等待(证据未齐,预算内继续观察)。
	Wait Verdict = "wait"
)

// Signals 是流观察信号(零分配解析器的输出;判定只消费信号,
// 不触碰流本身——扫描机械与判决分离,双跑对比只比判决)。
type Signals struct {
	// HasThinking 已收到有效思考增量(可见 reasoning/thinking 文本
	// delta;密文/注释/usage 声明都不算——I2)。
	HasThinking bool
	// ReasoningEndedWithoutThinking 推理阶段已闭合且零思考增量
	//(降智包确切签名)。
	ReasoningEndedWithoutThinking bool
	// VisibleTokens/OutputTokens 观测到的正文量(正文抢跑判定)。
	VisibleTokens int64
	OutputTokens  int64
	// Terminal 流已到终态。
	Terminal bool
}

// Rule 判决命中的规则(可解释性;I25 同源)。
type Rule string

const (
	RuleThinking Rule = "thinking"
	RuleItemDone Rule = "item_done"
	RuleOutrun   Rule = "outrun"
	RuleTerminal Rule = "terminal"
	RuleWait     Rule = "wait"
)

// Judge 三规则判决(纯函数,移植旧守卫分类语义——批6 双跑对比基准)。
func Judge(sig Signals) (Verdict, Rule) {
	switch {
	case sig.HasThinking:
		return Deliver, RuleThinking
	case sig.ReasoningEndedWithoutThinking:
		return Withhold, RuleItemDone
	case sig.VisibleTokens > 0 || sig.OutputTokens > 0:
		return Withhold, RuleOutrun
	case sig.Terminal:
		return Withhold, RuleTerminal
	default:
		return Wait, RuleWait
	}
}

// Config is the domain policy; storage readiness belongs to Service.
type Config = guardpolicy.Config

// configState atomically publishes the policy with its readiness.
type configState struct {
	Config
	unavailable error
}

// Service 是守卫配置面+自检(I4:守卫失效必须可见)。
type Service struct {
	updateMu sync.Mutex
	cfg      atomic.Pointer[configState]
	store    Store
	fileBase Config
}

// Store 是守卫配置的持久化面(切换批落档:runtime settings,启动读取)。
// nil store = 纯内存(I13 可剥离性;拔掉质量层时不留任何存储触点)。
type Store interface {
	// LoadGuard 读回持久化配置;found=false 表示无持久化行(用默认)。
	LoadGuard(ctx context.Context) (cfg Config, found bool, err error)
	// SaveGuard 持久化配置(先落库后生效:失败即拒绝更新,库为真相源 I17)。
	SaveGuard(ctx context.Context, cfg Config) error
}

// NewWithFileDefaults preserves the legacy startup fallback separately from
// the real file baseline. A persisted guard policy overrides the fallback.
func NewWithFileDefaults(initial, fileBase Config, store Store) *Service {
	initial = guardpolicy.Normalize(initial)
	fileBase = guardpolicy.Normalize(fileBase)
	s := &Service{store: store, fileBase: fileBase}
	s.cfg.Store(&configState{Config: initial, unavailable: guardpolicy.Validate(initial)})
	return s
}

// Config 返回当前配置快照。
func (s *Service) Config() Config {
	copied := s.cfg.Load().Config
	copied.GuardedModels = append([]string{}, copied.GuardedModels...)
	return copied
}

// FileDefaults returns the immutable constructor baseline for scoped reset.
func (s *Service) FileDefaults() Config {
	cfg := s.fileBase
	cfg.GuardedModels = append([]string{}, cfg.GuardedModels...)
	cfg.Revision = 0
	return cfg
}

// ResetToDefaults saves this instance's file baseline as a new explicit policy.
// It never deletes the clock or touches gateway/quality_tunables documents.
func (s *Service) ResetToDefaults(ctx context.Context, expected uint64) (Config, error) {
	cfg := s.FileDefaults()
	cfg.Revision = expected
	return s.Update(ctx, cfg)
}

// Update validates, commits the CAS, then publishes one immutable policy.
func (s *Service) Update(ctx context.Context, cfg Config) (Config, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	cfg = guardpolicy.Normalize(cfg)
	if err := guardpolicy.Validate(cfg); err != nil {
		return Config{}, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}
	if cfg.Revision != s.cfg.Load().Revision {
		return Config{}, ErrConflict
	}
	if cfg.Revision >= math.MaxInt64 {
		return Config{}, errors.New("guard: policy revision exhausted")
	}
	cfg.Revision++
	if s.store != nil {
		if err := s.store.SaveGuard(ctx, cfg); err != nil {
			return Config{}, err
		}
	}
	s.cfg.Store(&configState{Config: cfg})
	return s.Config(), nil
}

// Read refreshes management state from durable authority. Config/Snapshot stay
// nonblocking for request execution and return an immutable process snapshot.
func (s *Service) Read(ctx context.Context) (Config, error) {
	if err := s.LoadPersisted(ctx); err != nil {
		return Config{}, err
	}
	return s.Snapshot()
}

// LoadPersisted 启动时读回持久化配置(无行=保持构造时的默认/基线)。
func (s *Service) LoadPersisted(ctx context.Context) (err error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	defer func() {
		// Only the calling read owns its cancellation or deadline. A store
		// failure with a live caller still makes the authority unavailable.
		if err != nil && !errors.Is(err, ctx.Err()) {
			s.cfg.Store(&configState{Config: s.Config(), unavailable: err})
		}
	}()
	if s.store == nil {
		return guardpolicy.Validate(s.cfg.Load().Config)
	}
	cfg, found, err := s.store.LoadGuard(ctx)
	if err != nil {
		return err
	}
	if !found {
		current := s.Config()
		if current.Revision != 0 {
			return errors.New("guard: persisted policy disappeared")
		}
		if err := guardpolicy.Validate(current); err != nil {
			return err
		}
		s.cfg.Store(&configState{Config: current})
		return nil
	}
	if cfg.Revision < s.cfg.Load().Revision {
		return errors.New("guard: persisted policy revision regressed")
	}
	cfg = guardpolicy.Normalize(cfg)
	if err := guardpolicy.Validate(cfg); err != nil {
		return err
	}
	s.cfg.Store(&configState{Config: cfg})

	return nil
}

// Jurisdiction 报告模型是否在管辖内(G13;渠道限定见 Config.Jurisdiction)。
// 请求路径读取不可变快照，无锁、零分配；
// 清单为空=全部不在管辖(启用态空清单会被 Update 拒绝,这里是守卫
// 关闭态的物理表达)。
func (s *Service) Jurisdiction(provider, model string) bool {
	return s.cfg.Load().Jurisdiction(provider, model)
}

// SelfCheck 自检(I4:守卫在场且有效必须可验证)。
// 返回 nil=健康;错误即横幅素材(生效可见性)。
func (s *Service) SelfCheck() error {
	cfg, err := s.Snapshot()
	if err != nil {
		return err
	}
	if !cfg.Enabled {
		return errors.New("guard disabled")
	}
	if len(cfg.GuardedModels) == 0 {
		return errors.New("guard enabled without jurisdiction")
	}
	if cfg.MaxAttempts < 1 {
		return errors.New("guard budget below one attempt")
	}
	// 判决内核健全性:三规则各自的锚定信号必须得到各自判决。
	if verdict, _ := Judge(Signals{HasThinking: true}); verdict != Deliver {
		return errors.New("guard rule 1 broken: thinking must deliver")
	}
	if verdict, _ := Judge(Signals{ReasoningEndedWithoutThinking: true}); verdict != Withhold {
		return errors.New("guard rule 2 broken: item_done must withhold")
	}
	if verdict, _ := Judge(Signals{OutputTokens: 5}); verdict != Withhold {
		return errors.New("guard rule 3 broken: outrun must withhold")
	}
	return nil
}

// ErrConflict prevents a stale editor or replica from replacing a newer policy.
var ErrConflict = errors.New("guard: policy revision conflict")

// Snapshot returns the policy and readiness from one serialized load/update view.
// Config remains immutable after publication; callers receive an owned model list.
func (s *Service) Snapshot() (Config, error) {
	current := s.cfg.Load()
	cfg := current.Config
	cfg.GuardedModels = append([]string{}, cfg.GuardedModels...)
	return cfg, current.unavailable
}

var ErrInvalidInput = guardpolicy.ErrInvalidInput
var ErrInvalidJurisdiction = guardpolicy.ErrInvalidJurisdiction
