package egress

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	egressdomain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/repository"
	"github.com/gin-gonic/gin"
)

type proxyRevealRepository struct {
	node egressdomain.Node
}

func (r *proxyRevealRepository) ListEgressNodes(context.Context, repository.SortQuery) ([]egressdomain.Node, error) {
	return []egressdomain.Node{r.node}, nil
}
func (r *proxyRevealRepository) ListEgressNodePage(context.Context, repository.EgressNodeListQuery) ([]egressdomain.Node, int64, error) {
	return []egressdomain.Node{r.node}, 1, nil
}
func (r *proxyRevealRepository) GetEgressNode(_ context.Context, id uint64) (egressdomain.Node, error) {
	if id != r.node.ID {
		return egressdomain.Node{}, repository.ErrNotFound
	}
	return r.node, nil
}
func (r *proxyRevealRepository) CreateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	return value, nil
}
func (r *proxyRevealRepository) UpdateEgressNode(_ context.Context, value egressdomain.Node) (egressdomain.Node, error) {
	return value, nil
}
func (r *proxyRevealRepository) DeleteEgressNode(context.Context, uint64) error { return nil }

func TestProxyURLRevealIsExplicitAndNeverCacheable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "socks5h://user:secret@proxy.example:1080"
	encrypted, err := cipher.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(&proxyRevealRepository{node: egressdomain.Node{ID: 7, EncryptedProxyURL: encrypted}}, cipher)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: "7"}}
	context.Request = httptest.NewRequest("POST", "/egress-nodes/7/proxy-url/reveal", nil)
	NewHandler(service).proxyURL(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" || recorder.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("reveal cache headers = %#v", recorder.Header())
	}
}

func TestBatchNodeUpdateRequestRequiresEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
		want    bool
	}{
		{name: "missing", body: `{"ids":["1"]}`, wantErr: true},
		{name: "explicit false", body: `{"ids":["1"],"enabled":false}`, want: false},
		{name: "explicit true", body: `{"ids":["1"],"enabled":true}`, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			context, _ := gin.CreateTestContext(httptest.NewRecorder())
			context.Request = httptest.NewRequest("PATCH", "/egress-nodes/batch", bytes.NewBufferString(test.body))
			context.Request.Header.Set("Content-Type", "application/json")
			var request batchNodeUpdateRequest
			err := context.ShouldBindJSON(&request)
			if test.wantErr {
				if err == nil {
					t.Fatal("expected binding error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if request.Enabled == nil || *request.Enabled != test.want {
				t.Fatalf("enabled = %v, want %v", request.Enabled, test.want)
			}
		})
	}
}

func TestUpdateManyRejectsMissingEnabled(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("PATCH", "/egress-nodes/batch", bytes.NewBufferString(`{"ids":["1"]}`))
	context.Request.Header.Set("Content-Type", "application/json")

	(&Handler{}).updateMany(context)

	if recorder.Code != 400 || !strings.Contains(recorder.Body.String(), "invalidRequest") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLegacyEgressSourceListRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		path string
		want bool
	}{
		{path: "/egress-sources", want: true},
		{path: "/egress-sources?page=1", want: false},
		{path: "/egress-sources?pageSize=100", want: false},
		{path: "/egress-sources?search=alpha", want: false},
		{path: "/egress-sources?poolId=3", want: true},
	} {
		context, _ := gin.CreateTestContext(httptest.NewRecorder())
		context.Request = httptest.NewRequest("GET", test.path, nil)
		if got := legacyEgressSourceListRequest(context); got != test.want {
			t.Fatalf("legacyEgressSourceListRequest(%q) = %v, want %v", test.path, got, test.want)
		}
	}
}

