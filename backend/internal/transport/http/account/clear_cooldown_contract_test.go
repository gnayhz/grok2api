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

// TestClearCooldownContract：POST /accounts/:id/clear-cooldown 是人工运维逃生门
// ——无条件清零 failure_count/cooldown_until/last_error（含 5xx 冷却与
// missing-thinking 打击标记），enabled 状态保持不动，未知 id 返回 404。
func TestClearCooldownContract(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "clear-cooldown-contract.db"))
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
		Provider: accountdomain.ProviderBuild, Name: "cooldown", SourceKey: "cooldown",
		EncryptedAccessToken: "enc", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
		Priority: 1, MaxConcurrent: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatUint(created.ID, 10)

	// 制造一个带打击标记的冷却（模拟 missing-thinking 一次打击后的状态）。
	until := time.Now().UTC().Add(time.Hour)
	if err := repo.UpdateHealth(ctx, created.ID, accountdomain.ProviderBuild, 2, &until, accountdomain.LastErrorMissingThinking, false); err != nil {
		t.Fatal(err)
	}

	post := func(target string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Params = []gin.Param{{Key: "id", Value: target}}
		ginContext.Request = httptest.NewRequest("POST", "/api/admin/v1/accounts/"+target+"/clear-cooldown", nil)
		handler.clearCooldown(ginContext)
		return recorder
	}

	if rec := post(id); rec.Code != 200 {
		t.Fatalf("clear: status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err := repo.Get(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.FailureCount != 0 || stored.CooldownUntil != nil {
		t.Fatalf("cooldown not cleared: failure=%d cooldown=%v", stored.FailureCount, stored.CooldownUntil)
	}
	// 打击标记必须保留：清掉计时器不得把下次降智当首次打击（二击停用不可绕过）。
	if stored.LastError != accountdomain.LastErrorMissingThinking {
		t.Fatalf("strike marker must survive clear-cooldown: lastError=%q", stored.LastError)
	}
	if !stored.Enabled {
		t.Fatal("clear-cooldown must not flip the enabled state")
	}
	if rec := post("999999"); rec.Code != 404 {
		t.Fatalf("unknown id must 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}
