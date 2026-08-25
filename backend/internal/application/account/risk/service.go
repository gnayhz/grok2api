// Package risk attributes quality-withhold events to account-level RSC
// verdicts. A Build account inherits the verdict of its linked Web SSO
// identity; accounts without a link fall back to the gateway's escalating
// missing-thinking penalties.
package risk

import (
	"context"
	"errors"
	"log/slog"
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
}

// relationalVerdict decouples the service from the persistence layer shape.
type relationalVerdict = StoredVerdict

// Config mirrors config.AccountRiskRSCConfig at runtime.
type Config struct {
	Enabled        bool
	Concurrency    int
	Timeout        time.Duration
	OnDenied       string // disable | markOnly | flag
	PatrolInterval time.Duration
	ErrorRetry     time.Duration
}

// Accounts is the account-service surface attribution needs. The concrete
// *accountapp.Service satisfies it; tests substitute fakes.
type Accounts interface {
	DecryptedAccessToken(ctx context.Context, id uint64) (string, error)
	LinkedWebAccountID(ctx context.Context, buildAccountID uint64) (uint64, bool, error)
	LinkedBuildAccountIDs(ctx context.Context, webAccountID uint64) ([]uint64, error)
	// LinkedConsoleAccountIDs：同一 Web 身份的 Console 账号也属于身份组，
	// denied 处置必须一并覆盖（否则 Console 仍可被调度）。
	LinkedConsoleAccountIDs(ctx context.Context, webAccountID uint64) ([]uint64, error)
	SetAccountEnabled(ctx context.Context, id uint64, enabled bool, reason string) error
	SetAccountRiskStatus(ctx context.Context, id uint64, flagged bool) error
	ClearMissingThinkingCooldown(ctx context.Context, id uint64) error
}

// Service runs event-driven and patrol RSC attribution.
type Service struct {
	cfg      Config
	accounts Accounts
	store    Store
	checker  Checker
	logger   *slog.Logger
	// egressQuarantiner receives exit-IP quarantine duty when attribution
	// exonerates the account (RSC clean); nil keeps the degrade account-scoped.
	egressQuarantiner EgressQuarantiner

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
}

// checkCall 是一次进行中的 RSC 检查：done 关闭后 result 可读。
type checkCall struct {
	done   chan struct{}
	result CheckResult
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
}

// Checker probes registration risk for one SSO identity.
type Checker interface {
	Check(ctx context.Context, ssoToken string) CheckResult
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
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		cfg: cfg, accounts: accounts, store: store, checker: checker, logger: logger,
		sem: make(chan struct{}, cfg.Concurrency),
	}
}

// Enabled reports whether attribution is active.
func (s *Service) Enabled() bool { return s != nil && s.cfg.Enabled }

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
	detached, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Timeout*2+10*time.Second)
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
	webID, linked, err := s.resolveWebIdentity(ctx, credential)
	if err != nil {
		s.logger.Warn("account_risk_resolve_failed", "account_id", credential.ID, "error", err.Error())
		return
	}
	if !linked {
		s.logger.Info("account_risk_unlinked_behavior_attribution", "account_id", credential.ID)
		return
	}
	if verdict, fresh := s.freshVerdict(ctx, webID); fresh {
		s.applyConsequences(ctx, credential.ID, webID, verdict, egressNodeID)
		return
	}
	result := s.checkNow(ctx, webID)
	s.applyConsequences(ctx, credential.ID, webID, result, egressNodeID)
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
		return nil
	}
	if err := s.store.DeleteRiskVerdict(ctx, webID); err != nil {
		return err
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

// freshVerdict returns a cached verdict when it is still authoritative.
func (s *Service) freshVerdict(ctx context.Context, webID uint64) (StoredVerdict, bool) {
	verdict, err := s.store.GetRiskVerdict(ctx, webID)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.logger.Warn("account_risk_read_failed", "account_id", webID, "error", err.Error())
		}
		return StoredVerdict{}, false
	}
	now := time.Now().UTC()
	switch verdict.Verdict {
	case VerdictDenied, VerdictFlagged:
		return verdict, true
	case VerdictClean:
		return verdict, now.Sub(verdict.CheckedAt) < s.cfg.PatrolInterval
	case VerdictError:
		return verdict, now.Sub(verdict.CheckedAt) < s.cfg.ErrorRetry
	}
	return StoredVerdict{}, false
}

