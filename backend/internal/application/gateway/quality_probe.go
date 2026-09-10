package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	modeldomain "github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
)

// 质量取证探针的传输面:微型真实推理流 × 生产守卫信号分类。两个方向:
//   - 账号差分:案件已记录的生产降智是 baseline,只在指定 comparison
//     出口发起一次请求;comparison 必须是不同 IP 路径。
//   - 陪审员:健康他处账号钉扎被告出口单次探测(公理 B:多数降→出口
//     有罪方向,同时洗冤被告账号)。
// 探针只走 Build 面(I23:Web 与 Build 是不同风控面,探针不得跨面)。

// qualityProbePrompt 探针提示词:必须诱发可见思考——守卫规则 1 的
// clean 判据是"出现思考增量",而"回答数字 1"这类零思考问题在健康
// 模型上也会直接出正文(规则 3 误判降智),探针将永远测不出 clean
// (批8 事故:差分/陪审全 degraded、零 clean、裁决永远无法落地)。
const qualityProbePrompt = "Think step by step about 13+29, then reply with just the answer number."

// NodeExitIPResolver 出口取证面(差分路径比对):按地址族
// 返回活体解析的出口地址。WARP 类出口 IPv4 是共享 CGNAT、每出口身份
// 在 IPv6,对比必须按族进行,不得塌缩成单串。
type NodeExitIPResolver interface {
	NodeExitAddrs(ctx context.Context, nodeID uint64) (domainegress.ExitAddresses, error)
}

type nodeExitIPResolverValue struct{ NodeExitIPResolver }

// SetNodeExitIPResolver 安装出口地址解析器;nil 保持未设(差分
// 届时一律不可采,绝不据此定罪)。
func (s *Service) SetNodeExitIPResolver(resolver NodeExitIPResolver) {
	if resolver == nil {
		return
	}
	s.nodeExitIPResolver.Store(&nodeExitIPResolverValue{resolver})
}

// exitPathsDistinct 差分路径按族判等(I8 的本质:差分必须是不同路径):
// 任一共有族地址不同 → 不同路径;所有可比族都相同 → 同一路径;没有
// 可比族(一侧只 v4、另一侧只 v6)→ 可观测身份必然不同,算不同。
// 单侧某族未知时该族不参与判等(未知 ≠ 相同,也 ≠ 不同)——全部
// 可比族都相同即视为同路,宁可弃权不可误采。
func exitPathsDistinct(a, b domainegress.ExitAddresses) bool {
	comparable := false
	if a.IPv4 != "" && b.IPv4 != "" {
		comparable = true
		if a.IPv4 != b.IPv4 {
			return true
		}
	}
	if a.IPv6 != "" && b.IPv6 != "" {
		comparable = true
		if a.IPv6 != b.IPv6 {
			return true
		}
	}
	return !comparable
}

// ProbeAccountDifferentialOnPath 执行一条明确的账号差分路径:案件触发
// 时记录的生产观测是 baseline,本次探针只走 comparisonNodeID。对比
// 路径的实际出口仍需与 baseline 按地址族核实;节点字段相同或 IP 未知
// 时只能返回 error,不得产生降智票。
func (s *Service) ProbeAccountDifferentialOnPath(ctx context.Context, accountID, baselineNodeID, comparisonNodeID uint64) qualitymodel.ProbeMeasurement {
	return s.probeAccountDifferential(ctx, accountID, baselineNodeID, comparisonNodeID)
}

// probeAccountDifferential 是有限案件中单个 comparison 任务的执行器。
func (s *Service) probeAccountDifferential(ctx context.Context, accountID, baselineNodeID, comparisonNodeID uint64) qualitymodel.ProbeMeasurement {
	probeCtx, cancel := context.WithTimeout(ctx, qualityMeasurementTimeout)
	defer cancel()
	if baselineNodeID == 0 || comparisonNodeID == 0 {
		return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementError, Failure: qualitymodel.ProbeFailurePath, Reason: "explicit differential path is incomplete", Detail: "comparison-path-incomplete"}
	}
	request, release, failure := s.prepareQualityProbe(probeCtx, accountID)
	if failure.Outcome != "" {
		return failure
	}
	defer release()
	// 探针直接按 verdict 分类,不在任务内重试。
	hold := QualityRetryRuntime{CreatedTimeout: 10 * time.Second, EvidenceTimeout: 15 * time.Second}
	return s.probeAccountOnComparisonPath(probeCtx, request, hold, baselineNodeID, comparisonNodeID)
}

