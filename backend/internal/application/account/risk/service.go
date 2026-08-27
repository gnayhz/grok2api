// Package risk attributes quality-withhold events to account-level RSC
// verdicts. A Build account inherits the verdict of its linked Web SSO
// identity; accounts without a link fall back to the gateway's escalating
// missing-thinking penalties.
package risk

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/observability"
	"github.com/chenyme/grok2api/backend/internal/infra/rsc"
	"github.com/chenyme/grok2api/backend/internal/pkg/batch"
	"github.com/chenyme/grok2api/backend/internal/pkg/perfmetrics"
)

// Verdict strings persisted by the store.
const (
	VerdictClean   = "clean"
	VerdictDenied  = "denied"
	VerdictFlagged = "flagged"
	VerdictError   = "error"
)

// StoredVerdict is the persisted form of one RSC check.
type StoredVerdict struct {
	Verdict    string
	BotFlagSrc int
	BotFlagDtl string
	RiskScore  float64
	HTTPStatus int
	Error      string
	Source     string
	CheckedAt  time.Time
	// OriginAccountID 触发本次判定的账号:启动对账/巡检重放后果时只打到
	// 该账号(通道隔离),不连坐 verdict 键所在的 Web 身份。0=旧数据,重放
	// 退回 webID 语义。
	OriginAccountID uint64
	// Trigger 判定入口：degrade（请求降智）/ patrol（主动巡检）/ manual。
	Trigger string
	// DeniedStreak 记录连续 denied 次数（0=旧数据/单次）：达到
	// DeniedConfirmations 才处置与连坐；clean/error 覆盖时自然归零。
	DeniedStreak int
}

// Risky reports a verdict that marks the identity as registration risk.
func (v StoredVerdict) Risky() bool { return v.Verdict == VerdictDenied || v.Verdict == VerdictFlagged }

// ErrNotFound marks a missing stored verdict; the concrete store maps its
// persistence-layer not-found error onto it at the wiring boundary.
var ErrNotFound = errors.New("risk verdict not found")

// Store persists verdicts. denied/flagged never expire; clean goes stale
// after the patrol interval; error after the retry window.
type Store interface {
	GetRiskVerdict(ctx context.Context, accountID uint64) (relationalVerdict, error)
	SaveRiskVerdict(ctx context.Context, accountID uint64, verdict StoredVerdict) error
	// DeleteRiskVerdict 永久移除一个身份的结论。此前 denied/flagged 既不过期
	// 也无删除路径, 人工清除 risk_status 会被启动对账与后续降智事件回滚。
	DeleteRiskVerdict(ctx context.Context, accountID uint64) error
	// ListRiskyVerdictAccountIDs 列出持有 denied/flagged 结论的账号，供启动
	// 对账把 risk_status 收敛到 verdict 表。
	ListRiskyVerdictAccountIDs(ctx context.Context) ([]uint64, error)
	// ListRiskyVerdictAccountIDsAfter 以账号 ID 游标分页（大池全量收敛）。
	ListRiskyVerdictAccountIDsAfter(ctx context.Context, afterID uint64) ([]uint64, error)
	// DeleteCleanVerdictsExceptSources 删除由其他检测方法产生的 clean 结论
	// （方法切换后旧缓存一律失效；keepSources 之外全部清除）；
	// denied/flagged/error 不受影响。
	DeleteCleanVerdictsExceptSources(ctx context.Context, keepSources ...string) (int64, error)
	// MostRecentCleanVerdict 返回最近一次 clean 结论的账号（限指定检测
	// 方法，且结论年龄不超过 maxAge；maxAge<=0 不过滤），作为通道词汇熔断
	// 的见证人候选；无则 found=false。
	MostRecentCleanVerdict(ctx context.Context, source string, maxAge time.Duration) (uint64, bool, error)
}

// relationalVerdict decouples the service from the persistence layer shape.
type relationalVerdict = StoredVerdict

// Config mirrors config.AccountRiskRSCConfig at runtime. Every field is
// hot-reloadable via UpdateConfig (settings surface); the probe
// implementation itself (method/timeout) is swapped via UpdateChecker.
type Config struct {
	Enabled        bool
	Concurrency    int
	Timeout        time.Duration
	OnDenied       string // disable | markOnly | flag
	PatrolEnabled  bool
	PatrolInterval time.Duration
	// PatrolTickEvery is the periodic loop interval (default 15m).
	PatrolTickEvery time.Duration
	// PatrolBatchSize caps how many due identities one tick re-checks (default 50).
	PatrolBatchSize int
	ErrorRetry      time.Duration
	// DeniedConfirmations 是 denied 定罪所需的连续确认次数（0=默认 2）：
	// 未达次数的 denied 只记录不处置，待 ErrorRetry 后的重探确认。
	DeniedConfirmations int
	// DeniedTTL 是已确认 denied verdict 的新鲜期（0=默认 24h）：过期后
	// 允许重探，误判可自愈（clean 会覆盖旧 denied）。
	DeniedTTL time.Duration
	// BuildProbeEnabled gates the Build-native differential fallback for
	// unlinked Build accounts (config-switchable, default off).
	BuildProbeEnabled bool
}

// Accounts is the account-service surface attribution needs. The concrete
// *accountapp.Service satisfies it; tests substitute fakes.
type Accounts interface {
	DecryptedAccessToken(ctx context.Context, id uint64) (string, error)
	LinkedWebAccountID(ctx context.Context, buildAccountID uint64) (uint64, bool, error)
	// LinkedBuildAccountIDs / LinkedConsoleAccountIDs enumerate the other
	// channels of one SSO identity. Request-path attribution stays
	// channel-scoped; SSO patrol denials fan out to this whole group.
	LinkedBuildAccountIDs(ctx context.Context, webAccountID uint64) ([]uint64, error)
	LinkedConsoleAccountIDs(ctx context.Context, webAccountID uint64) ([]uint64, error)
	SetAccountEnabled(ctx context.Context, id uint64, enabled bool, reason string) error
	SetAccountRiskStatus(ctx context.Context, id uint64, flagged bool) error
	SetAccountRiskAttribution(ctx context.Context, id uint64, flagged bool, trigger string, originAccountID uint64, detail string, checkedAt time.Time) error
	ClearMissingThinkingCooldown(ctx context.Context, id uint64) error
}