// checkNow runs one bounded RSC check. Concurrent checks for the same web
// identity are merged singleflight-style: later callers wait for and share the
// first caller's result instead of erroring out (a hard error here used to
// strand the second degraded account's cooldown forever). A merged (shared)
// result is returned to every caller but persisted only once by the leader.
func (s *Service) checkNow(ctx context.Context, webID uint64) StoredVerdict {
	call := &checkCall{done: make(chan struct{})}
	if actual, loaded := s.checkInflight.LoadOrStore(webID, call); loaded {
		// 已有同身份检查在跑：等它完成并共享结果。等待不占并发闸。
		leader := actual.(*checkCall)
		s.waiters.Add(1)
		defer s.waiters.Add(-1)
		select {
		case <-leader.done:
			return storedFromCheck(leader.result)
		case <-ctx.Done():
			return StoredVerdict{Verdict: VerdictError, Error: "wait for in-flight check: " + ctx.Err().Error(), CheckedAt: time.Now().UTC()}
		}
	}
	defer func() {
		s.checkInflight.Delete(webID)
		close(call.done)
	}()
	select {
	case s.sem <- struct{}{}:
		defer func() { <-s.sem }()
	case <-ctx.Done():
		call.result = CheckResult{Verdict: VerdictError, Error: "concurrency gate timeout", CheckedAt: time.Now().UTC()}
		return StoredVerdict{Verdict: VerdictError, Error: call.result.Error, Source: "rsc", CheckedAt: call.result.CheckedAt}
	}
	token, err := s.accounts.DecryptedAccessToken(ctx, webID)
	if err != nil {
		verdict := StoredVerdict{Verdict: VerdictError, Error: "decrypt sso: " + err.Error(), Source: "rsc", CheckedAt: time.Now().UTC()}
		call.result = CheckResult{Verdict: VerdictError, Error: verdict.Error, CheckedAt: verdict.CheckedAt}
		s.saveVerdictGuarded(ctx, webID, verdict)
		return verdict
	}
	result := s.checker.Check(ctx, token)
	call.result = result
	verdict := storedFromCheck(result)
	if verdict.Verdict == "" {
		verdict.Verdict = VerdictError
	}
	s.saveVerdictGuarded(ctx, webID, verdict)
	perfmetrics.Default.Inc("account_rsc_check_total", perfmetrics.Labels{
		Subsystem: "account", Operation: "rsc_check", Outcome: verdict.Verdict,
	})
	// 日志与落库同一脱敏规则（rsc.RedactSecrets）：BotFlagDtl 是上游可控
	// 文本，此前仓储层落库前脱敏但日志打原始值——同一载荷两套口径。
	s.logger.Info("account_rsc_checked", "account_id", webID, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", rsc.RedactSecrets(verdict.BotFlagDtl))
	return verdict
}

// storedFromCheck projects a checker result onto the persisted verdict shape.
// The leader and the merged waiters must map identical fields: the previous
// hand-copied second block silently drifted on field additions.
func storedFromCheck(result CheckResult) StoredVerdict {
	return StoredVerdict{
		Verdict: result.Verdict, BotFlagSrc: result.BotFlagSource, BotFlagDtl: result.BotFlagDetails,
		RiskScore: result.RiskScore, HTTPStatus: result.HTTPStatus, Error: result.Error,
		Source: "rsc", CheckedAt: result.CheckedAt,
	}
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
	switch {
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
		switch s.cfg.OnDenied {
		case "flag":
			s.flagIdentity(ctx, webID, verdict)
		case "disable":
			s.disableIdentity(ctx, webID, verdict)
		default:
			s.logger.Warn("account_risk_marked", "account_id", webID, "verdict", verdict.Verdict, "details", verdict.BotFlagDtl)
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

// SetEgressQuarantiner installs the exit-IP quarantine bridge; nil keeps
// degrades account-scoped only.
func (s *Service) SetEgressQuarantiner(quarantiner EgressQuarantiner) {
	if s == nil || quarantiner == nil {
		return
	}
	s.egressQuarantiner = quarantiner
}

// flagIdentity marks the web identity and its linked Build accounts with the
// long-term risk flag (risk_status = rsc_denied). Unlike disableIdentity the
// accounts stay enabled: the flag reflects the true account state (usable but
// risk-routed) while permanently excluding them from scheduling until an
// operator clears it.
func (s *Service) flagIdentity(ctx context.Context, webID uint64, verdict StoredVerdict) {
	flagAccount := func(id uint64, webLinked uint64) {
		if err := s.accounts.SetAccountRiskStatus(ctx, id, true); err != nil {
			s.logger.Error("account_risk_flag_failed", "account_id", id, "error", err.Error())
			return
		}
		if webLinked != 0 {
			s.logger.Warn("account_risk_flagged", "account_id", id, "web_account_id", webLinked, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", verdict.BotFlagDtl)
			return
		}
		s.logger.Warn("account_risk_flagged", "account_id", id, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", verdict.BotFlagDtl)
	}
	flagAccount(webID, 0)
	builds, err := s.accounts.LinkedBuildAccountIDs(ctx, webID)
	if err != nil {
		// Build 查询失败不应阻断 Console 处置：两渠道独立收敛（对账兜底）。
		s.logger.Warn("account_risk_linked_lookup_failed", "account_id", webID, "channel", "build", "error", err.Error())
	}
	for _, buildID := range builds {
		flagAccount(buildID, webID)
	}
	consoles, err := s.accounts.LinkedConsoleAccountIDs(ctx, webID)
	if err != nil {
		s.logger.Warn("account_risk_linked_lookup_failed", "account_id", webID, "channel", "console", "error", err.Error())
		return
	}
	for _, consoleID := range consoles {
		flagAccount(consoleID, webID)
	}
}

func (s *Service) disableIdentity(ctx context.Context, webID uint64, verdict StoredVerdict) {
	const reason = "registration risk (RSC)"
	if err := s.accounts.SetAccountEnabled(ctx, webID, false, reason); err != nil {
		s.logger.Error("account_risk_disable_failed", "account_id", webID, "error", err.Error())
		return
	}
	s.logger.Warn("account_risk_disabled", "account_id", webID, "verdict", verdict.Verdict, "risk_score", verdict.RiskScore, "details", verdict.BotFlagDtl)
	builds, err := s.accounts.LinkedBuildAccountIDs(ctx, webID)
	if err != nil {
		s.logger.Warn("account_risk_linked_lookup_failed", "account_id", webID, "error", err.Error())
		return
	}
	for _, buildID := range builds {
		if err := s.accounts.SetAccountEnabled(ctx, buildID, false, reason); err != nil {
			s.logger.Error("account_risk_disable_failed", "account_id", buildID, "error", err.Error())
			continue
		}
		s.logger.Warn("account_risk_disabled", "account_id", buildID, "web_account_id", webID, "verdict", verdict.Verdict)
	}
	consoles, err := s.accounts.LinkedConsoleAccountIDs(ctx, webID)
	if err != nil {
		s.logger.Warn("account_risk_linked_lookup_failed", "account_id", webID, "channel", "console", "error", err.Error())
		return
	}
	for _, consoleID := range consoles {
		if err := s.accounts.SetAccountEnabled(ctx, consoleID, false, reason); err != nil {
			s.logger.Error("account_risk_disable_failed", "account_id", consoleID, "error", err.Error())
			continue
		}
		s.logger.Warn("account_risk_disabled", "account_id", consoleID, "web_account_id", webID, "verdict", verdict.Verdict)
	}
}

// PatrolTick re-checks clean and errored web identities whose cached verdict
// went stale, bounded by the concurrency gate.

// PatrolCutoffs returns the freshness bounds the wiring layer feeds into the
// patrol query.
func (s *Service) PatrolCutoffs() (patrolInterval, errorRetry time.Time) {
	now := time.Now().UTC()
	return now.Add(-s.cfg.PatrolInterval), now.Add(-s.cfg.ErrorRetry)
}

// ReconcileRiskyVerdicts converges risk_status flags with the verdict table at
// startup by replaying consequences for every risky identity. flagIdentity is
// an idempotent targeted write, so replaying converged identities is free while
// healing any drift in the group (web flag set but linked build missing, verdict
// recorded by an older binary whose consequence path was absent, crashed writes,
// config gaps). markOnly keeps logging only.
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
			s.applyConsequences(ctx, webID, webID, verdict, 0)
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
// clean→denied transition found by patrol must flag/disable the identity right
// away, not wait for the next request-path degrade event.
func (s *Service) PatrolTick(ctx context.Context, dueIDs []uint64) {
	if !s.Enabled() {
		return
	}
	for _, webID := range dueIDs {
		select {
		case <-ctx.Done():
			return
		default:
		}
		verdict := s.checkNow(ctx, webID)
		s.applyConsequences(ctx, webID, webID, verdict, 0)
	}
}