func TestParseBoundedEgressNodeIDsChecksRawInputLength(t *testing.T) {
	values := make([]string, 5001)
	for index := range values {
		values[index] = "1"
	}
	if _, err := parseBoundedEgressNodeIDs(values, 5000); err == nil || !strings.Contains(err.Error(), "count") {
		t.Fatalf("oversized duplicate input error = %v", err)
	}
	ids, err := parseBoundedEgressNodeIDs([]string{"2", "2", "1"}, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != 2 || ids[1] != 1 {
		t.Fatalf("ids = %v", ids)
	}
}

func TestNewNodeResponseIncludesIPv4AndIPv6ProbeDetails(t *testing.T) {
	testedAt := time.Now().UTC().Truncate(time.Second)
	response := newNodeResponse(egressdomain.PublicNode{
		ProbeStatus:   egressdomain.ProbeStatusHealthy,
		ProbeProvider: egressdomain.ProbeProviderCloudflare,
		IPv4Probe: egressdomain.ProbeFamilyResult{
			Status: egressdomain.ProbeStatusHealthy, TestedAt: testedAt, LatencyMS: 21, ExitIP: "198.51.100.2",
		},
		IPv6Probe: egressdomain.ProbeFamilyResult{
			Status: egressdomain.ProbeStatusUnhealthy, TestedAt: testedAt, LatencyMS: 48, Error: "代理连接失败",
		},
	})
	if response.ProbeProvider != "cloudflare" || response.IPv4Probe.ExitIP != "198.51.100.2" || response.IPv4Probe.TestedAt == nil || response.IPv6Probe.Status != "unhealthy" || response.IPv6Probe.Error == "" {
		t.Fatalf("node response = %#v", response)
	}
}

func TestOperationsConfigRequestParsesRoutingTargets(t *testing.T) {
	input, err := (operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900,
		DefaultTarget: &routingTargetRequest{Mode: "node", NodeID: "42"},
		ScopeTargets: map[string]routingTargetRequest{
			"grok_web": {Mode: "direct"},
		},
		ClassTargets: map[string]routingTargetRequest{
			"inference": {Mode: "pool", PoolID: "7"},
		},
	}).input()
	if err != nil {
		t.Fatal(err)
	}
	if input.DefaultTarget == nil || input.DefaultTarget.Mode != egressdomain.RoutingTargetNode || input.DefaultTarget.NodeID != 42 {
		t.Fatalf("default target = %#v", input.DefaultTarget)
	}
	if target := input.ScopeTargets[egressdomain.ScopeWeb]; target.Mode != egressdomain.RoutingTargetDirect || target.NodeID != 0 {
		t.Fatalf("web scope target = %#v", target)
	}
	if target := input.ClassTargets[egressdomain.TrafficClassInference]; target.Mode != egressdomain.RoutingTargetPool || target.PoolID != 7 {
		t.Fatalf("inference class target = %#v", target)
	}
	if input.ProbeProvider != egressdomain.ProbeProviderCloudflare {
		t.Fatalf("probe provider = %q", input.ProbeProvider)
	}
}

func TestOperationsConfigRequestDefaultsEmptyModeToAuto(t *testing.T) {
	input, err := (operationsConfigRequest{
		DefaultTarget: &routingTargetRequest{NodeID: "1"},
	}).input()
	if err != nil {
		t.Fatal(err)
	}
	if input.DefaultTarget.Mode != egressdomain.RoutingTargetAuto {
		t.Fatalf("default target mode = %q, want auto", input.DefaultTarget.Mode)
	}
}

