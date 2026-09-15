package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	qualitymodel "github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

type qualityProbeAttemptAdapter struct {
	body func() io.ReadCloser
}

func (a qualityProbeAttemptAdapter) Provider() account.Provider { return account.ProviderBuild }

func (a qualityProbeAttemptAdapter) ForwardResponse(context.Context, provider.ResponseResourceRequest) (*provider.Response, error) {
	return &provider.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     make(http.Header),
		Body:       a.body(),
	}, nil
}

// idleQualityProbeBody makes the probe wait for its own deadline and verifies
// that closing the read pump also unblocks the upstream reader.
type idleQualityProbeBody struct {
	released chan struct{}
	once     sync.Once
}

func newIdleQualityProbeBody() *idleQualityProbeBody {
	return &idleQualityProbeBody{released: make(chan struct{})}
}

func (b *idleQualityProbeBody) Read([]byte) (int, error) {
	<-b.released
	return 0, io.EOF
}

func (b *idleQualityProbeBody) Close() error {
	b.once.Do(func() { close(b.released) })
	return nil
}

// TestQualityProbeDoesNotPromoteTransportSilenceToDegraded 锚定 I10:
// 陪审/差分探针只有在流被分类为 degraded 时才能投降智票。空流、首事件
// 超时和零证据超时都没有质量信息，必须进入 error，不能被法院当作出口或
// 账号定罪证据。
func TestQualityProbeDoesNotPromoteTransportSilenceToDegraded(t *testing.T) {
	tests := []struct {
		name       string
		body       func() io.ReadCloser
		hold       QualityRetryRuntime
		reasonPart string
	}{
		{
			name:       "empty stream",
			body:       func() io.ReadCloser { return io.NopCloser(strings.NewReader("")) },
			reasonPart: errQualityEmptyStream.Error(),
		},
		{
			name:       "created timeout",
			body:       func() io.ReadCloser { return newIdleQualityProbeBody() },
			hold:       QualityRetryRuntime{CreatedTimeout: 20 * time.Millisecond, EvidenceTimeout: time.Second},
			reasonPart: errQualityCreatedTimeout.Error(),
		},
		{
			name: "evidence timeout",
			body: func() io.ReadCloser {
				idle := newIdleQualityProbeBody()
				return struct {
					io.Reader
					io.Closer
				}{io.MultiReader(strings.NewReader("data: {\"type\":\"response.created\"}\n\n"), idle), idle}
			},
			hold:       QualityRetryRuntime{CreatedTimeout: time.Second, EvidenceTimeout: 20 * time.Millisecond},
			reasonPart: errQualityEvidenceTimeout.Error(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := &Service{providers: provider.NewRegistry(qualityProbeAttemptAdapter{body: test.body})}
			outcome, reason := service.qualityProbeAttempt(context.Background(), provider.ResponseResourceRequest{
				Credential: account.Credential{Provider: account.ProviderBuild},
			}, test.hold)
			if outcome != qualitymodel.MeasurementError {
				t.Fatalf("transport silence outcome = %s, want error (reason=%q)", outcome, reason)
			}
			if !strings.Contains(reason, test.reasonPart) {
				t.Fatalf("reason = %q, want %q", reason, test.reasonPart)
			}
		})
	}
}

