package egress

import (
	"bytes"
	"context"
	"errors"
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
	node        egressdomain.Node
	profile     egressdomain.ProxyProfile
	profilePage repository.PageQuery
}

func (r *proxyRevealRepository) ListEgressNodes(context.Context, egressdomain.Scope, repository.SortQuery) ([]egressdomain.Node, error) {
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
func (r *proxyRevealRepository) ListEgressProxyProfiles(_ context.Context, page repository.PageQuery) ([]egressdomain.ProxyProfile, int64, error) {
	r.profilePage = page
	return []egressdomain.ProxyProfile{r.profile}, 1, nil
}
func (r *proxyRevealRepository) GetEgressProxyProfile(_ context.Context, id uint64) (egressdomain.ProxyProfile, error) {
	if id != r.profile.ID {
		return egressdomain.ProxyProfile{}, repository.ErrNotFound
	}
	return r.profile, nil
}
func (r *proxyRevealRepository) CreateEgressProxyProfile(_ context.Context, value egressdomain.ProxyProfile) (egressdomain.ProxyProfile, error) {
	return value, nil
}
func (r *proxyRevealRepository) UpdateEgressProxyProfile(_ context.Context, value egressdomain.ProxyProfile, _ bool) (egressdomain.ProxyProfile, []uint64, error) {
	return value, nil, nil
}
func (r *proxyRevealRepository) DeleteEgressProxyProfile(context.Context, uint64) error { return nil }

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
	service := egressapp.NewService(&proxyRevealRepository{node: egressdomain.Node{ID: 7, EncryptedProxyURL: encrypted}}, cipher, "")
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

func TestProxyProfileURLRevealIsExplicitAndNeverCacheable(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := cipher.Encrypt("http://profile-user:profile-secret@proxy.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	service := egressapp.NewService(&proxyRevealRepository{profile: egressdomain.ProxyProfile{ID: 9, EncryptedProxyURL: encrypted}}, cipher, "")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Params = gin.Params{{Key: "id", Value: "9"}}
	context.Request = httptest.NewRequest("POST", "/egress-proxy-profiles/9/proxy-url/reveal", nil)
	NewHandler(service).proxyProfileURL(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "profile-secret") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "private, no-store" || recorder.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("reveal cache headers = %#v", recorder.Header())
	}
}

func TestProxyProfileListUsesBoundedPaginationAndSearch(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	repository := &proxyRevealRepository{profile: egressdomain.ProxyProfile{ID: 9, Name: "Tokyo", EncryptedProxyURL: "invalid-but-redacted"}}
	service := egressapp.NewService(repository, cipher, "")
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest("GET", "/egress-proxy-profiles?page=2&pageSize=5&search=tokyo", nil)
	NewHandler(service).listProxyProfiles(context)
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), `"page":2`) || !strings.Contains(recorder.Body.String(), `"pageSize":5`) || !strings.Contains(recorder.Body.String(), `"total":1`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if repository.profilePage.Offset != 5 || repository.profilePage.Limit != 5 || repository.profilePage.Search != "tokyo" {
		t.Fatalf("page query = %#v", repository.profilePage)
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
		{path: "/egress-sources?scope=grok_build", want: false},
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

func TestOperationsConfigRequestParsesFallbacks(t *testing.T) {
	input, err := (operationsConfigRequest{
		ProbeProvider: "cloudflare", ProbeIntervalSeconds: 900, AssignmentIntervalSeconds: 300,
		Fallbacks: map[string]operationsFallbackRequest{
			"grok_build": {Mode: "fixed", NodeID: "42"},
			"grok_web":   {Mode: "direct"},
		},
	}).input()
	if err != nil {
		t.Fatal(err)
	}
	if fallback := input.Fallbacks[egressdomain.ScopeBuild]; fallback.Mode != egressdomain.FallbackModeFixed || fallback.NodeID != 42 {
		t.Fatalf("Build fallback = %#v", fallback)
	}
	if fallback := input.Fallbacks[egressdomain.ScopeWeb]; fallback.Mode != egressdomain.FallbackModeDirect || fallback.NodeID != 0 {
		t.Fatalf("Web fallback = %#v", fallback)
	}
	if input.ProbeProvider != egressdomain.ProbeProviderCloudflare {
		t.Fatalf("probe provider = %q", input.ProbeProvider)
	}
}

func TestOperationsConfigRequestRejectsInvalidFallbackNodeID(t *testing.T) {
	_, err := (operationsConfigRequest{
		Fallbacks: map[string]operationsFallbackRequest{"grok_build": {Mode: "fixed", NodeID: "zero"}},
	}).input()
	if !errors.Is(err, egressapp.ErrInvalidInput) {
		t.Fatalf("invalid node ID error = %v", err)
	}
}