// probeAccountOnComparisonPath 是现行账号差分语义:案件建立时的生产
// 降智观测已经是 baseline,调查任务只需要把同一被告账号送到对比出口。
// 不再为了“复现 baseline”额外发一次请求——那次请求既重复证据,又会
// 因原始降智出口的排队/静默而阻塞整轮差分,正是仲裁庭反复出现全 error
// 的根因。comparison 的 clean/degraded/error 仍按原规则落账;baseline
// 只用于核验对比路径确实是不同出口。
func (s *Service) probeAccountOnComparisonPath(ctx context.Context, request provider.ResponseResourceRequest, hold QualityRetryRuntime, baselineNodeID, comparisonNodeID uint64) qualitymodel.ProbeMeasurement {
	return s.probeAccountComparison(ctx, request, hold, baselineNodeID, comparisonNodeID, s.qualityProbeMeasurement)
}

type qualityProbeAttemptFunc func(context.Context, provider.ResponseResourceRequest, QualityRetryRuntime) qualitymodel.ProbeMeasurement

// probeAccountComparison classifies exactly one probe on the comparison path.
// Keeping the attempt function injectable makes the one-request invariant
// testable without contacting an upstream provider.
func (s *Service) probeAccountComparison(ctx context.Context, request provider.ResponseResourceRequest, hold QualityRetryRuntime, baselineNodeID, comparisonNodeID uint64, attempt qualityProbeAttemptFunc) qualitymodel.ProbeMeasurement {
	comparisonCtx := infraegress.WithQualityVerificationNode(ctx, comparisonNodeID)
	traceCtx, trace := infraegress.WithTrace(comparisonCtx)
	result := attempt(traceCtx, request, hold)
	if result.Outcome == qualitymodel.MeasurementError {
		// Transport/provider failures do not require an observed egress
		// selection.  Preserve the stable cause even when the adapter failed
		// before it could acquire a lease; never replace it with a misleading
		// "comparison path unobserved" message.
		result.Detail = "comparison=error | cause=" + string(result.FailureCode())
		// A failure can support a repeated pattern only if the pinned path
		// was actually observed and proved independent. Keep the original
		// cause even when path verification is unavailable.
		if selected, observed := trace.Selection(domainegress.ScopeBuild); observed && selected.NodeID == comparisonNodeID {
			_, result.VerifiedIPChange = s.verifyExcludeRoutePathChange(ctx, baselineNodeID, trace)
			result.PathKey = s.qualityPathKey(ctx, comparisonNodeID)
		}
		return result
	}
	selected, observed := trace.Selection(domainegress.ScopeBuild)
	if !observed || selected.NodeID != comparisonNodeID {
		return qualitymodel.ProbeMeasurement{
			Outcome: qualitymodel.MeasurementError,
			Reason:  "explicit comparison path was not observed",
			Failure: qualitymodel.ProbeFailurePath, Attempt: result.Attempt,
			Detail: "comparison-path-unobserved",
		}
	}
	note, verified := s.verifyExcludeRoutePathChange(ctx, baselineNodeID, trace)
	if !verified {
		return qualitymodel.ProbeMeasurement{
			Outcome: qualitymodel.MeasurementError,
			Reason:  "explicit comparison path not verified distinct: " + note,
			Failure: qualitymodel.ProbeFailurePath, Attempt: result.Attempt,
			Detail: "comparison-path-not-distinct | " + note,
		}
	}
	switch result.Outcome {
	case qualitymodel.MeasurementClean:
		// Clean is useful as exculpatory evidence only on the dispatched
		// comparison path. The same path identity check is required for both
		// outcomes because the investigation contract explicitly asks for a
		// different IP, not merely a different node record.
		result.Detail, result.VerifiedIPChange, result.PathKey = "comparison=thinking", true, s.qualityPathKey(ctx, comparisonNodeID)
		return result
	case qualitymodel.MeasurementDegraded:
		return qualitymodel.ProbeMeasurement{
			Outcome:          qualitymodel.MeasurementDegraded,
			Attempt:          result.Attempt,
			Reason:           "no thinking evidence on comparison path",
			Detail:           "comparison=degraded " + note,
			VerifiedIPChange: true,
			PathKey:          s.qualityPathKey(ctx, comparisonNodeID),
		}
	default:
		return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementError, Failure: qualitymodel.ProbeFailureUnknown, Attempt: result.Attempt, Reason: "unknown comparison probe outcome", Detail: "comparison=unknown"}
	}
}