// TestQualityProbeRequestExplicitEndpoint 锚定 I22(探针路径显式指向
// 正确端点):空 Path 会打到 base 根,cli-chat-proxy 对根路径恒 404,
// 探针将永远误判降智——历史事故。探针请求的 Path 必须恒为 /responses。
// 批8 追加锚定(校准):请求必须显式带 reasoning effort——不带参数的
// 微型请求,健康模型对简单问题直接出正文,守卫规则 3 将其误判降智,
// 探针永远测不出 clean(事故:全部探针 degraded、零 clean、裁决无法
// 落地,只能靠羁押期限兜底);输出预算必须容得下思考增量。
func TestQualityProbeRequestExplicitEndpoint(t *testing.T) {
	service := &Service{}
	request, err := service.qualityProbeRequest(model.Route{Provider: "grok_build", PublicID: "grok-4.5", UpstreamModel: "grok-4.5"}, account.Credential{Provider: account.ProviderBuild}, nil)
	if err != nil {
		t.Fatalf("构造探针请求: %v", err)
	}
	if request.Path != "/responses" {
		t.Fatalf("I22:探针 Path 必须显式为 /responses, got %q", request.Path)
	}
	if request.Method != "POST" || !request.Streaming {
		t.Fatalf("探针必须是流式 POST: %+v", request)
	}
	var payload map[string]any
	if err := json.Unmarshal(request.Body, &payload); err != nil {
		t.Fatalf("解析探针请求体: %v", err)
	}
	reasoning, ok := payload["reasoning"].(map[string]any)
	if !ok || reasoning["effort"] != "low" {
		t.Fatalf("探针必须请求推理(否则健康流被误判降智): %+v", payload["reasoning"])
	}
	if tokens, _ := payload["max_output_tokens"].(float64); tokens < 128 {
		t.Fatalf("输出预算必须容得下思考增量, got %v", payload["max_output_tokens"])
	}
}

// TestQualityProbeBuildFaceOnly 锚定 I23(Web 与 Build 不同风控面):
// 质量取证探针只接受 Build 账号;Web/Console 账号一律拒绝——
// Build 探针的结论不得跨面定罪 Web 账号。
func TestQualityProbeBuildFaceOnly(t *testing.T) {
	if !qualityProbeOnBuildFace(account.ProviderBuild) {
		t.Fatal("Build 账号必须在探针面内")
	}
	for _, provider := range []account.Provider{account.ProviderWeb, account.ProviderConsole} {
		if qualityProbeOnBuildFace(provider) {
			t.Fatalf("I23:%s 账号不得进入 Build 探针面", provider)
		}
	}
}

// TestAccountDifferentialUsesKnownBaselineAndOneComparisonAttempt 锚定账号
// 差分的真实语义:触发事件已经提供 baseline,调查任务只请求 comparison。
// 如果再次请求 baseline,原始降智出口的排队/静默会把所有差分任务拖成
// created_timeout,即使对比出口本身可用也永远得不到账号证据。
func TestAccountDifferentialUsesKnownBaselineAndOneComparisonAttempt(t *testing.T) {
	service := &Service{}
	service.SetNodeExitIPResolver(stubNodeExitAddrResolver{addrs: map[uint64]domainegress.ExitAddresses{
		112: {IPv4: "198.51.100.112", IPv6: "2001:db8::112"},
		108: {IPv4: "198.51.100.108", IPv6: "2001:db8::108"},
	}})
	var attempts int
	result := service.probeAccountComparison(
		context.Background(), provider.ResponseResourceRequest{}, QualityRetryRuntime{}, 112, 108,
		func(ctx context.Context, _ provider.ResponseResourceRequest, _ QualityRetryRuntime) qualitymodel.ProbeMeasurement {
			attempts++
			trace := infraegress.TraceFromContext(ctx)
			if trace == nil {
				t.Fatal("comparison attempt must carry an egress trace")
			}
			trace.Record(infraegress.Selection{NodeID: 108, Scope: domainegress.ScopeBuild})
			return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementClean, Reason: ""}
		},
	)
	if attempts != 1 {
		t.Fatalf("account differential must make one comparison request, attempts=%d", attempts)
	}
	if result.Outcome != qualitymodel.MeasurementClean || result.Detail != "comparison=thinking" {
		t.Fatalf("comparison clean result=%+v", result)
	}
}

