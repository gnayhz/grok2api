package gateway

import (
	"context"
	"encoding/json"
	executionapp "github.com/chenyme/grok2api/backend/internal/application/execution"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"

	"io"
	"net/http"

	"sync"
	"testing"

	modelapp "github.com/chenyme/grok2api/backend/internal/application/model"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/domain/model"

	"github.com/chenyme/grok2api/backend/internal/port/provider"
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

// TestQualityProbeRequestExplicitEndpoint 锚定 I22(探针路径显式指向
// 正确端点):空 Path 会打到 base 根,cli-chat-proxy 对根路径恒 404,
// 探针将永远误判降智——历史事故。探针请求的 Path 必须恒为 /responses。
// 批8 追加锚定(校准):请求必须显式带 reasoning effort——不带参数的
// 微型请求,健康模型对简单问题直接出正文,守卫规则 3 将其误判降智,
// 探针永远测不出 clean(事故:全部探针 degraded、零 clean、裁决无法
// 落地,只能靠羁押期限兜底);输出预算必须容得下思考增量。
func TestQualityProbeRequestExplicitEndpoint(t *testing.T) {
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory()}
	request, err := service.qualityProbeRequestForContext(qualitymodel.WithProbeExperiment(context.Background(), qualitymodel.ProbeExperiment{Version: qualitymodel.ResourceCheckVersion, Sample: "token-short", Baseline: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.5", RuleVersion: "fictional"}}), model.Route{Provider: "grok_build", PublicID: "grok-4.5", UpstreamModel: "grok-4.5"}, account.Credential{Provider: account.ProviderBuild}, nil)
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
	service := &Service{physicalJournals: executionapp.NewPhysicalJournalFactory(), models: resolver}
	route, reason := service.qualityProbeRoute(qualitymodel.WithProbeExperiment(context.Background(), qualitymodel.NewProbeExperiment(qualitymodel.Observation{Attempt: attemptmeta.Identity{Provider: "grok_build", Model: "grok-4.5", RuleVersion: "fictional-rule"}})))
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