// verifyExcludeRoutePathChange 排除换路差分的路径核实:attempt2 的实际
// 出口节点(租约 trace)与 attempt1 基准节点逐一按族活体解析出口地址,
// 按族判等(任一共有族不同 → 真换路;所有可比族相同 → 同路)。直连
// 落点/无 trace/解析失败/两族皆未知一律不采(宁可弃权不可误采)。
// note 是稳定审计词汇,永不包含地址本身(I24)。
func (s *Service) verifyExcludeRoutePathChange(ctx context.Context, baseNode uint64, secondTrace *infraegress.Trace) (note string, verified bool) {
	selection, traced := secondTrace.Selection(domainegress.ScopeBuild)
	if !traced {
		return "no-second-attempt-trace", false
	}
	if selection.NodeID == 0 {
		return "second-path=direct", false
	}
	if selection.NodeID == baseNode {
		return "second-path=same-node", false
	}
	resolver := s.nodeExitIPResolver.Load()
	if resolver == nil {
		return "no-ip-resolver", false
	}
	base, baseErr := resolver.NodeExitAddrs(ctx, baseNode)
	second, secondErr := resolver.NodeExitAddrs(ctx, selection.NodeID)
	if baseErr != nil || secondErr != nil || !base.Resolved() || !second.Resolved() {
		return "exit-ip-unresolved", false
	}
	if !exitPathsDistinct(base, second) {
		return "same-exit-ip", false
	}
	return "exit-ip-verified:" + comparedFamilies(base, second), true
}

// ProbeExitJury 陪审员取证:健康陪审员账号钉扎被告出口单次探测
// (取证通道绕过资格;被告出口若脏,健康陪审员也应降智)。
func (s *Service) ProbeExitJury(ctx context.Context, jurorAccountID, defendantNodeID uint64) qualitymodel.ProbeMeasurement {
	probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	request, release, failure := s.prepareQualityProbe(probeCtx, jurorAccountID)
	if failure.Outcome != "" {
		return failure
	}
	defer release()
	juryCtx := probeCtx
	if defendantNodeID != 0 {
		juryCtx = infraegress.WithQualityVerificationNode(probeCtx, defendantNodeID)
	}
	traceCtx, trace := infraegress.WithTrace(juryCtx)
	result := s.qualityProbeMeasurement(traceCtx, request, QualityRetryRuntime{CreatedTimeout: 10 * time.Second, EvidenceTimeout: 15 * time.Second})
	if result.Outcome != qualitymodel.MeasurementError && defendantNodeID != 0 {
		selection, observed := trace.Selection(domainegress.ScopeBuild)
		if !observed || selection.NodeID != defendantNodeID {
			return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementError, Reason: "explicit jury path was not observed", Detail: "jury-path-unobserved", Failure: qualitymodel.ProbeFailurePath, Attempt: result.Attempt}
		}
	}
	result.Detail = "jury single-attempt"
	if selected, observed := trace.Selection(domainegress.ScopeBuild); observed && selected.NodeID == defendantNodeID {
		result.PathKey = s.qualityPathKey(probeCtx, defendantNodeID)
	}
	return result
}