// Service runs event-driven and patrol RSC attribution.
type Service struct {
	// cfgMu guards cfg/sem/checker for settings hot-reload (UpdateConfig /
	// UpdateChecker run on the settings apply chain while checks are in
	// flight). Reads copy the fields they need into locals and never hold
	// the lock across blocking work.
	cfgMu    sync.RWMutex
	cfg      Config
	accounts Accounts
	store    Store
	checker  Checker
	// checkerTag identifies the active probe method ("sso_probe"/"rsc"); see
	// CheckResult.Source. Empty keeps legacy freshness semantics (all stored
	// clean verdicts stay eligible), used by tests and pre-tag callers.
	checkerTag string
	logger     *slog.Logger
	// egressQuarantiner receives exit-IP quarantine duty when attribution
	// exonerates the account (RSC clean); nil keeps the degrade account-scoped.
	egressQuarantiner EgressQuarantiner
	// buildProber is the Build-native fallback for unlinked Build accounts;
	// nil keeps the behavioral-only path (no RSC signal without a Web SSO).
	buildProber BuildProber

	sem chan struct{}
	// admissionDedup: credential.ID -> struct{}。仅约束 OnDegraded 入队。
	// 与 checkInflight 键空间隔离：同一账号 ID 可同时出现在两个 map（如
	// Web 账号 credential.ID == webID），共用一张 map 会让 checkNow 撞上
	// admission 占位而误判 "already running"（多份外部复核交叉确认）。
	admissionDedup sync.Map
	// checkInflight: webID -> *checkCall。并发同身份检查在此合并：后来者
	// 等待首个调用完成并共享其结果（singleflight 语义），避免"第二个
	// Build 的归因 error-out 且冷却滞留"。
	checkInflight sync.Map
	pending       atomic.Int32
	// waiters 记录此刻停靠在同身份在途检查上的调用数（测试可等待的确定性观测点）。
	waiters atomic.Int32
	// witnessMu 限制通道词汇熔断的见证人复验频率。
	witnessMu     sync.Mutex
	lastWitnessAt time.Time
}

// witnessRetryInterval 限制见证人复验的最低间隔：复验消耗一位健康账号的
// 一条消息额度，熔断持续触发时也至多每 10 分钟一次。
const witnessRetryInterval = 10 * time.Minute

// witnessMaxAge 限定 SSO 通道词汇见证人的最大结论年龄，与 Build 差分
// 见证人(buildWitnessMaxAge)一致。巡检关闭时库里可能留着数月前的
// clean：词汇可能早已改变，用它"治愈"熔断会把本轮真实 denied 放过去。
const witnessMaxAge = 7 * 24 * time.Hour

// revalidateChannelWitness answers a Suppressed verdict by re-probing the most
// recently proven-clean identity: a thinking stream from the witness proves the
// channel vocabulary is alive (which also heals the probe's internal breaker
// through its own clean record), so the caller may re-run the real verdict. No
// witness or a non-clean witness keeps the breaker tripped.
func (s *Service) revalidateChannelWitness(ctx context.Context, checker Checker) bool {
	now := time.Now().UTC()
	probed := false
	s.witnessMu.Lock()
	if now.Sub(s.lastWitnessAt) < witnessRetryInterval {
		s.witnessMu.Unlock()
		return false
	}
	s.lastWitnessAt = now
	s.witnessMu.Unlock()
	// 复验预算买的是"烧一位健康账号一条消息额度"的权利:查库失败或无
	// 候选属于廉价失败,归还预算,避免把 10 分钟窗口白白占满。
	defer func() {
		if !probed {
			s.witnessMu.Lock()
			s.lastWitnessAt = time.Time{}
			s.witnessMu.Unlock()
		}
	}()
	source := s.sourceTag()
	if source == "" {
		return false
	}
	witnessID, found, err := s.store.MostRecentCleanVerdict(ctx, source, witnessMaxAge)
	if err != nil {
		s.logger.Warn("account_rsc_witness_lookup_failed", "error", err.Error())
		return false
	}
	if !found {
		s.logger.Warn("account_rsc_witness_unavailable", "hint", "no fresh clean verdict to prove channel vocabulary; denied stays suppressed")
		return false
	}
	probed = true
	token, err := s.accounts.DecryptedAccessToken(ctx, witnessID)
	if err != nil {
		s.logger.Warn("account_rsc_witness_token_failed", "account_id", witnessID, "error", err.Error())
		return false
	}
	witness := checker.Check(ctx, token)
	if witness.Verdict == VerdictClean {
		s.logger.Warn("account_rsc_witness_healed", "witness_account_id", witnessID)
		return true
	}
	s.logger.Warn("account_rsc_witness_still_suspect", "witness_account_id", witnessID, "witness_verdict", witness.Verdict)
	return false
}

// checkCall 是一次进行中的 RSC 检查：done 关闭后 result 可读。
// origin/trigger 是 leader 入场时的归因章，waiter 必须原样继承，不得改写成
// 自己的账号——否则 Build 降智与巡检/Web 撞车时会把通道隔离升级成身份连坐。
type checkCall struct {
	done    chan struct{}
	result  CheckResult
	origin  uint64
	trigger string
	// stored 是 leader 落库的最终 verdict(含 DeniedStreak 连击数)。
	// 等待方从 result(CheckResult)重组 verdict 时拿不到连击数——
	// 连击在 leader 侧基于历史 verdict 计算,不在探针结果里。nil 表示
	// leader 未走到落库(如取消),等待方沿用旧组装路径。
	stored *StoredVerdict
}

// CheckResult is the application-layer projection of one RSC probe outcome.
// The risk package must not depend on the infra implementation shape; the
// composition root adapts the concrete RSC checker onto this port type.
type CheckResult struct {
	Verdict        string
	BotFlagSource  int
	BotFlagDetails string
	RiskScore      float64
	HTTPStatus     int
	Error          string
	CheckedAt      time.Time
	// Source identifies the probe implementation that produced this result
	// (e.g. "sso_probe" / "rsc"). A cached clean verdict is only authoritative
	// while it came from the method currently in effect — grok.com stopped
	// delivering the homepage botFlag payload, so legacy homepage-era clean
	// verdicts read every account as healthy and must never short-circuit the
	// SSO probe. Set by the adapter; empty keeps legacy semantics.
	Source string
	// Suppressed marks a denied downgraded by the channel-vocabulary breaker;
	// the risk service answers it with witness re-validation.
	Suppressed bool
}

// Checker probes registration risk for one SSO identity.
type Checker interface {
	Check(ctx context.Context, ssoToken string) CheckResult
}

// buildProbeSourceTag marks verdicts produced by the Build-native probe
// (unlinked Build accounts only; the SSO probe stays the primary method).
const buildProbeSourceTag = "build_probe"

// Build-native probe verdicts (gateway differential probe outcome mapped).
const (
	BuildProbeClean        = "clean"
	BuildProbeDenied       = "denied"
	BuildProbeError        = "error"
	BuildProbeUnconfigured = "unconfigured"
)

// BuildProbeResult is the outcome of one Build-native differential probe.
type BuildProbeResult struct {
	// Verdict: clean (thinking seen on any path), denied (degraded on two
	// distinct exit paths), error (transport trouble / single-path), or
	// unconfigured (no reasoning build model available — probe disabled).
	Verdict   string
	Error     string
	Details   string
	CheckedAt time.Time
}