// TestAccountDifferentialComparisonDegradedNeedsDistinctPath 锚定差分降智
// 的可采条件:comparison 降智仍须证明它与已知 baseline 是不同出口；
// 不把 transport error 变成降智票。
func TestAccountDifferentialComparisonDegradedNeedsDistinctPath(t *testing.T) {
	service := &Service{}
	service.SetNodeExitIPResolver(stubNodeExitAddrResolver{addrs: map[uint64]domainegress.ExitAddresses{
		112: {IPv4: "198.51.100.112"},
		108: {IPv4: "198.51.100.108"},
	}})
	result := service.probeAccountComparison(
		context.Background(), provider.ResponseResourceRequest{}, QualityRetryRuntime{}, 112, 108,
		func(ctx context.Context, _ provider.ResponseResourceRequest, _ QualityRetryRuntime) qualitymodel.ProbeMeasurement {
			trace := infraegress.TraceFromContext(ctx)
			trace.Record(infraegress.Selection{NodeID: 108, Scope: domainegress.ScopeBuild})
			return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementDegraded, Reason: "no thinking evidence"}
		},
	)
	if result.Outcome != qualitymodel.MeasurementDegraded || !result.VerifiedIPChange {
		t.Fatalf("distinct comparison degraded result=%+v", result)
	}
}

// TestAccountDifferentialComparisonTransportStaysInconclusive 锚定 I10:
// 对比出口传输失败只能是 error,不能因为 baseline 已知降智就把 error
// 拼成一张假的账号降智票。
func TestAccountDifferentialComparisonTransportStaysInconclusive(t *testing.T) {
	service := &Service{}
	var attempts int
	result := service.probeAccountComparison(
		context.Background(), provider.ResponseResourceRequest{}, QualityRetryRuntime{}, 112, 108,
		func(ctx context.Context, _ provider.ResponseResourceRequest, _ QualityRetryRuntime) qualitymodel.ProbeMeasurement {
			attempts++
			trace := infraegress.TraceFromContext(ctx)
			trace.Record(infraegress.Selection{NodeID: 108, Scope: domainegress.ScopeBuild})
			return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementError, Failure: qualitymodel.ProbeFailureHTTPServer, Reason: "upstream HTTP 503"}
		},
	)
	if attempts != 1 || result.Outcome != qualitymodel.MeasurementError || result.VerifiedIPChange {
		t.Fatalf("transport error must remain inconclusive, attempts=%d result=%+v", attempts, result)
	}
	if result.Detail != "comparison=error | cause=upstream/headers/server_error" {
		t.Fatalf("transport error detail=%q", result.Detail)
	}
}

func TestAccountDifferentialComparisonCleanNeedsDistinctPath(t *testing.T) {
	service := &Service{}
	service.SetNodeExitIPResolver(stubNodeExitAddrResolver{addrs: map[uint64]domainegress.ExitAddresses{
		112: {IPv4: "same"},
		108: {IPv4: "same"},
	}})
	result := service.probeAccountComparison(
		context.Background(), provider.ResponseResourceRequest{}, QualityRetryRuntime{}, 112, 108,
		func(ctx context.Context, _ provider.ResponseResourceRequest, _ QualityRetryRuntime) qualitymodel.ProbeMeasurement {
			trace := infraegress.TraceFromContext(ctx)
			trace.Record(infraegress.Selection{NodeID: 108, Scope: domainegress.ScopeBuild})
			return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementClean, Reason: ""}
		},
	)
	if result.Outcome != qualitymodel.MeasurementError || result.VerifiedIPChange {
		t.Fatalf("same IP must invalidate even a clean comparison result: %+v", result)
	}
	if !strings.Contains(result.Detail, "same-exit-ip") {
		t.Fatalf("same IP failure should remain diagnosable: %+v", result)
	}
}

type pagedQualityProbeRouteResolver struct {
	routes []model.Route
	pages  []int
}

func (r *pagedQualityProbeRouteResolver) Get(context.Context, uint64) (model.Route, error) {
	return model.Route{}, repository.ErrNotFound
}

func (r *pagedQualityProbeRouteResolver) GetByPublicID(context.Context, string) (model.Route, error) {
	return model.Route{}, repository.ErrNotFound
}

func (r *pagedQualityProbeRouteResolver) GetByPublicIDCandidates(context.Context, string) ([]model.Route, error) {
	return nil, repository.ErrNotFound
}

