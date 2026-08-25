package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestUpdateApplyPanicDoesNotAdvanceRevision：apply 回调 panic 时不推进
// lastAppliedRevision——下次 ReloadPersisted 重放该配置（消费者最终收到
// 应用），而不是把失败的 revision 标记为已应用后静默丢失。这是 runApply
// 注释声明的恢复语义，此前无测试锁定。
func TestUpdateApplyPanicDoesNotAdvanceRevision(t *testing.T) {
	repository := &runtimeSettingsRepositoryStub{}
	applies := 0
	service := NewService(testConfig(t), time.Time{}, 0, repository, nil, func(next config.Config) {
		applies++
		if applies == 1 {
			panic("first apply explodes")
		}
	})

	// Update 成功：持久化状态有效，apply panic 被 runApply 恢复并记日志。
	input := service.Get().Config
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatalf("update must succeed despite apply panic: %v", err)
	}
	if applies != 1 {
		t.Fatalf("first apply ran %d times", applies)
	}

	// panic 后重载必须重放：消费者最终收到应用。
	if err := service.ReloadPersisted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if applies != 2 {
		t.Fatalf("apply replay expected: got %d applies (want 2)", applies)
	}
}
