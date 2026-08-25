package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

// TestRequestRetryAndEgressRotationRuntimeOverride 验证两个新增可热更新节:
// 1) 提交后覆盖文件基线、持久化并扇出到 apply; 2) 旧持久化载荷(nil 节)
// 沿用文件基线而不是把零值当作"全部关闭"。
func TestRequestRetryAndEgressRotationRuntimeOverride(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	input := service.Get().Config
	input.RequestRetryProvided = true
	input.RequestRetry.CreatedTimeout = "12s"
	input.RequestRetry.EarlyHeaderAbort = "0s"
	input.EgressRotationProvided = true
	input.EgressRotation.MinNodeInterval = "3m"

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Config.RequestRetry.CreatedTimeout; got != "12s" {
		t.Fatalf("createdTimeout = %q, want 12s", got)
	}
	if got := snapshot.Config.EgressRotation.MinNodeInterval; got != "3m" {
		t.Fatalf("minNodeInterval = %q, want 3m", got)
	}
	if applied.RequestRetry.CreatedTimeout.Value() != 12*time.Second {
		t.Fatalf("apply fanout createdTimeout = %v", applied.RequestRetry.CreatedTimeout)
	}
	if applied.Egress.Rotation.MinNodeInterval.Value() != 3*time.Minute {
		t.Fatalf("apply fanout minNodeInterval = %v", applied.Egress.Rotation.MinNodeInterval)
	}
	if applied.RequestRetry.EarlyHeaderAbort.Value() != 0 {
		t.Fatalf("earlyHeaderAbort must support explicit 0 (disabled): %v", applied.RequestRetry.EarlyHeaderAbort)
	}

	// 旧持久化载荷(整节缺失)沿用文件基线。
	legacy := applyDomainConfig(cfg, settingsdomain.Config{})
	if legacy.RequestRetry.CreatedTimeout.Value() != cfg.RequestRetry.CreatedTimeout.Value() {
		t.Fatalf("legacy payload must inherit file requestRetry: %v vs %v", legacy.RequestRetry.CreatedTimeout, cfg.RequestRetry.CreatedTimeout)
	}
	if legacy.Egress.Rotation.MinNodeInterval.Value() != cfg.Egress.Rotation.MinNodeInterval.Value() {
		t.Fatalf("legacy payload must inherit file rotation interval: %v vs %v", legacy.Egress.Rotation.MinNodeInterval, cfg.Egress.Rotation.MinNodeInterval)
	}

	// 未提交两节(旧管理端)时,更新其他字段不得清掉这两节的当前值。
	kept := service.Get().Config
	kept.RequestRetryProvided = false
	kept.EgressRotationProvided = false
	kept.Server.MaxConcurrentRequests = kept.Server.MaxConcurrentRequests + 1
	snapshot2, err := service.Update(context.Background(), service.Get().Revision, kept)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot2.Config.RequestRetry.CreatedTimeout != "12s" || snapshot2.Config.EgressRotation.MinNodeInterval != "3m" {
		t.Fatalf("omitted sections must keep current values: %+v %+v", snapshot2.Config.RequestRetry, snapshot2.Config.EgressRotation)
	}
}
