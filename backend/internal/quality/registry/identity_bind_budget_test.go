package registry

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
)

// TestRefreshIdentityGroupsFleetScaleSQLiteBindBudget 回归防护:Fleet 级
// 关联(数万账号)的整表重写必须分片执行。q_identity_group 每行 3 个宿主
// 参数;2000 成员行在未分片的单条 INSERT 中需要 6000 个绑定参数,超过
// glebarez/go-sqlite 的变量上限(实测 749),启动将失败并崩溃退出。
func TestRefreshIdentityGroupsFleetScaleSQLiteBindBudget(t *testing.T) {
	ctx := context.Background()
	// 30,000 成员行与生产同规模等级(61,401 行):未分片的整表重写
	// 必然超出 SQLite 变量预算(实测 glebarez/go-sqlite 上限 749)。
	const pairs = 15000
	links := make([]account.IdentityLink, 0, pairs)
	for i := uint64(1); i <= pairs; i++ {
		links = append(links, account.IdentityLink{AccountID: 2*i - 1, RelatedAccountID: 2 * i})
	}
	source := identityLinksFunc(func(context.Context) ([]account.IdentityLink, error) { return links, nil })
	path := filepath.Join(t.TempDir(), "identity-fleet.db")
	r, err := Open(ctx, Options{SQLitePath: path, AccountLinks: source})
	if err != nil {
		t.Fatalf("fleet-scale startup rebuild failed (SQLite bind budget exceeded?): %v", err)
	}
	t.Cleanup(func() { _ = r.Close() })

	_, first := r.IdentityGroupOf(1)
	if len(first) != 2 || first[0] != 1 || first[1] != 2 {
		t.Fatalf("group projection wrong for pair (1,2): %v", first)
	}
	_, last := r.IdentityGroupOf(2 * pairs)
	if len(last) != 2 || last[0] != 2*pairs-1 || last[1] != 2*pairs {
		t.Fatalf("group projection wrong for final pair: %v", last)
	}
	var persisted int64
	if err := r.db.Model(&qIdentityGroupModel{}).Count(&persisted).Error; err != nil {
		t.Fatal(err)
	}
	if persisted != 2*pairs {
		t.Fatalf("persisted members = %d, want %d", persisted, 2*pairs)
	}
}