// BuildProber runs the Build-native risk probe for one unlinked Build
// account. Implemented by the gateway (tiny reasoning request through the
// real Build channel with egress differential); wired in the composition
// root so the risk package stays gateway-free.
type BuildProber interface {
	// ProbeBuildThinking runs one differential probe. degradedNodeID is the
	// egress node that served the degraded attempt (0 = direct): attempt 1
	// re-pins it to reproduce the degrade path; attempt 2 differentials over
	// a different exit.
	ProbeBuildThinking(ctx context.Context, accountID, degradedNodeID uint64) BuildProbeResult
}

// EgressQuarantiner takes over exit-IP scoped degradations: the account was
// exonerated by a clean RSC verdict, so the egress node that served the
// degraded attempt becomes the suspect. Implemented by the egress application
// service as a confirmed quarantine (a second in-window degrade observation is
// required before the 24h isolation; single events stay on the L2 soft
// cooldown) plus account migration and rotation enqueue.
type EgressQuarantiner interface {
	OnRscCleanDegrade(ctx context.Context, nodeID, degradedAccountID uint64)
	// ClearDegradeEvidence lifts the node's pending soft cooldown when the
	// verdict incriminates the account instead of the exit IP.
	ClearDegradeEvidence(nodeID uint64)
}

// EgressEvidenceRemover 是 RISK 归因(账号有罪)的补充撤销面:跨账号证据窗口
// 里该账号的观测必须移除; 若节点的硬隔离证据全部来自该有罪账号, 隔离本身
// 也应回滚。由 egress 服务实现; 未实现时仅退化为原有的软冷却解除。
type EgressEvidenceRemover interface {
	// RemoveAccountEvidence 从跨账号确认窗口移除指定账号的全部观测。
	RemoveAccountEvidence(nodeID, accountID uint64)
	// ReleaseIfEvidenceOnlyFrom 在节点处于质量隔离且窗口内证据仅来自
	// guiltyAccountID 时解除隔离(证据来自多个账号时保留隔离, 交被动守卫)。
	ReleaseIfEvidenceOnlyFrom(ctx context.Context, nodeID, guiltyAccountID uint64)
}

// New builds the service; defaults fill zero fields.
func New(cfg Config, accounts Accounts, store Store, checker Checker, logger *slog.Logger) *Service {
	cfg = normalizeConfig(cfg)
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg: cfg, accounts: accounts, store: store, checker: checker, logger: logger,
		sem: make(chan struct{}, cfg.Concurrency),
	}
}

func normalizeConfig(cfg Config) Config {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 2
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.OnDenied == "" {
		cfg.OnDenied = "flag"
	}
	cfg.OnDenied = strings.TrimSpace(cfg.OnDenied)
	if cfg.PatrolInterval <= 0 {
		cfg.PatrolInterval = 30 * 24 * time.Hour
	}
	if cfg.ErrorRetry <= 0 {
		cfg.ErrorRetry = time.Hour
	}
	if cfg.DeniedConfirmations <= 0 {
		cfg.DeniedConfirmations = 2
	}
	if cfg.DeniedConfirmations > 5 {
		cfg.DeniedConfirmations = 5
	}
	if cfg.DeniedTTL <= 0 {
		cfg.DeniedTTL = 24 * time.Hour
	}
	if cfg.PatrolTickEvery <= 0 {
		cfg.PatrolTickEvery = 15 * time.Minute
	}
	if cfg.PatrolBatchSize <= 0 {
		cfg.PatrolBatchSize = 50
	}
	return cfg
}

// config returns a snapshot of the runtime configuration.
func (s *Service) config() Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg
}

// SnapshotConfig is the exported form of config() for one-shot CLI/admin
// callers that must force-enable attribution without losing other fields.
func (s *Service) SnapshotConfig() Config { return s.config() }

// UpdateConfig replaces the runtime configuration (settings hot-reload).
// A concurrency change swaps the semaphore channel: waiters capture the
// channel at acquire time and release their token to that same channel, so
// an in-flight generation never strands or double-counts tokens across a
// swap.
func (s *Service) UpdateConfig(next Config) {
	if s == nil {
		return
	}
	next = normalizeConfig(next)
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	if cap(s.sem) != next.Concurrency {
		s.sem = make(chan struct{}, next.Concurrency)
	}
	s.cfg = next
}

// UpdateChecker atomically replaces the RSC probe implementation (check
// method or timeout changed at runtime). sourceTag names the new method and
// gates cached-clean freshness (see CheckResult.Source); pass "" to keep
// legacy semantics.
func (s *Service) UpdateChecker(checker Checker, sourceTag string) {
	if s == nil || checker == nil {
		return
	}
	s.cfgMu.Lock()
	s.checker = checker
	s.checkerTag = sourceTag
	s.cfgMu.Unlock()
}

// sourceTag returns the active probe method tag.
func (s *Service) sourceTag() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.checkerTag
}

// InvalidateStaleCleanVerdicts deletes cached clean verdicts produced by a
// different probe method. Called at boot and whenever the check method
// changes at runtime: those verdicts describe the world as seen through a
// dead detection method (homepage-era cleans are universally "clean" since
// grok.com stopped delivering botFlag fields) and would otherwise pin the
// exit-IP-suspect conclusion for a full patrol interval. denied/flagged
// verdicts are real detections and stay untouched.
func (s *Service) InvalidateStaleCleanVerdicts(ctx context.Context) {
	if s == nil {
		return
	}
	tag := s.sourceTag()
	if tag == "" {
		return
	}
	// build_probe 的 clean 缓存属于另一套探测体系（未关联 Build），不随
	// SSO 方法切换失效。
	removed, err := s.store.DeleteCleanVerdictsExceptSources(ctx, tag, buildProbeSourceTag)
	if err != nil {
		if !observability.IsShutdownCancellation(ctx, err) {
			s.logger.Warn("account_risk_stale_clean_purge_failed", "keep_source", tag, "error", err.Error())
		}
		return
	}
	if removed > 0 {
		s.logger.Info("account_risk_stale_cleans_purged", "count", removed, "keep_source", tag)
	}
}

// Enabled reports whether attribution is active.
func (s *Service) Enabled() bool { return s != nil && s.config().Enabled }

// PatrolEnabled reports whether the bucketed patrol loop should run; the
// patrol background task always starts and gates each tick here so the
// setting can flip at runtime without a restart.
func (s *Service) PatrolEnabled() bool {
	if s == nil {
		return false
	}
	return s.config().PatrolEnabled
}

func (s *Service) PatrolTickEvery() time.Duration {
	if s == nil {
		return 15 * time.Minute
	}
	return s.config().PatrolTickEvery
}

func (s *Service) PatrolBatchSize() int {
	if s == nil {
		return 50
	}
	return s.config().PatrolBatchSize
}

// maxQueuedAttributions bounds detached attribution goroutines (admission
// ceiling before per-credential dedup): degrade storms merge/drop instead of
// accumulating waiters on the RSC semaphore.
const maxQueuedAttributions = 64

