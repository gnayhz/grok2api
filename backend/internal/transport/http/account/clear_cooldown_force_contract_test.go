package account

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/gin-gonic/gin"
)

// TestClearCooldownForceContract：POST /accounts/:id/clear-cooldown-force 是
// clear-cooldown 的兼容别名：清瞬态冷却（failure_count/cooldown_until 与非
// 持久 last_error），保留 missing-thinking 打击标记，enabled 不动，未知 id
// 返回 404。两路由必须收敛到同一实现——曾存在独立 repo 路径，其失效事件
// 不携带保留后的标记，导致路由覆盖层与数据库终态不一致。
func TestClearCooldownForceContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "clear-cooldown-force.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	repo := relational.NewAccountRepository(database)
	audits := relational.NewAuditRepository(database)
	service := accountapp.NewService(repo, audits, nil, nil, nil, nil, nil)
	handler := NewHandler(service, nil)

	created, _, err := repo.UpsertByIdentity(ctx, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "force", SourceKey: "force",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(created.ID, 10)

	until := time.Now().UTC().Add(time.Hour)
	if err := repo.UpdateHealth(ctx, created.ID, accountdomain.ProviderBuild, 3, &until, accountdomain.LastErrorMissingThinking, false); err != nil {
		t.Fatal(err)
	}

	post := func(target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Params = []gin.Param{{Key: "id", Value: target}}
		ginContext.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/"+target+"/clear-cooldown-force", nil)
		handler.clearCooldownUnconditional(ginContext)
		return recorder
	}

	if rec := post(id); rec.Code != 200 {
		t.Fatalf("force clear: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != 0 || stored.CooldownUntil != nil {
		t.Fatalf("cooldown not cleared: failure=%d cooldown=%v", stored.FailureCount, stored.CooldownUntil)
	}
	// 与 clear-cooldown 相同：打击标记必须保留，二击停用策略不可被别名路由绕过。
	if stored.LastError != accountdomain.LastErrorMissingThinking {
		t.Fatalf("strike marker must survive force clear: lastError=%q", stored.LastError)
	}
	if !stored.Enabled {
		t.Fatal("force clear must not flip the enabled state")
	}
	if rec := post("999999"); rec.Code != 404 {
		t.Fatalf("unknown id must 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}