func TestOperationsConfigRequestRejectsInvalidTargetIDs(t *testing.T) {
	for name, request := range map[string]operationsConfigRequest{
		"invalid node id":      {DefaultTarget: &routingTargetRequest{Mode: "node", NodeID: "zero"}},
		"invalid pool id":      {DefaultTarget: &routingTargetRequest{Mode: "pool", PoolID: "0"}},
		"invalid mode":         {DefaultTarget: &routingTargetRequest{Mode: "rendezvous"}},
		"invalid scope target": {ScopeTargets: map[string]routingTargetRequest{"grok_web": {Mode: "node", NodeID: "x"}}},
		"invalid class target": {ClassTargets: map[string]routingTargetRequest{"inference": {Mode: "direct", PoolID: "x"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := request.input(); !errors.Is(err, egressapp.ErrInvalidInput) {
				t.Fatalf("error = %v, want ErrInvalidInput", err)
			}
		})
	}
}

func TestNewOperationsConfigResponseIncludesAllTargetLevels(t *testing.T) {
	response := newOperationsConfigResponse(egressdomain.OperationsConfig{
		ProbeProvider:        egressdomain.ProbeProviderCloudflare,
		ProbeIntervalSeconds: 900,
		DefaultTarget:        egressdomain.RoutingTarget{Mode: egressdomain.RoutingTargetNode, NodeID: 42},
		ScopeTargets:         map[egressdomain.Scope]egressdomain.RoutingTarget{egressdomain.ScopeWeb: {Mode: egressdomain.RoutingTargetDirect}},
		ClassTargets:         map[egressdomain.TrafficClass]egressdomain.RoutingTarget{egressdomain.TrafficClassInference: {Mode: egressdomain.RoutingTargetPool, PoolID: 7}},
	})
	if response.DefaultTarget.Mode != "node" || response.DefaultTarget.NodeID != "42" {
		t.Fatalf("default target = %#v", response.DefaultTarget)
	}
	if target := response.ScopeTargets["grok_web"]; target.Mode != "direct" || target.NodeID != "" {
		t.Fatalf("web scope target = %#v", target)
	}
	if target := response.ClassTargets["inference"]; target.Mode != "pool" || target.PoolID != "7" {
		t.Fatalf("inference class target = %#v", target)
	}
	if response.ScopeTargets == nil || response.ClassTargets == nil {
		t.Fatal("scope/class maps must serialize as maps, not null")
	}
}

type poolMembersStubRepo struct {
	egressapp.ServiceRepository
}

func (r *poolMembersStubRepo) ListEgressPools(_ context.Context) ([]egressdomain.Pool, error) {
	return nil, nil
}

func (r *poolMembersStubRepo) GetEgressPool(_ context.Context, _ uint64) (egressdomain.Pool, error) {
	return egressdomain.Pool{ID: 1, Enabled: true}, nil
}

func (r *poolMembersStubRepo) CreateEgressPool(_ context.Context, value egressdomain.Pool) (egressdomain.Pool, error) {
	return value, nil
}

func (r *poolMembersStubRepo) UpdateEgressPool(_ context.Context, value egressdomain.Pool) (egressdomain.Pool, error) {
	return value, nil
}

func (r *poolMembersStubRepo) DeleteEgressPool(_ context.Context, _ uint64) error { return nil }

func (r *poolMembersStubRepo) EgressPoolMembers(_ context.Context) (map[uint64][]uint64, error) {
	return nil, nil
}

func (r *poolMembersStubRepo) SetEgressPoolMemberPriority(_ context.Context, _, _ uint64, _ int64) error {
	return nil
}

func (r *poolMembersStubRepo) ListEgressNodesByPool(_ context.Context, _ uint64) ([]egressdomain.Node, error) {
	return nil, nil
}

func (r *poolMembersStubRepo) ListEgressNodes(_ context.Context, _ repository.SortQuery) ([]egressdomain.Node, error) {
	return []egressdomain.Node{{ID: 1, Name: "one"}, {ID: 2, Name: "two"}}, nil
}

func (r *poolMembersStubRepo) SetEgressPoolMembers(_ context.Context, _ uint64, _ []uint64) error {
	return nil
}

// 成员设置载荷必须显式携带 nodeIds/ids:两个键都缺位(例如误用响应字段
// memberIds)曾被解释为"清空全部成员"并返回 updated=true——静默销毁性
// 默认值。显式空数组仍是合法的全清空。
func TestSetPoolMembersRequiresExplicitIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{name: "response field misuse", body: `{"memberIds":["1","2"]}`, wantErr: true},
		{name: "empty object", body: `{}`, wantErr: true},
		{name: "explicit clear", body: `{"ids":[]}`, wantErr: false},
		{name: "legacy ids", body: `{"ids":["1"]}`, wantErr: false},
		{name: "nodeIds", body: `{"nodeIds":["1","2"]}`, wantErr: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest("PUT", "/egress-pools/1/members", bytes.NewBufferString(test.body))
			context.Request.Header.Set("Content-Type", "application/json")
			context.Params = gin.Params{{Key: "id", Value: "1"}}
			handler := NewHandler(egressapp.NewService(&poolMembersStubRepo{}, nil))
			handler.setPoolMembers(context)
			if test.wantErr {
				if context.Writer.Status() != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400 (body %s)", context.Writer.Status(), test.body)
				}
				return
			}
			if context.Writer.Status() != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", context.Writer.Status(), test.body)
			}
		})
	}
}
type missingPoolStubRepo struct {
	egressapp.ServiceRepository
}