// riskyVerdictPageLimit mirrors the repository page size; the reconcile loop
// stops paging once a page comes back short. Kept in sync by test.
const riskyVerdictPageLimit = 10000

// OnDegraded is the gateway withhold hook. It never blocks the retry loop.
// Admission happens BEFORE the goroutine: per-credential dedup via the same
// inflight set used by checkNow plus a hard cap on queued attributions, so a
// degrade storm cannot accumulate unbounded detached goroutines waiting on the
// RSC semaphore. Dropped events are safe: the verdict cache plus patrol
// reconcile converge the same state later.
func (s *Service) OnDegraded(ctx context.Context, credential accountdomain.Credential, egressNodeID uint64) {
	if !s.Enabled() {
		return
	}
	if _, running := s.admissionDedup.LoadOrStore(credential.ID, struct{}{}); running {
		return
	}
	if n := s.pending.Add(1); n > maxQueuedAttributions {
		s.pending.Add(-1)
		s.admissionDedup.Delete(credential.ID)
		s.logger.Warn("account_risk_attribution_dropped", "account_id", credential.ID, "queued", n-1)
		return
	}
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.config().Timeout*2+10*time.Second)
	go func() {
		defer func() {
			s.pending.Add(-1)
			s.admissionDedup.Delete(credential.ID)
			cancel()
		}()
		// 归因链路(身份解析/RSC 探测/后果落地)panic 不得击穿进程。
		if err := batch.Do(detached, func(taskCtx context.Context) error {
			s.attribute(taskCtx, credential, egressNodeID)
			return nil
		}); err != nil {
			var panicErr *batch.PanicError
			if errors.As(err, &panicErr) {
				s.logger.Error("account_risk_attribution_panic", "account_id", credential.ID, "panic", panicErr.Value, "stack", string(panicErr.Stack))
			}
		}
	}()
}

func (s *Service) attribute(ctx context.Context, credential accountdomain.Credential, egressNodeID uint64) {
	s.attributeWithTrigger(ctx, credential, egressNodeID, accountdomain.RiskTriggerDegrade)
}

func (s *Service) attributeWithTrigger(ctx context.Context, credential accountdomain.Credential, egressNodeID uint64, trigger string) {
	if trigger == "" {
		trigger = accountdomain.RiskTriggerDegrade
	}
	webID, linked, err := s.resolveWebIdentity(ctx, credential)
	if err != nil {
		s.logger.Warn("account_risk_resolve_failed", "account_id", credential.ID, "error", err.Error())
		return
	}
	if !linked {
		// 未关联 SSO 的 Build 走 Build 原生差分探针兜底（有关联时 SSO 探针
		// 始终优先：它的信号是账号级的，不受出口 IP 质量影响）。
		if credential.Provider == accountdomain.ProviderBuild && s.buildProber != nil && s.config().BuildProbeEnabled {
			s.attributeBuildNative(ctx, credential, egressNodeID)
			return
		}
		s.logger.Info("account_risk_unlinked_behavior_attribution", "account_id", credential.ID)
		return
	}
	if verdict, fresh := s.freshVerdict(ctx, webID); fresh {
		s.applyConsequences(ctx, credential.ID, webID, verdict, egressNodeID)
		return
	}
	result := s.checkNow(ctx, webID, credential.ID, trigger)
	s.applyConsequences(ctx, credential.ID, webID, result, egressNodeID)
}

// attributeBuildNative runs the Build-native fallback for an unlinked Build
// account: cached verdict reuse first, then one differential probe. The
// verdict is keyed by the Build account itself and consequences stay
// channel-scoped to it.
func (s *Service) attributeBuildNative(ctx context.Context, credential accountdomain.Credential, egressNodeID uint64) {
	if verdict, fresh := s.freshVerdictFor(ctx, credential.ID, buildProbeSourceTag); fresh {
		s.applyConsequences(ctx, credential.ID, credential.ID, verdict, egressNodeID)
		return
	}
	verdict := s.checkNowBuild(ctx, credential, egressNodeID)
	s.applyConsequences(ctx, credential.ID, credential.ID, verdict, egressNodeID)
}

// buildWitnessMaxAge bounds how old a build-probe clean witness may be to
// vouch for the differential verdict vocabulary.
const buildWitnessMaxAge = 7 * 24 * time.Hour

// checkNowBuild runs one Build-native differential probe under the shared
// concurrency gate. No singleflight: the per-credential admission dedup
// upstream already merges concurrent degrade events for the same account.
// A denied verdict additionally requires a recent build-probe clean witness
// (differential "both paths degraded" can still be all-dirty-IP); without
// one it degrades to error and is retried on the next event.
func (s *Service) checkNowBuild(ctx context.Context, credential accountdomain.Credential, degradedNodeID uint64) StoredVerdict {
	now := time.Now().UTC()
	if s.buildProber == nil {
		return StoredVerdict{Verdict: VerdictError, Error: "build prober unavailable", Source: buildProbeSourceTag, CheckedAt: now}
	}
	// Capture the semaphore generation up front (hot-reload may swap it).
	s.cfgMu.RLock()
	sem := s.sem
	s.cfgMu.RUnlock()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		return StoredVerdict{Verdict: VerdictError, Error: "concurrency gate timeout", Source: buildProbeSourceTag, CheckedAt: now}
	}
	result := s.buildProber.ProbeBuildThinking(ctx, credential.ID, degradedNodeID)
	verdict := StoredVerdict{Source: buildProbeSourceTag, CheckedAt: result.CheckedAt, OriginAccountID: credential.ID, Trigger: accountdomain.RiskTriggerDegrade}
	switch result.Verdict {
	case BuildProbeClean:
		verdict.Verdict = VerdictClean
		verdict.BotFlagDtl = "build probe thinking_ok: " + result.Details
	case BuildProbeDenied:
		verdict.Verdict = VerdictDenied
		verdict.BotFlagDtl = "build probe differential_degraded: " + result.Details
		// 见证人门控：差分双降仍可能是"两条路都脏 IP"，需要近期 clean 见证。
		witnessID, found, err := s.store.MostRecentCleanVerdict(ctx, buildProbeSourceTag, buildWitnessMaxAge)
		if err != nil || !found {
			verdict.Verdict = VerdictError
			verdict.Error = "denied suppressed: no build-probe clean witness (both paths degraded could be dirty IPs)"
			verdict.BotFlagDtl = "build probe suppressed: " + result.Details
			s.logger.Warn("account_rsc_build_denied_suppressed", "account_id", credential.ID, "reason", "no clean witness")
			break
		}
		witness, err := s.store.GetRiskVerdict(ctx, witnessID)
		if err != nil || time.Now().UTC().Sub(witness.CheckedAt) > buildWitnessMaxAge {
			verdict.Verdict = VerdictError
			verdict.Error = "denied suppressed: build-probe clean witness stale"
			verdict.BotFlagDtl = "build probe suppressed: " + result.Details
			s.logger.Warn("account_rsc_build_denied_suppressed", "account_id", credential.ID, "reason", "witness stale")
		}
	case BuildProbeUnconfigured:
		// 探针未配置（无可用推理 Build 模型）：不落库不重试，保持行为兜底。
		s.logger.Info("account_rsc_build_probe_unconfigured", "account_id", credential.ID, "detail", result.Details)
		return StoredVerdict{Verdict: VerdictError, Error: "build probe unconfigured: " + result.Details, Source: buildProbeSourceTag, CheckedAt: now}
	default:
		verdict.Verdict = VerdictError
		verdict.Error = result.Error
		verdict.BotFlagDtl = "build probe inconclusive: " + result.Details
	}
	s.saveVerdictGuarded(ctx, credential.ID, verdict)
	perfmetrics.Default.Inc("account_rsc_check_total", perfmetrics.Labels{
		Subsystem: "account", Operation: "rsc_check", Outcome: verdict.Verdict,
	})
	s.logger.Info("account_rsc_checked", "account_id", credential.ID, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", rsc.RedactSecrets(verdict.BotFlagDtl))
	return verdict
}