func (s *Service) qualityPathKey(ctx context.Context, nodeID uint64) string {
	resolver := s.nodeExitIPResolver.Load()
	if resolver == nil {
		return ""
	}
	addrs, err := resolver.NodeExitAddrs(ctx, nodeID)
	if err != nil || !addrs.Resolved() {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(addrs.IPv4+"|"+addrs.IPv6)))
}

// comparedFamilies 列出两侧都解析出、且据此判等的地址族,只用于
// verified note 的稳定审计词汇(如 "ipv4+ipv6"),不含地址本身。
func comparedFamilies(a, b domainegress.ExitAddresses) string {
	families := make([]string, 0, 2)
	if a.IPv4 != "" && b.IPv4 != "" {
		families = append(families, "ipv4")
	}
	if a.IPv6 != "" && b.IPv6 != "" {
		families = append(families, "ipv6")
	}
	return strings.Join(families, "+")
}

// A probe reports clean only after admission and successful protocol completion.
// Degradation can be established early; transport and budget failures abstain.
func (s *Service) qualityProbeAttempt(ctx context.Context, request provider.ResponseResourceRequest, hold QualityRetryRuntime) (qualitymodel.MeasurementOutcome, string) {
	result := s.qualityProbeMeasurement(ctx, request, hold)
	return result.Outcome, result.Reason
}

func (s *Service) qualityProbeMeasurement(ctx context.Context, request provider.ResponseResourceRequest, limits QualityRetryRuntime) (result qualitymodel.ProbeMeasurement) {
	fail := func(kind qualitymodel.ProbeFailure, reason string) qualitymodel.ProbeMeasurement {
		result.Outcome, result.Failure, result.Reason = qualitymodel.MeasurementError, kind, reason
		return result
	}
	// Each physical measurement freezes the same policy authority as traffic.
	// Probes use shorter transport deadlines, without losing policy identity.
	hold, _ := s.requestGuardSnapshot()
	if hold.unavailable != nil {
		return fail(qualitymodel.ProbeFailurePolicy, "guard policy unavailable")
	}
	spec, frozen := qualitymodel.ProbeExperimentFromContext(ctx)
	if frozen && (spec.UnsupportedReason() != "" || hold.RuleVersion != spec.Baseline.RuleVersion || hold.Revision != spec.Baseline.Revision) {
		return fail(qualitymodel.ProbeFailureExperiment, "experiment policy changed or unavailable")
	}
	if limits.CreatedTimeout > 0 {
		hold.CreatedTimeout = limits.CreatedTimeout
	}
	if limits.EvidenceTimeout > 0 {
		hold.EvidenceTimeout = limits.EvidenceTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, qualityMeasurementTimeout)
	defer cancel()
	ctx = responsebuffer.WithContext(ctx, responsebuffer.NewLimitedRequest(8<<20))
	ctx, resources := newAttemptResources(ctx)
	defer resources.close()
	ctx = attemptmeta.WithRequest(ctx, newAuditEventID(), hold.Revision, hold.RuleVersion, hold.pathResolver)
	ctx = attemptmeta.WithAccount(ctx, request.Credential.ID, string(request.Credential.Provider), request.Model)
	if frozen {
		profile := spec.Baseline.Profile
		profile.Experiment, profile.Sample = spec.Version, spec.Sample
		ctx = attemptmeta.WithProfile(ctx, profile)
	}
	ctx = infraegress.WithPhysicalCallTrace(ctx, string(request.Credential.Provider), "responses")
	requestBudget := inferencedomain.NewAttemptBudget(1)
	defer requestBudget.Close()
	ctx = infraegress.WithPhysicalCallBudget(ctx, requestBudget)
	defer func() {
		resources.close()
		if result.Attempt.ID == "" {
			facts := infraegress.PhysicalFacts(ctx)
			if len(facts) > 0 {
				result.Attempt = facts[len(facts)-1].Attempt
			}
		}
		if err := s.recordPhysicalEvents(ctx); err != nil {
			result = fail(qualitymodel.ProbeFailurePersistence, "physical evidence persistence failed")
		}
	}()
	// One probe task means one physical generation. Its output never belongs
	// in a user conversation cache, even when the probe finishes successfully.
	request.DisableAutomaticReplay = true
	request.DeferOutputCommit = true
	response, err := s.runPhysicalAttempt(ctx, request, resources)
	if response != nil && response.DiscardOutput != nil {
		defer response.DiscardOutput()
	}
	if response != nil {
		result.Attempt = response.Attempt
		response.Body = resources.own(response.Body)
	}
	if err != nil {
		return fail(probeOperationFailure(ctx, err, qualitymodel.ProbeFailureForward), "forward: "+err.Error())
	}
	if response == nil || response.Body == nil {
		return fail(qualitymodel.ProbeFailureResponse, "empty response")
	}
	if frozen && !spec.Matches(result.Attempt) {
		return fail(qualitymodel.ProbeFailureExperiment, "normalized experiment mismatch")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		response.Body.Close()
		kind := qualitymodel.ProbeFailureHTTPRejected
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			kind = qualitymodel.ProbeFailureHTTPServer
		}
		return fail(kind, fmt.Sprintf("upstream HTTP %d", response.StatusCode))
	}
	if responseflow.FromReader(response.Body) == nil {
		response.Body = resources.own(responseflow.New(response.Body, responsebuffer.FromContext(ctx)))
	}
	replay, verdict, _, peekErr := peekQualityStream(ctx, response.Body, qualityProtocolResponses, hold)
	replay = resources.own(replay)
	switch {
	case peekErr != nil:
		// A probe can only produce quality evidence from a classified stream.
		// Empty streams and both no-data deadlines are transport/inconclusive
		// outcomes, not proof that the account or exit is degraded (I10). The
		// production request path may use these errors for its own bounded
		// retry/cooldown policy, but the investigation path must never promote
		// them to a jury vote.
		return fail(probeAdmissionFailure(ctx, peekErr), peekErr.Error())
	case verdict == QualityWithhold:
		result.Outcome, result.Reason = qualitymodel.MeasurementDegraded, "withheld: no thinking evidence"
		return result
	case verdict == QualityDeliver:
		if err := finishQualityProbe(ctx, replay, resources); err != nil {
			return fail(probeOperationFailure(ctx, err, qualitymodel.ProbeFailureCompletion), "completion: "+err.Error())
		}
		result.Outcome = qualitymodel.MeasurementClean
		return result
	default:
		// QualityWait 等不可判定形态:既非 clean 也非 degraded——error。
		return fail(qualitymodel.ProbeFailureAdmission, "inconclusive quality verdict: "+string(verdict))
	}
}