func (r *missingPoolStubRepo) ListEgressPools(_ context.Context) ([]egressdomain.Pool, error) {
	return nil, nil
}

func (r *missingPoolStubRepo) GetEgressPool(_ context.Context, _ uint64) (egressdomain.Pool, error) {
	return egressdomain.Pool{}, repository.ErrNotFound
}

func (r *missingPoolStubRepo) CreateEgressPool(_ context.Context, value egressdomain.Pool) (egressdomain.Pool, error) {
	return value, nil
}

func (r *missingPoolStubRepo) UpdateEgressPool(_ context.Context, value egressdomain.Pool) (egressdomain.Pool, error) {
	return value, nil
}

func (r *missingPoolStubRepo) DeleteEgressPool(_ context.Context, _ uint64) error { return nil }

func (r *missingPoolStubRepo) EgressPoolMembers(_ context.Context) (map[uint64][]uint64, error) {
	return nil, nil
}

func (r *missingPoolStubRepo) SetEgressPoolMemberPriority(_ context.Context, _, _ uint64, _ int64) error {
	return nil
}

func (r *missingPoolStubRepo) ListEgressNodesByPool(_ context.Context, _ uint64) ([]egressdomain.Node, error) {
	return nil, nil
}

func (r *missingPoolStubRepo) SetEgressPoolMembers(_ context.Context, _ uint64, _ []uint64) error {
	return nil
}

// 不存在的池设置成员曾把 repository.ErrNotFound 透传到 writeError 的
// default 分支返回 500;必须归一为应用层 ErrNotFound(404)。
func TestSetPoolMembersMissingPoolMapsTo404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("PUT", "/egress-pools/999/members", bytes.NewBufferString(`{"ids":["1"]}`))
	context.Request.Header.Set("Content-Type", "application/json")
	context.Params = gin.Params{{Key: "id", Value: "999"}}
	handler := NewHandler(egressapp.NewService(&missingPoolStubRepo{}, nil))
	handler.setPoolMembers(context)
	if context.Writer.Status() != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (missing pool must not surface as 500)", context.Writer.Status())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "egressNodeNotFound") {
		t.Fatalf("body = %s, want egressNodeNotFound code", body)
	}
}

// fallbackMode=pool 而未指定 fallbackPoolId 曾绕过应用层校验、由数据库
// CHECK 兜底失败后返回 500;必须在校验层以 400 拒绝。
func TestCreatePoolFallbackModePoolWithoutIDRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/egress-pools", bytes.NewBufferString(`{"name":"x","strategy":"random","fallbackMode":"pool"}`))
	context.Request.Header.Set("Content-Type", "application/json")
	handler := NewHandler(egressapp.NewService(&poolMembersStubRepo{}, nil))
	handler.createPool(context)
	if context.Writer.Status() != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (pool fallback without pool id must be rejected by validation, body %s)", context.Writer.Status(), recorder.Body.String())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "invalidEgressNode") {
		t.Fatalf("body = %s, want invalidEgressNode code", body)
	}
}
// RotateNode 对不存在的节点曾把 repository.ErrNotFound 透传到 writeError
// 的 default 分支返回 500;必须与其他节点路由一致地归一为 404。
func TestRotateNodeMissingNodeMapsTo404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	// rotateNotFoundRepo 内嵌 proxyRevealRepository 并覆盖 GetEgressNode:
	// 节点 999 不存在。
	repoNotFound := &rotateNotFoundRepo{}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("POST", "/egress-nodes/999/rotate", nil)
	context.Params = gin.Params{{Key: "id", Value: "999"}}
	handler := NewHandler(egressapp.NewService(repoNotFound, cipher))
	handler.rotateNode(context)
	if context.Writer.Status() != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (missing node must not surface as 500)", context.Writer.Status())
	}
	if body := recorder.Body.String(); !strings.Contains(body, "egressNodeNotFound") {
		t.Fatalf("body = %s, want egressNodeNotFound code", body)
	}
}

type rotateNotFoundRepo struct {
	proxyRevealRepository
}

func (r *rotateNotFoundRepo) GetEgressNode(_ context.Context, _ uint64) (egressdomain.Node, error) {
	return egressdomain.Node{}, repository.ErrNotFound
}