// ClearIdentityVerdicts removes the persisted verdict behind an operator's
// manual risk-status clear. Without this, denied/flagged verdicts stay
// permanently fresh: the next degrade event (or startup reconcile) would
// instantly re-flag the identity the operator just restored.
func (s *Service) ClearIdentityVerdicts(ctx context.Context, credential accountdomain.Credential) error {
	if s == nil {
		return nil
	}
	webID, linked, err := s.resolveWebIdentity(ctx, credential)
	if err != nil {
		return err
	}
	if !linked {
		// 未关联 Build 的原生探针 verdict 挂在账号自身：人工清标时同样要
		// 删除，否则 denied 永久新鲜会把刚清的标原样打回来。
		if credential.Provider == accountdomain.ProviderBuild {
			if err := s.store.DeleteRiskVerdict(ctx, credential.ID); err != nil {
				return err
			}
			s.logger.Info("account_risk_verdict_cleared", "account_id", credential.ID, "web_account_id", 0)
		}
		return nil
	}
	verdict, verr := s.store.GetRiskVerdict(ctx, webID)
	if verr != nil && !errors.Is(verr, ErrNotFound) {
		return verr
	}
	buildOrigin := verr == nil && verdict.OriginAccountID == credential.ID && credential.Provider == accountdomain.ProviderBuild
	if credential.Provider == accountdomain.ProviderBuild {
		// Build 自己降智产生的结论：清这个 Build 时删掉身份键，避免对账按
		// origin 把刚清的标打回来。SSO 巡检/Web 降智产生的身份结论
		// （origin==webID）必须保留：删掉会让身份从 ListPatrolDue 永久消失
		// （无 verdict 行 = 不巡检），兄弟通道继续挂标却没有可对账的 denied。
		// 启动对账会按保留的 SSO-origin 把该 Build 再标回去——这是身份级
		// 定罪的正确收敛；要持久解开整组，清 Web 账号。
		if buildOrigin {
			if err := s.store.DeleteRiskVerdict(ctx, webID); err != nil {
				return err
			}
		}
		if err := s.store.DeleteRiskVerdict(ctx, credential.ID); err != nil {
			return err
		}
	} else if err := s.store.DeleteRiskVerdict(ctx, webID); err != nil {
		return err
	}
	// 解 Web 身份 = 解整组连坐标志，避免只清 Web、Build/Console 继续 rsc_denied。
	if credential.Provider == accountdomain.ProviderWeb {
		for _, id := range s.identityGroupIDs(ctx, webID) {
			if id == credential.ID {
				continue
			}
			if err := s.accounts.SetAccountRiskStatus(ctx, id, false); err != nil {
				s.logger.Warn("account_risk_group_unflag_failed", "account_id", id, "web_account_id", webID, "error", err.Error())
			}
		}
	}
	s.logger.Info("account_risk_verdict_cleared", "account_id", credential.ID, "web_account_id", webID)
	return nil
}

func (s *Service) resolveWebIdentity(ctx context.Context, credential accountdomain.Credential) (uint64, bool, error) {
	if credential.Provider == accountdomain.ProviderWeb {
		return credential.ID, true, nil
	}
	if credential.Provider != accountdomain.ProviderBuild {
		return 0, false, nil
	}
	return s.accounts.LinkedWebAccountID(ctx, credential.ID)
}

// freshVerdict returns a cached SSO verdict when still authoritative.
func (s *Service) freshVerdict(ctx context.Context, webID uint64) (StoredVerdict, bool) {
	return s.freshVerdictFor(ctx, webID, s.sourceTag())
}

// freshVerdictFor generalizes freshness over the producing probe tag: a
// cached clean verdict is only authoritative while it came from the method
// that owns this identity (SSO tag for web identities, build_probe for
// unlinked builds).
func (s *Service) freshVerdictFor(ctx context.Context, id uint64, expectedTag string) (StoredVerdict, bool) {
	verdict, err := s.store.GetRiskVerdict(ctx, id)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.logger.Warn("account_risk_read_failed", "account_id", id, "error", err.Error())
		}
		return StoredVerdict{}, false
	}
	now := time.Now().UTC()
	cfg := s.config()
	switch verdict.Verdict {
	case VerdictDenied, VerdictFlagged:
		// 未达确认次数:ErrorRetry 内视作新鲜(防抖),过后重探补确认;
		// 已确认:DeniedTTL 内新鲜,过期允许重探让误判自愈(clean 可覆盖)。
		// 此前 denied 永久新鲜是"单次采样永久定罪"的一半根源。
		if !verdictConfirmed(cfg, verdict) {
			return verdict, now.Sub(verdict.CheckedAt) < cfg.ErrorRetry
		}
		return verdict, now.Sub(verdict.CheckedAt) < cfg.DeniedTTL
	case VerdictClean:
		// Homepage-era cleans were produced after grok.com stopped delivering
		// botFlag fields — every account read as healthy — and must never
		// short-circuit the probe of the method now in effect.
		if expectedTag != "" && verdict.Source != expectedTag {
			return StoredVerdict{}, false
		}
		return verdict, now.Sub(verdict.CheckedAt) < cfg.PatrolInterval
	case VerdictError:
		return verdict, now.Sub(verdict.CheckedAt) < cfg.ErrorRetry
	}
	return StoredVerdict{}, false
}