const qualityProbeCompletionBytes = 1 << 20
const qualityProbeCompletionTimeout = 15 * time.Second

func finishQualityProbe(ctx context.Context, body io.ReadCloser, resources *attemptResources) error {
	completionCtx, cancel := context.WithTimeout(ctx, qualityProbeCompletionTimeout)
	defer cancel()
	stop := context.AfterFunc(completionCtx, resources.close)
	defer stop()
	state := qualityScanState{protocol: qualityProtocolResponses}
	stream := responseflow.FromReader(body)
	if stream == nil {
		return errors.New("canonical probe stream unavailable")
	}
	err := stream.ConsumeLimit(qualityProbeCompletionBytes, func(event *responseflow.Event) error {
		if event.HasData && !bytes.Equal(bytes.TrimSpace(event.Data), []byte("[DONE]")) {
			observeQualityPayload(&state, event.Data)
		}
		return nil
	})
	if completionCtx.Err() != nil {
		return completionCtx.Err()
	}
	if err != nil {
		return err
	}
	if !state.completed || state.failed {
		return errors.New("successful response.completed not observed")
	}
	return nil
}

// qualityProbeRequest 构造微型探测请求。
// I22:Path 必须显式指向 /responses——空 Path 会打到 base 根,
// cli-chat-proxy 对 /v1/ 根路径恒 404,探针将永远误判降智(历史事故)。
func (s *Service) qualityProbeRequest(route modeldomain.Route, credential account.Credential, billing *account.Billing) (provider.ResponseResourceRequest, error) {
	body, err := json.Marshal(map[string]any{
		"model":             route.PublicID,
		"input":             qualityProbePrompt,
		"stream":            true,
		"max_output_tokens": 512,
		// 校准(批8):健康流量的形状=请求推理后先出思考增量。不带
		// reasoning 参数的微型请求,健康模型对简单问题直接出正文 →
		// 守卫规则 3(无思考出正文)误判降智 → 探针永远测不出 clean。
		// effort 取 low:足以产生思考增量,成本最低;模型不支持时
		// 由底座 normalizer 按既有语义处理。
		"reasoning": map[string]any{"effort": string(modeldomain.ReasoningEffortLow)},
	})
	if err != nil {
		return provider.ResponseResourceRequest{}, err
	}
	return provider.ResponseResourceRequest{
		Credential: credential, Billing: billing, Method: "POST", Model: route.UpstreamModel,
		Path: "/responses", Body: body, Streaming: true, NormalizeBody: true, Operation: "responses",
	}, nil
}