func (r *pagedQualityProbeRouteResolver) GetByProviderUpstream(context.Context, account.Provider, string) (model.Route, error) {
	return model.Route{}, repository.ErrNotFound
}

func (r *pagedQualityProbeRouteResolver) HasEnabledRouteByPublicID(context.Context, string) (bool, error) {
	return false, nil
}

func (r *pagedQualityProbeRouteResolver) List(_ context.Context, page, pageSize int, _ string, _ modelapp.ListFilter) ([]model.Route, int64, error) {
	r.pages = append(r.pages, page)
	start := (page - 1) * pageSize
	if start >= len(r.routes) {
		return nil, int64(len(r.routes)), nil
	}
	end := min(start+pageSize, len(r.routes))
	return r.routes[start:end], int64(len(r.routes)), nil
}

// TestQualityProbeRouteScansAllPages 锚定调查局路由发现:模型目录超过
// 单页上限时,探针仍必须找到后续页面的 Build 推理路由,不能因前页是
// 媒体/非推理模型而把调查局报告为未配置。
func TestQualityProbeRouteScansAllPages(t *testing.T) {
	routes := make([]model.Route, 2001)
	for i := range routes {
		routes[i] = model.Route{ID: uint64(i + 1), Provider: account.ProviderBuild, PublicID: "grok-3"}
	}
	routes[len(routes)-1] = model.Route{ID: 2001, Provider: account.ProviderBuild, PublicID: "grok-4.5", UpstreamModel: "grok-4.5"}
	resolver := &pagedQualityProbeRouteResolver{routes: routes}
	service := &Service{models: resolver}
	route, reason := service.qualityProbeRoute(context.Background())
	if reason != nil || route.PublicID != "grok-4.5" {
		t.Fatalf("跨页探针路由解析失败: route=%+v reason=%q", route, reason)
	}
	if len(resolver.pages) != 2 || resolver.pages[0] != 1 || resolver.pages[1] != 2 {
		t.Fatalf("应按页扫描模型目录, pages=%v", resolver.pages)
	}
}

// stubNodeExitAddrResolver 差分验证测试的按族出口地址解析桩。
type stubNodeExitAddrResolver struct {
	addrs map[uint64]domainegress.ExitAddresses
	err   error
}

func (s stubNodeExitAddrResolver) NodeExitAddrs(_ context.Context, nodeID uint64) (domainegress.ExitAddresses, error) {
	if s.err != nil {
		return domainegress.ExitAddresses{}, s.err
	}
	return s.addrs[nodeID], nil
}

// TestExitPathsDistinctPerFamily 锚定按地址族的差分路径判等:出口对比
// 不得塌缩成单串(v4 优先合成)。WARP 类出口 IPv4 是共享 CGNAT、每出口
// 身份在 IPv6——v4 相同而 v6 不同是不同路径;只有可比族全部相同才算
// 同路;没有可比族(一侧只 v4、另一侧只 v6)可观测身份必然不同。
func TestExitPathsDistinctPerFamily(t *testing.T) {
	cases := []struct {
		name string
		a, b domainegress.ExitAddresses
		want bool
	}{
		{"warp v4 same v6 differ", domainegress.ExitAddresses{IPv4: "198.51.100.10", IPv6: "2001:db8::a"}, domainegress.ExitAddresses{IPv4: "198.51.100.10", IPv6: "2001:db8::b"}, true},
		{"twin both families same", domainegress.ExitAddresses{IPv4: "198.51.100.10", IPv6: "2001:db8::a"}, domainegress.ExitAddresses{IPv4: "198.51.100.10", IPv6: "2001:db8::a"}, false},
		{"v4 only same", domainegress.ExitAddresses{IPv4: "198.51.100.10"}, domainegress.ExitAddresses{IPv4: "198.51.100.10"}, false},
		{"v4 only differ", domainegress.ExitAddresses{IPv4: "198.51.100.10"}, domainegress.ExitAddresses{IPv4: "198.51.100.11"}, true},
		{"v6 only same", domainegress.ExitAddresses{IPv6: "2001:db8::a"}, domainegress.ExitAddresses{IPv6: "2001:db8::a"}, false},
		{"v6 only differ", domainegress.ExitAddresses{IPv6: "2001:db8::a"}, domainegress.ExitAddresses{IPv6: "2001:db8::b"}, true},
		{"no common family", domainegress.ExitAddresses{IPv4: "198.51.100.10"}, domainegress.ExitAddresses{IPv6: "2001:db8::b"}, true},
		{"v6 incomparable v4 same fails closed", domainegress.ExitAddresses{IPv4: "198.51.100.10", IPv6: "2001:db8::a"}, domainegress.ExitAddresses{IPv4: "198.51.100.10"}, false},
	}
	for _, tc := range cases {
		if got := exitPathsDistinct(tc.a, tc.b); got != tc.want {
			t.Fatalf("%s: exitPathsDistinct(%+v, %+v) = %v, want %v", tc.name, tc.a, tc.b, got, tc.want)
		}
	}
}