// checkNow runs one bounded RSC check. Concurrent checks for the same web
// identity are merged singleflight-style: later callers wait for and share the
// first caller's result instead of erroring out (a hard error here used to
// strand the second degraded account's cooldown forever). A merged (shared)
// result is returned to every caller but persisted only once by the leader.
func (s *Service) checkNow(ctx context.Context, webID, originAccountID uint64, trigger string) StoredVerdict {
	call := &checkCall{done: make(chan struct{}), origin: originAccountID, trigger: trigger}
	if actual, loaded := s.checkInflight.LoadOrStore(webID, call); loaded {
		// 已有同身份检查在跑：等它完成并共享结果。等待不占并发闸。
		leader := actual.(*checkCall)
		s.waiters.Add(1)
		defer s.waiters.Add(-1)
		select {
		case <-leader.done:
			shared := storedFromCheck(leader.result)
			if leader.stored != nil {
				// 继承 leader 的最终 verdict(含 DeniedStreak):连击数按
				// 落库历史计算,等待方无法从探针结果自行推导。
				shared = *leader.stored
			}
			shared.OriginAccountID = leader.origin
			shared.Trigger = leader.trigger
			return shared
		case <-ctx.Done():
			return StoredVerdict{Verdict: VerdictError, Error: "wait for in-flight check: " + ctx.Err().Error(), CheckedAt: time.Now().UTC(), OriginAccountID: originAccountID, Trigger: trigger}
		}
	}
	defer func() {
		s.checkInflight.Delete(webID)
		close(call.done)
	}()
	// Capture the semaphore/checker generation up front: a concurrent
	// UpdateConfig/UpdateChecker may swap them, and a token acquired from the
	// old channel must release to the same channel.
	s.cfgMu.RLock()
	sem := s.sem
	checker := s.checker
	s.cfgMu.RUnlock()
	select {
	case sem <- struct{}{}:
		defer func() { <-sem }()
	case <-ctx.Done():
		call.result = CheckResult{Verdict: VerdictError, Error: "concurrency gate timeout", CheckedAt: time.Now().UTC()}
		return StoredVerdict{Verdict: VerdictError, Error: call.result.Error, Source: s.sourceTag(), CheckedAt: call.result.CheckedAt, OriginAccountID: originAccountID, Trigger: trigger}
	}
	token, err := s.accounts.DecryptedAccessToken(ctx, webID)
	if err != nil {
		// 与主路径一致盖上触发源章:error verdict 本身不携带后果,但保持
		// "每条落库 verdict 都记录触发账号"的不变量完整。
		verdict := StoredVerdict{Verdict: VerdictError, Error: "decrypt sso: " + err.Error(), Source: s.sourceTag(), CheckedAt: time.Now().UTC(), OriginAccountID: originAccountID, Trigger: trigger}
		call.result = CheckResult{Verdict: VerdictError, Error: verdict.Error, CheckedAt: verdict.CheckedAt}
		s.saveVerdictGuarded(ctx, webID, verdict)
		return verdict
	}
	result := checker.Check(ctx, token)
	// 通道词汇熔断压下 denied 时,尝试见证人复验: 用最近一次 clean 的身份
	// 再探一次,看到 thinking 即证明通道词汇还活着(熔断随之自愈),随即重跑
	// 本次判定。否则纯 denied 风暴(整池真被风控)会把熔断器永久锁死——
	// 清标后的重新归因再也标不上。复验限频一次/10min。
	if result.Suppressed && s.revalidateChannelWitness(ctx, checker) {
		if retried := checker.Check(ctx, token); !retried.Suppressed {
			result = retried
		}
	}
	call.result = result
	verdict := storedFromCheck(result)
	if verdict.Verdict == "" {
		verdict.Verdict = VerdictError
	}
	// 记录触发源账号:请求路径/对账重放后果时只打到该账号(通道隔离),
	// 不连坐 verdict 键所在的 Web 身份。巡检固定传 origin==webID,denied
	// 时由 patrolTick 连坐身份组。
	verdict.OriginAccountID = originAccountID
	verdict.Trigger = trigger
	if verdict.Verdict == VerdictDenied {
		verdict.DeniedStreak = s.nextDeniedStreak(ctx, webID)
	}
	s.saveVerdictGuarded(ctx, webID, verdict)
	call.stored = &verdict
	perfmetrics.Default.Inc("account_rsc_check_total", perfmetrics.Labels{
		Subsystem: "account", Operation: "rsc_check", Outcome: verdict.Verdict,
	})
	// 日志与落库同一脱敏规则（rsc.RedactSecrets）：BotFlagDtl 是上游可控
	// 文本，此前仓储层落库前脱敏但日志打原始值——同一载荷两套口径。
	s.logger.Info("account_rsc_checked", "account_id", webID, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", rsc.RedactSecrets(verdict.BotFlagDtl))
	return verdict
}

// verdictOrigin resolves the channel-scoped replay target: the account whose
// degrade produced the verdict; legacy rows (origin=0) fall back to the web
// identity key itself.
func verdictOrigin(verdict StoredVerdict, webID uint64) uint64 {
	if verdict.OriginAccountID != 0 {
		return verdict.OriginAccountID
	}
	return webID
}

// storedFromCheck projects a checker result onto the persisted verdict shape.
// The leader and the merged waiters must map identical fields: the previous
// hand-copied second block silently drifted on field additions.
func storedFromCheck(result CheckResult) StoredVerdict {
	source := result.Source
	if source == "" {
		source = "rsc" // legacy default; the adapter always tags real probes
	}
	return StoredVerdict{
		Verdict: result.Verdict, BotFlagSrc: result.BotFlagSource, BotFlagDtl: result.BotFlagDetails,
		RiskScore: result.RiskScore, HTTPStatus: result.HTTPStatus, Error: result.Error,
		Source: source, CheckedAt: result.CheckedAt,
	}
}

// nextDeniedStreak 统计一个身份的连续 denied 次数:已有 denied/flagged
// verdict(saveVerdictGuarded 使其对 error 具有粘性)则 +1,否则从 1 起。
// 连续次数达到 DeniedConfirmations 才进入处置——单次瞬时误读(2026-08-28
// 生产首批 7 连发被整批降级服务)不可能再直接定罪。
func (s *Service) nextDeniedStreak(ctx context.Context, webID uint64) int {
	if existing, err := s.store.GetRiskVerdict(ctx, webID); err == nil && existing.Risky() {
		return existing.DeniedStreak + 1
	}
	return 1
}

// verdictConfirmed 报告一条 risky verdict 是否已达到处置所需的连续确认
// 次数。flagged 视为旧数据已确认;denied 需要 DeniedConfirmations 次连续。
func verdictConfirmed(cfg Config, verdict StoredVerdict) bool {
	if !verdict.Risky() {
		return false
	}
	if verdict.Verdict == VerdictFlagged {
		return true
	}
	// Build 差分探针的 denied 自带确认:它本来就要求两个不同出口都降智
	// 才出 denied,单次采样问题不适用于该路径。
	if verdict.Source == buildProbeSourceTag {
		return true
	}
	confirmations := cfg.DeniedConfirmations
	if confirmations <= 0 {
		confirmations = 2
	}
	return verdict.DeniedStreak >= confirmations
}

// saveVerdictGuarded persists a verdict unless it would clobber a strictly
// better one: a transient error must never overwrite a denied/flagged verdict
// ("denied 永久缓存" invariant — external review found the unconditional
// save could regress a denial to error when a late check errored).
func (s *Service) saveVerdictGuarded(ctx context.Context, webID uint64, verdict StoredVerdict) {
	if verdict.Verdict == VerdictError {
		if existing, err := s.store.GetRiskVerdict(ctx, webID); err == nil && existing.Risky() {
			s.logger.Info("account_rsc_verdict_kept", "account_id", webID, "kept", existing.Verdict, "incoming", verdict.Verdict)
			return
		}
	}
	if err := s.store.SaveRiskVerdict(ctx, webID, verdict); err != nil {
		s.logger.Warn("account_rsc_verdict_save_failed", "account_id", webID, "error", err.Error())
	}
}

// applyConsequences acts on a verdict for the degraded account.
func (s *Service) applyConsequences(ctx context.Context, degradedID, webID uint64, verdict StoredVerdict, egressNodeID uint64) {
	cfg := s.config()
	switch {
	case verdict.Risky() && !verdictConfirmed(cfg, verdict):
		// 单次 denied 未达确认次数:只记录,不处置、不连坐。ErrorRetry 后
		// 巡检/归因会重探,连续一致才进入处置分支。
		s.logger.Warn("account_risk_denied_pending_confirmation",
			"web_account_id", webID, "degraded_account_id", degradedID,
			"streak", verdict.DeniedStreak, "confirmations", cfg.DeniedConfirmations,
			"trigger", verdict.Trigger)
	case verdict.Risky():
		// 账号有罪 → 出口节点无辜。三层撤销:
		// (1) 清未决软冷却(L2);
		// (2) 若节点已被质量隔离且窗口内证据确实只来自本有罪账号, 回滚隔离
		//     (ReleaseEgressQuarantine), 避免整个隔离周期白扣——检查必须在
		//     观测移除之前:移除后窗口为空,"仅含有罪账号"不可判定,空洞真值
		//     不得释放跨账号确认的隔离;
		// (3) 从跨账号证据窗口移除该账号的观测——残留观测会在窗口期内与另一
		//     账号的一次瞬时降智凑满阈值, 错杀无辜节点。
		if egressNodeID != 0 && s.egressQuarantiner != nil {
			s.egressQuarantiner.ClearDegradeEvidence(egressNodeID)
			if remover, ok := s.egressQuarantiner.(EgressEvidenceRemover); ok {
				remover.ReleaseIfEvidenceOnlyFrom(ctx, egressNodeID, degradedID)
				remover.RemoveAccountEvidence(egressNodeID, degradedID)
			}
		}
		// 处置面默认收窄到"实际降智被抓的通道账号"(degradedID): Build 通道
		// 触发只标/停 Build,不级联 Web/Console。SSO 身份本身被判定风控
		// (degradedID==webID 且 origin 明确为该 SSO)时连坐其余渠道,否则
		// SSO 已标风控后其他渠道再降智无法调度探针,会永远停在冷却。
		// origin==0 的旧数据不连坐,避免把 Build 降智遗留 verdict 扩到整组。
		s.applyDeniedAction(ctx, degradedID, webID, verdict)
		if degradedID == webID && webID != 0 && verdict.OriginAccountID == webID {
			members := s.identityGroupIDs(ctx, webID)
			s.logger.Warn("account_risk_sso_denied_cascade", "web_account_id", webID, "members", members, "verdict", verdict.Verdict)
			for _, id := range members {
				if id == webID {
					continue
				}
				s.applyDeniedAction(ctx, id, webID, verdict)
			}
		}
	case verdict.Verdict == VerdictClean:
		// The account is innocent: the degrade was exit-IP scoped. Lift the
		// missing-thinking cooldown so the account stays schedulable.
		if err := s.accounts.ClearMissingThinkingCooldown(ctx, degradedID); err != nil {
			s.logger.Warn("account_risk_clean_clear_failed", "account_id", degradedID, "error", err.Error())
			return
		}
		s.logger.Info("account_risk_clean_ip_suspect", "account_id", degradedID, "web_account_id", webID, "egress_node_id", egressNodeID)
		// Hand the exit-IP suspect to the egress layer. The implementation
		// requires a second in-window degrade observation before the 24h
		// quarantine: one exonerated degrade is suspicion, not conviction.
		if egressNodeID != 0 && s.egressQuarantiner != nil {
			s.egressQuarantiner.OnRscCleanDegrade(ctx, egressNodeID, degradedID)
		}
	}
}

// applyDeniedAction executes the configured onDenied policy for one account.
func (s *Service) applyDeniedAction(ctx context.Context, id, webID uint64, verdict StoredVerdict) {
	switch s.config().OnDenied {
	case "flag":
		s.flagAccount(ctx, id, webID, verdict)
	case "disable":
		s.disableAccount(ctx, id, webID, verdict)
	default:
		s.logger.Warn("account_risk_marked", "account_id", id, "web_account_id", webID, "verdict", verdict.Verdict, "details", verdict.BotFlagDtl)
	}
}

// SetEgressQuarantiner installs the exit-IP quarantine bridge; nil keeps
// degrades account-scoped only.
func (s *Service) SetEgressQuarantiner(quarantiner EgressQuarantiner) {
	if s == nil || quarantiner == nil {
		return
	}
	s.egressQuarantiner = quarantiner
}

// SetBuildProber installs the Build-native probe fallback (unlinked Build
// accounts); nil keeps the behavioral-only path.
func (s *Service) SetBuildProber(prober BuildProber) {
	if s == nil || prober == nil {
		return
	}
	s.buildProber = prober
}

// flagAccount 给"实际降智被抓的通道账号"打长期风控标记(risk_status =
// rsc_denied),不级联身份组其余通道。账号保持启用:标志反映真实账号
// 状态(可用但被风控路由),并永久排除调度直到人工解除。rsc_denied 已把
// 账号排除出调度,残留的 missing-thinking 冷却只是 UI 噪音,顺手清掉。
func (s *Service) flagAccount(ctx context.Context, id, webID uint64, verdict StoredVerdict) {
	origin := verdict.OriginAccountID
	if origin == 0 {
		origin = webID
	}
	checkedAt := verdict.CheckedAt
	if checkedAt.IsZero() {
		checkedAt = time.Now().UTC()
	}
	if err := s.accounts.SetAccountRiskAttribution(ctx, id, true, verdict.Trigger, origin, rsc.RedactSecrets(verdict.BotFlagDtl), checkedAt); err != nil {
		s.logger.Error("account_risk_flag_failed", "account_id", id, "error", err.Error())
		return
	}
	s.clearCooldownQuietly(ctx, id)
	if id != webID {
		s.logger.Warn("account_risk_flagged", "account_id", id, "web_account_id", webID, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", rsc.RedactSecrets(verdict.BotFlagDtl))
		return
	}
	s.logger.Warn("account_risk_flagged", "account_id", id, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", rsc.RedactSecrets(verdict.BotFlagDtl))
}

// disableAccount 停用"实际降智被抓的通道账号"(通道隔离,不级联身份组)。
func (s *Service) disableAccount(ctx context.Context, id, webID uint64, verdict StoredVerdict) {
	const reason = "registration risk (RSC)"
	if err := s.accounts.SetAccountEnabled(ctx, id, false, reason); err != nil {
		s.logger.Error("account_risk_disable_failed", "account_id", id, "error", err.Error())
		return
	}
	// 停用是更强的调度闸,残留冷却只是 UI 噪音。
	s.clearCooldownQuietly(ctx, id)
	if id != webID {
		s.logger.Warn("account_risk_disabled", "account_id", id, "web_account_id", webID, "verdict", verdict.Verdict)
		return
	}
	s.logger.Warn("account_risk_disabled", "account_id", id, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", rsc.RedactSecrets(verdict.BotFlagDtl))
}

// clearCooldownQuietly lifts a missing-thinking/empty-stream cooldown after a
// risk verdict made it redundant (flag/disable already gate scheduling). A
// failure only means the cooldown expires on its own; it never blocks the
// verdict consequences.
func (s *Service) clearCooldownQuietly(ctx context.Context, id uint64) {
	if err := s.accounts.ClearMissingThinkingCooldown(ctx, id); err != nil {
		s.logger.Warn("account_risk_cooldown_clear_failed", "account_id", id, "error", err.Error())
	}
}

// PatrolCutoffs returns the freshness bounds the wiring layer feeds into the
// patrol query.
func (s *Service) PatrolCutoffs() (patrolInterval, errorRetry time.Time) {
	cfg := s.config()
	now := time.Now().UTC()
	return now.Add(-cfg.PatrolInterval), now.Add(-cfg.ErrorRetry)
}

// ReconcileRiskyVerdicts converges risk_status flags with the verdict table at
// startup by replaying consequences for the verdict origin. An explicit
// SSO-origin denial (OriginAccountID==webID) fans out to the identity group;
// Build/Console origins and legacy origin=0 stay channel-scoped. markOnly
// keeps logging only.
func (s *Service) ReconcileRiskyVerdicts(ctx context.Context) {
	if !s.Enabled() {
		return
	}
	reconciled := 0
	after := uint64(0)
	for {
		ids, err := s.store.ListRiskyVerdictAccountIDsAfter(ctx, after)
		if err != nil {
			if !observability.IsShutdownCancellation(ctx, err) {
				s.logger.Warn("account_risk_reconcile_list_failed", "cursor", after, "error", err.Error())
			}
			return
		}
		for _, webID := range ids {
			if ctx.Err() != nil {
				return
			}
			verdict, err := s.store.GetRiskVerdict(ctx, webID)
			if err != nil {
				continue
			}
			s.applyConsequences(ctx, verdictOrigin(verdict, webID), webID, verdict, 0)
			reconciled++
		}
		if len(ids) < riskyVerdictPageLimit {
			break
		}
		after = ids[len(ids)-1]
	}
	if reconciled > 0 {
		s.logger.Info("account_risk_reconciled", "identities", reconciled)
	}
}

// PatrolTick re-checks due web identities and applies consequences: a
// clean→denied transition found by patrol is an SSO-origin denial and
// fans out to the whole identity group (Web+Build+Console).
func (s *Service) PatrolTick(ctx context.Context, dueIDs []uint64) {
	s.patrolTick(ctx, dueIDs, false)
}

// PatrolTickForced runs the same re-check loop without the Enabled() gate,
// for one-shot CLI/admin sweeps while runtime attribution is otherwise off.
func (s *Service) PatrolTickForced(ctx context.Context, dueIDs []uint64) {
	s.patrolTick(ctx, dueIDs, true)
}

// AttributeNow runs one synchronous request-path attribution for the given
// credential, bypassing the Enabled() gate. Used by the one-shot CLI to
// verify channel-scoped flagging (Build degrade flags Build only).
func (s *Service) AttributeNow(ctx context.Context, credential accountdomain.Credential) {
	s.AttributeNowWithTrigger(ctx, credential, accountdomain.RiskTriggerDegrade)
}

// AttributeNowWithTrigger is the admin "check now" path. Trigger should be
// manual so the UI does not label an operator click as request-path degrade.
func (s *Service) AttributeNowWithTrigger(ctx context.Context, credential accountdomain.Credential, trigger string) {
	if s == nil {
		return
	}
	s.attributeWithTrigger(ctx, credential, 0, trigger)
}

func (s *Service) patrolTick(ctx context.Context, dueIDs []uint64, force bool) {
	if !force && !s.Enabled() {
		return
	}
	for index, webID := range dueIDs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		// 巡检探针打的是 SSO 身份本身,不是某条通道的降智归因。origin 固定
		// 为 webID:denied 表示注册风控,必须连坐 Web/Build/Console,否则 SSO
		// 已标风控后 Build 再降智时无法再调度 SSO 探针,Build 会永远停在冷却。
		verdict := s.checkNow(ctx, webID, webID, accountdomain.RiskTriggerPatrol)
		s.applyConsequences(ctx, webID, webID, verdict, 0)
		// 探针间隔抖动:2026-08-28 首批 7 连发(9 秒)触发上游对整批连接的
		// 降级服务,7 个健康身份被一次性误判。0.5-1.2s 随机间隔打散批次
		// 节奏,期间保持可取消。
		if index < len(dueIDs)-1 {
			gap := time.Duration(500+rand.IntN(700)) * time.Millisecond
			select {
			case <-ctx.Done():
				return
			case <-time.After(gap):
			}
		}
	}
}

// identityGroupIDs returns the SSO web identity plus every linked Build and
// Console account. SSO-origin denials apply consequences to this whole set
// so a registration-risk identity cannot leave sibling channels schedulable.
func (s *Service) identityGroupIDs(ctx context.Context, webID uint64) []uint64 {
	seen := map[uint64]struct{}{webID: {}}
	ids := []uint64{webID}
	add := func(extra []uint64) {
		for _, id := range extra {
			if id == 0 {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if builds, err := s.accounts.LinkedBuildAccountIDs(ctx, webID); err != nil {
		s.logger.Warn("account_risk_list_builds_failed", "web_account_id", webID, "error", err.Error())
	} else {
		add(builds)
	}
	if consoles, err := s.accounts.LinkedConsoleAccountIDs(ctx, webID); err != nil {
		s.logger.Warn("account_risk_list_consoles_failed", "web_account_id", webID, "error", err.Error())
	} else {
		add(consoles)
	}
	return ids
}
