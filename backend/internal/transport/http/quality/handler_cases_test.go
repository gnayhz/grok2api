package qualityhttp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/quality/court"
	"github.com/chenyme/grok2api/backend/internal/quality/evidence"
	"github.com/chenyme/grok2api/backend/internal/quality/guard"
	"github.com/chenyme/grok2api/backend/internal/quality/management"
	"github.com/chenyme/grok2api/backend/internal/quality/model"
	"github.com/chenyme/grok2api/backend/internal/quality/proxy"
	"github.com/chenyme/grok2api/backend/internal/quality/registry"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// TestGetCasesIncludesParties covers real management/Court/Registry composition.
func TestGetCasesIncludesParties(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	ctx := context.Background()
	caseID, err := reg.CreateCase(ctx, time.Now().UTC(), "{}")
	if err != nil {
		t.Fatalf("CreateCase: %v", err)
	}
	// 被告账号 + 共同被冻结出口：验证案件当事方投影
	// 账号档(hasAccount 分支)与出口档两条路径。
	if err := reg.UpsertParty(ctx, model.PartyRecord{
		CaseID: caseID, Kind: model.PartyAccount, AccountID: 42,
		Role: model.RoleDefendant, Disposition: model.DispositionRemanded,
	}); err != nil {
		t.Fatalf("UpsertParty(account): %v", err)
	}
	if err := reg.UpsertParty(ctx, model.PartyRecord{
		CaseID: caseID, Kind: model.PartyExit, NodeID: 7, Epoch: 3,
		Role: model.RoleCoRemanded, Disposition: model.DispositionRemanded,
	}); err != nil {
		t.Fatalf("UpsertParty(exit): %v", err)
	}

	// 本测试只调用查询端点；完整路由装配的必填契约另测。
	handler := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	group := router.Group("")
	handler.Register(group)

	req := httptest.NewRequest(http.MethodGet, "/quality/court/cases", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("案件列表应正常返回 200, 实际 %d: %s", w.Code, w.Body.String())
	}

	var payload struct {
		Data struct {
			Items []struct {
				ID      uint64 `json:"id"`
				Status  string `json:"status"`
				Parties []struct {
					Kind string `json:"kind"`
				} `json:"parties"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应解码失败: %v", err)
	}
	if len(payload.Data.Items) != 1 {
		t.Fatalf("应返回 1 案件, 实际 %d", len(payload.Data.Items))
	}
	item := payload.Data.Items[0]
	if item.Status != string(model.CaseInvestigating) {
		t.Fatalf("案件状态应为 investigating, 实际 %s", item.Status)
	}
	if len(item.Parties) != 2 {
		t.Fatalf("应返回 2 当事方, 实际 %d", len(item.Parties))
	}
}

// TestGetEgressPropagatesBaseProjectionFailure 锚定管理面一致性:
// 质量档案可读不代表底座节点投影也可读。底座查询失败时不能把所有节点
// 静默标成 webhook=false 并返回伪成功，否则人工会错过轮换入口。
func TestGetEgressPropagatesBaseProjectionFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	// 故意不创建 egress_nodes：质量库自身已初始化，底座投影表缺席时
	// 必须暴露真实故障而不是返回 nodes=[] 的 200。
	handler := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	router.GET("/quality/egress", handler.getEgress)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quality/egress", nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("底座节点投影不可读时应返回 500, status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestGetCasesEmitsEmptyPartiesArray 锚定响应契约:即使历史半状态案件
// 暂无当事方,parties 也必须编码为 [] 而不是 null,前端解码器才能继续
// 显示案件列表并暴露异常状态。
func TestGetCasesEmitsEmptyPartiesArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	if _, err := reg.CreateCase(context.Background(), time.Now().UTC(), "{}"); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	handler.Register(router.Group(""))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quality/court/cases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("案件列表应成功返回, status=%d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data struct {
			Items []struct {
				Parties json.RawMessage `json:"parties"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 || string(payload.Data.Items[0].Parties) != "[]" {
		t.Fatalf("空当事方必须编码为 [], got %s", payload.Data.Items[0].Parties)
	}
}

// TestGetCasesDoesNotExposeRetiredRemandDeadline verifies that a pending case
// contains only finite-round state; the retired timer/bail state is not part
// of the management contract.
func TestGetCasesDoesNotExposeRetiredRemandDeadline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx := context.Background()
	opened := time.Now().UTC()
	caseID, err := reg.CreateCase(ctx, opened, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertParty(ctx, model.PartyRecord{
		CaseID: caseID, Kind: model.PartyAccount, AccountID: 42,
		Role: model.RoleDefendant, Disposition: model.DispositionRemanded,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.UpsertParty(ctx, model.PartyRecord{
		CaseID: caseID, Kind: model.PartyExit, NodeID: 7, Epoch: 0,
		Role: model.RoleCoRemanded, Disposition: model.DispositionRemanded,
	}); err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionAccount(ctx, model.AccountTransitionRequest{AccountID: 42, To: model.AccountRemanded, CaseID: caseID}); err != nil {
		t.Fatal(err)
	}
	if err := reg.TransitionExit(ctx, model.ExitTransitionRequest{NodeID: 7, Epoch: 0, To: model.ExitRemanded, CaseID: caseID}); err != nil {
		t.Fatal(err)
	}
	handler := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	handler.Register(router.Group(""))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quality/court/cases", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("案件列表应成功返回, status=%d body=%s", w.Code, w.Body.String())
	}
	var payload struct {
		Data struct {
			Items []map[string]any `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data.Items) != 1 {
		t.Fatalf("retired remand deadline response shape is invalid, payload=%s", w.Body.String())
	}
	if _, present := payload.Data.Items[0]["remand_deadline"]; present {
		t.Fatalf("retired remand deadline must be absent, payload=%s", w.Body.String())
	}
}

// TestNewHandlerRequiresCoreDeps 锚定组装契约:必填依赖缺席必须在
// 构造期点名崩溃(启动期组装错误),而非运行期 nil panic 的 500;
// 全量注入则正常构建。
func TestNewHandlerRequiresCoreDeps(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("缺 Queries/Court/Enforcement/Guard 时构造必须崩溃")
		}
		msg, ok := recovered.(string)
		if !ok || !strings.Contains(msg, "Court") || !strings.Contains(msg, "Guard") {
			t.Fatalf("崩溃信息必须点名缺席字段, got %v", recovered)
		}
	}()
	NewHandler(Deps{}) // 构造期应报告所有缺席依赖
}

// TestPutGuardAcceptsExplicitFalse 锚定管理面 bool 语义:
// 关闭总开关必须能区分显式 false 与字段缺省,且缺省仍应拒绝。
func TestPutGuardAcceptsExplicitFalse(t *testing.T) {
	gin.SetMode(gin.TestMode)
	guardService := guard.New(guard.Config{
		Enabled: true, GuardedModels: []string{"grok-4.5"}, MaxAttempts: 2,
	}, nil)
	handler := &Handler{deps: Deps{
		Guard: guardService,
	}}
	router := gin.New()
	router.PUT("/quality/guard", handler.putGuard)
	router.GET("/quality/guard", handler.getGuard)

	req := httptest.NewRequest(http.MethodPut, "/quality/guard", strings.NewReader(`{"revision":0,"enabled":false,"guarded_models":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("显式 false 必须可保存, status=%d body=%s", w.Code, w.Body.String())
	}
	if guardService.Config().Enabled {
		t.Fatal("显式 false 未写入守卫配置")
	}
	if guardService.Config().Revision != 1 {
		t.Fatal("policy revision did not advance")
	}

	getReq := httptest.NewRequest(http.MethodGet, "/quality/guard", nil)
	getResp := httptest.NewRecorder()
	router.ServeHTTP(getResp, getReq)
	var getPayload struct {
		Data struct {
			GuardedModels []string `json:"guarded_models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(getResp.Body.Bytes(), &getPayload); err != nil {
		t.Fatalf("读取关闭后的守卫配置失败: %v", err)
	}
	if getPayload.Data.GuardedModels == nil {
		t.Fatal("关闭后的 guarded_models 必须是空数组而不是 null")
	}

	req = httptest.NewRequest(http.MethodPut, "/quality/guard", strings.NewReader(`{"guarded_models":["grok-4.5"]}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("缺省 enabled 必须拒绝, status=%d body=%s", w.Code, w.Body.String())
	}
}

// TestOverviewSurfacesObservationDrops 锚定 I19 丢弃可见性:
// 注入计数器时 overview 必须透出 observation_drops;
// 未注入时字段整体缺席(不得透出 null——
// 前端 isOptional 只认 undefined,批8 契约教训同款)。
func TestOverviewSurfacesObservationDrops(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	get := func(handler *Handler) map[string]any {
		t.Helper()
		router := gin.New()
		group := router.Group("")
		handler.Register(group)
		req := httptest.NewRequest(http.MethodGet, "/quality/overview", nil)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("overview 应 200, 实际 %d: %s", w.Code, w.Body.String())
		}
		var payload struct {
			Data map[string]any `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatalf("解码: %v", err)
		}
		return payload.Data
	}

	// 未注入:字段缺席且不是 null。
	bare := get(&Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}})
	if _, exists := bare["observation_drops"]; exists {
		t.Fatalf("未注入时字段应整体缺席, got %v", bare["observation_drops"])
	}
	// 注入:计数器值透出。
	counter := int64(7)
	wired := get(&Handler{deps: Deps{Queries: testManagementQueries(t, reg, func(deps *management.QueryDependencies) { deps.ObservationDrops = func() int64 { return counter } })}})
	got, ok := wired["observation_drops"].(float64)
	if !ok || int64(got) != counter {
		t.Fatalf("observation_drops 应为 %d, got %v", counter, wired["observation_drops"])
	}
}

// mustEvidence 在注册库同库上构建证据局(overview 聚合需要)。
func mustEvidence(t *testing.T, reg *registry.Registry) *evidence.Store {
	t.Helper()
	store, err := evidence.New(context.Background(), reg.DB(), model.DefaultEvidenceConfig())
	if err != nil {
		t.Fatalf("evidence.New: %v", err)
	}
	return store
}

// TestGetProbesFiltersByCaseID 锚定案件详情的数据完整性契约:
// case_id 过滤必须返回该案件的完整任务历史(不受全局最近窗口截断),
// 非法 case_id 必须显式拒绝而不是静默回退全局列表。
func TestGetProbesFiltersByCaseID(t *testing.T) {
	gin.SetMode(gin.TestMode)
	reg, err := registry.Open(context.Background(), registry.Options{
		Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db"),
	})
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close() })

	ctx := context.Background()
	first, err := reg.CreateCase(ctx, time.Now().UTC(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	second, err := reg.CreateCase(ctx, time.Now().UTC(), "{}")
	if err != nil {
		t.Fatal(err)
	}
	tasks := registry.NewProbeTaskStore(reg)
	for i := 0; i < 3; i++ {
		if _, err := tasks.CreateProbeTask(ctx, model.ProbeTask{
			CaseID: first, Direction: model.ProbeAccountDifferential,
			DefendantAccountID: 7, DefendantNodeID: uint64(i + 1), BaselineNodeID: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tasks.CreateProbeTask(ctx, model.ProbeTask{
		CaseID: second, Direction: model.ProbeExitJury, DefendantAccountID: 7, DefendantNodeID: 3,
	}); err != nil {
		t.Fatal(err)
	}

	handler := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	handler.Register(router.Group(""))

	get := func(query string) (int, []struct {
		CaseID uint64 `json:"case_id"`
		ID     uint64 `json:"id"`
	}, string) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/quality/probes"+query, nil))
		var payload struct {
			Data struct {
				Items []struct {
					CaseID uint64 `json:"case_id"`
					ID     uint64 `json:"id"`
				} `json:"items"`
			} `json:"data"`
		}
		if w.Code == http.StatusOK {
			if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
				t.Fatalf("解码: %v", err)
			}
		}
		return w.Code, payload.Data.Items, w.Body.String()
	}

	code, items, body := get("?case_id=" + strconv.FormatUint(first, 10))
	if code != http.StatusOK {
		t.Fatalf("case_id 过滤应 200, got %d: %s", code, body)
	}
	if len(items) != 3 {
		t.Fatalf("案件 %d 应有 3 条任务, got %d", first, len(items))
	}
	for _, item := range items {
		if item.CaseID != first {
			t.Fatalf("case_id 过滤泄漏了其他案件的任务: %+v", item)
		}
	}

	code, items, body = get("?case_id=not-a-number")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 case_id 必须 400, got %d: %s", code, body)
	}
	_ = items
}

// sqlTestNodeProfiles reads the same technical node fields as the assembly adapter.
type sqlTestNodeProfiles struct{ db *gorm.DB }

func (s sqlTestNodeProfiles) ListProfiles(ctx context.Context) ([]proxy.NodeProfile, error) {
	var rows []struct {
		ID              uint64
		RotationEnabled bool
	}
	if err := s.db.WithContext(ctx).Table("egress_nodes").Select("id, rotation_enabled").Find(&rows).Error; err != nil {
		return nil, err
	}
	values := make([]proxy.NodeProfile, 0, len(rows))
	for _, row := range rows {
		values = append(values, proxy.NodeProfile{ID: row.ID, RotationWebhook: row.RotationEnabled})
	}
	return values, nil
}

func testManagementQueries(t *testing.T, reg *registry.Registry, configure func(*management.QueryDependencies)) *management.Queries {
	t.Helper()
	observations := mustEvidence(t, reg)
	cfg := court.DefaultConfig()
	cfg.EvaluateEvery = time.Hour
	service := court.New(cfg, reg, observations, nil, registry.NewProbeTaskStore(reg))
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	deps := management.QueryDependencies{Registry: reg, Evidence: observations, Court: service,
		Probes: registry.NewProbeTaskStore(reg), Guard: guard.New(guard.DefaultConfig(), nil), Nodes: sqlTestNodeProfiles{reg.DB()}}
	if configure != nil {
		configure(&deps)
	}
	return management.NewQueries(deps)
}

func TestCaseProofHTTPPreservesLargeCertificateIdentity(t *testing.T) {
	ctx := context.Background()
	reg, err := registry.Open(ctx, registry.Options{Driver: "sqlite", SQLitePath: filepath.Join(t.TempDir(), "quality.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer reg.Close()
	const id = uint64(9007199254740993)
	now := time.Now().UTC()
	proof := model.ResourceCheckReport{Version: model.ResourceCheckVersion, Results: []model.ResourceProof{{ResourceTarget: model.ResourceTarget{Kind: "account", ResourceID: id}, IdentityGroup: id, Outcome: "degraded", Rule: "R2", Evidence: []int{1, 2}}}}
	raw, _ := json.Marshal(map[string]any{"assessment": map[string]any{"proof": proof}})
	caseID, err := reg.OpenInvestigation(ctx, id, model.EpochKey{NodeID: 7}, now, `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.SettleInvestigation(ctx, caseID, model.VerdictBothGuilty, string(raw), now, true); err != nil {
		t.Fatal(err)
	}
	h := &Handler{deps: Deps{Queries: testManagementQueries(t, reg, nil)}}
	router := gin.New()
	router.GET("/cases", h.getCases)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/cases", nil))
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var body struct {
		Data struct {
			Items []struct {
				Verdict string `json:"verdict"`
				Proof   struct {
					Results []struct {
						ResourceID    string `json:"resource_id"`
						IdentityGroup string `json:"identity_group"`
						Rule          string `json:"rule"`
					} `json:"results"`
				} `json:"proof"`
			} `json:"items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Data.Items) != 1 || body.Data.Items[0].Verdict != "both_guilty" {
		t.Fatal("dual verdict lost")
	}
	got := body.Data.Items[0].Proof.Results
	if len(got) != 1 || got[0].ResourceID != "9007199254740993" || got[0].IdentityGroup != "9007199254740993" || got[0].Rule != "R2" {
		t.Fatal("certificate identity rounded", got)
	}
}