// TestVerifyExcludeRoutePathFamilyAware 排除换路差分的事后核实必须按族:
// WARP 兄弟(v4 相同、v6 不同)是可采差分;真孪生仍不可采。
func TestVerifyExcludeRoutePathFamilyAware(t *testing.T) {
	service := &Service{}
	service.SetNodeExitIPResolver(stubNodeExitAddrResolver{addrs: map[uint64]domainegress.ExitAddresses{
		116: {IPv4: "198.51.100.10", IPv6: "2001:db8::116"},
		115: {IPv4: "198.51.100.10", IPv6: "2001:db8::115"},
		117: {IPv4: "198.51.100.10", IPv6: "2001:db8::116"},
	}})
	_, secondTrace := infraegress.WithTrace(context.Background())
	secondTrace.Record(infraegress.Selection{NodeID: 115, Scope: domainegress.ScopeBuild})

	note, verified := service.verifyExcludeRoutePathChange(context.Background(), 116, secondTrace)
	if !verified {
		t.Fatalf("WARP 兄弟(v4 同、v6 不同)必须是可采差分: note=%s", note)
	}
	if strings.Contains(note, "198.51.100.10") || strings.Contains(note, "2001:db8") {
		t.Fatalf("核实 note 不得携带出口地址(I24): %s", note)
	}

	twinTrace := func() *infraegress.Trace {
		_, trace := infraegress.WithTrace(context.Background())
		trace.Record(infraegress.Selection{NodeID: 117, Scope: domainegress.ScopeBuild})
		return trace
	}()
	if note, verified := service.verifyExcludeRoutePathChange(context.Background(), 116, twinTrace); verified {
		t.Fatalf("真孪生(两族全同)不可采: note=%s", note)
	}
}

func TestComparisonTransportPersistsObservedIndependentPath(t *testing.T) {
	service := &Service{}
	service.SetNodeExitIPResolver(stubNodeExitAddrResolver{addrs: map[uint64]domainegress.ExitAddresses{1: {IPv4: "198.51.100.1"}, 2: {IPv4: "198.51.100.2"}}})
	result := service.probeAccountComparison(context.Background(), provider.ResponseResourceRequest{}, QualityRetryRuntime{}, 1, 2,
		func(ctx context.Context, _ provider.ResponseResourceRequest, _ QualityRetryRuntime) qualitymodel.ProbeMeasurement {
			infraegress.TraceFromContext(ctx).Record(infraegress.Selection{NodeID: 2, Scope: domainegress.ScopeBuild})
			return qualitymodel.ProbeMeasurement{Outcome: qualitymodel.MeasurementError, Reason: "created timeout"}
		})
	if result.Outcome != qualitymodel.MeasurementError || !result.VerifiedIPChange || result.PathKey == "" {
		t.Fatalf("response failure lost its verified path: %+v", result)
	}
	if strings.Contains(result.PathKey, "198.51") {
		t.Fatal("path fingerprint leaked an IP")
	}
}
