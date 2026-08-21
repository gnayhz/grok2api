package gateway

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/domain/model"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/repository"
)

// TestResolvePublicModelRoutesDistinguishesNoAccount 锁定 404/503 消歧：
// 路由已启用但 Provider 无可用账号时，候选查询（带账号存在性谓词）为空，
// 解析必须返回 ErrNoAvailableAccount（→503 upstream_unavailable）而非
// ErrModelNotFound（→404）。背景：availableRoutePredicate 把无账号 Provider
// 的路由整体过滤，实测 grok-4.20-0309-reasoning 无 console 账号时曾被误报
// 「模型不存在」。同时锁定真不存在的模型仍为 404 语义。
func TestResolvePublicModelRoutesDistinguishesNoAccount(t *testing.T) {
	ctx := context.Background()
	database, err := relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "no-account-shape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	modelRepo := relational.NewModelRepository(database)
	// 建路由但不建任何 console 账号：路由存在、启用、零账号。
	if err := modelRepo.UpsertDiscovered(ctx, account.ProviderConsole, []string{"grok-4.20-0309-reasoning"}); err != nil {
		t.Fatal(err)
	}
	// 真实关系仓储作为 resolver（HasEnabledRouteByPublicID 走真实 SQL）。
	service := &Service{models: modelRepo, logger: nil}

	if _, _, err := service.resolvePublicModelRoutes(ctx, "grok-4.20-0309-reasoning", true); !errorsIs(err, ErrNoAvailableAccount) {
		t.Fatalf("无账号路由应返回 ErrNoAvailableAccount（503 语义），得到 %v", err)
	}
	if _, _, err := service.resolvePublicModelRoutes(ctx, "definitely-not-a-model", true); !errorsIs(err, repository.ErrNotFound) {
		t.Fatalf("未知模型在解析层应返回 repository.ErrNotFound（调用者映射 404），得到 %v", err)
	}
	// effort 别名出口（round 16 对抗审查补）：base 是无账号路由时，
	// grok-<base>-low 不得误报 404——别名解析失败必须同样消歧为 503 语义。
	if _, _, err := service.resolvePublicModelRoutes(ctx, "grok-4.20-0309-reasoning-low", true); !errorsIs(err, ErrNoAvailableAccount) {
		t.Fatalf("无账号 base 的 effort 别名应返回 ErrNoAvailableAccount，得到 %v", err)
	}
	// 对照：effort 别名指向不存在的 base 仍是 404 语义。
	if _, _, err := service.resolvePublicModelRoutes(ctx, "definitely-not-a-model-low", true); !errorsIs(err, repository.ErrNotFound) {
		t.Fatalf("未知 base 的 effort 别名应为 repository.ErrNotFound，得到 %v", err)
	}
	_ = audit.OperationChat
	_ = model.Route{}
	_ = repository.ErrNotFound
	_ = time.Now
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