// qualityProbeOnBuildFace I23:质量取证探针只走 Build 面——Web 与
// Build 是不同风控面,Web 账号的 SSO 风控结论不得由 Build 探针跨面
// 定罪(反之亦然)。
func qualityProbeOnBuildFace(provider account.Provider) bool {
	return provider == account.ProviderBuild
}

// qualityProbeRoute 解析探针用的 Build 推理路由:第一个启用的
// Build 推理模型(I23:只解析 Build 面)。空 provider 表示未配置。
func (s *Service) qualityProbeRoute(ctx context.Context) (route modeldomain.Route, err error) {
	lister, ok := s.models.(interface {
		List(ctx context.Context, page, pageSize int, search string, filter modelapp.ListFilter) ([]modeldomain.Route, int64, error)
	})
	if !ok {
		return modeldomain.Route{}, errors.New("route enumeration unavailable")
	}
	spec, frozen := qualitymodel.ProbeExperimentFromContext(ctx)
	if frozen && spec.UnsupportedReason() != "" {
		return modeldomain.Route{}, errors.New(spec.UnsupportedReason())
	}
	const pageSize = 2000
	for page := 1; ; page++ {
		routes, total, err := lister.List(ctx, page, pageSize, "", modelapp.ListFilter{Provider: "grok_build", Status: "enabled"})
		if err != nil {
			return modeldomain.Route{}, fmt.Errorf("list build routes: %w", err)
		}
		for _, value := range routes {
			if frozen && string(value.Provider) != spec.Baseline.Provider {
				continue
			}
			if frozen && value.UpstreamModel != spec.Baseline.Model {
				continue
			}
			if modeldomain.SupportsReasoningForProvider(value.Provider, value.PublicID) || modeldomain.SupportsReasoningForProvider(value.Provider, value.UpstreamModel) {
				return value, nil
			}
		}
		if len(routes) == 0 || total <= int64(page*pageSize) || len(routes) < pageSize {
			break
		}
	}
	return modeldomain.Route{}, errors.New("no enabled reasoning build model")
}

// qualityProbeRequestForContext keeps a versioned synthetic sample and the
// original normalized effort. Unsupported profiles cannot silently downgrade.
func (s *Service) qualityProbeRequestForContext(ctx context.Context, route modeldomain.Route, credential account.Credential, billing *account.Billing) (provider.ResponseResourceRequest, error) {
	spec, ok := qualitymodel.ProbeExperimentFromContext(ctx)
	if !ok {
		return s.qualityProbeRequest(route, credential, billing)
	}
	if reason := spec.UnsupportedReason(); reason != "" {
		return provider.ResponseResourceRequest{}, errors.New(reason)
	}
	body := map[string]any{"model": route.PublicID, "input": spec.Prompt(), "stream": true, "max_output_tokens": 1024}
	if effort := spec.Baseline.Profile.ReasoningEffort; effort != "" {
		body["reasoning"] = map[string]any{"effort": effort}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return provider.ResponseResourceRequest{}, err
	}
	return provider.ResponseResourceRequest{Credential: credential, Billing: billing, Method: "POST", Model: route.UpstreamModel,
		Path: "/responses", Body: raw, Streaming: true, NormalizeBody: true, Operation: "responses"}, nil
}
