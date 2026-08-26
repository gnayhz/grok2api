package settings

import (
	"context"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestAccountRiskRuntimeOverride 验证账号风险归因节进入运行时设置面：
// 1) 提交后覆盖文件基线、持久化并扇出 apply；2) 旧持久化载荷(nil 节)
// 沿用文件基线；3) 旧管理端未提交该节时不清当前值；4) 非法值被拒。
func TestAccountRiskRuntimeOverride(t *testing.T) {
	cfg := testConfig(t)
	cfg.AccountRisk.RSCCheck.Enabled = false
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	input := service.Get().Config
	input.AccountRiskProvided = true
	input.AccountRisk.Enabled = true
	input.AccountRisk.Method = "ssoProbe"
	input.AccountRisk.Concurrency = 3
	input.AccountRisk.Timeout = "45s"
	input.AccountRisk.OnDenied = "markOnly"
	input.AccountRisk.PatrolEnabled = true
	input.AccountRisk.PatrolBucketDays = 14

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Config.AccountRisk.Enabled || snapshot.Config.AccountRisk.OnDenied != "markOnly" || snapshot.Config.AccountRisk.Timeout != "45s" {
		t.Fatalf("snapshot accountRisk = %#v", snapshot.Config.AccountRisk)
	}
	if !applied.AccountRisk.RSCCheck.Enabled || applied.AccountRisk.RSCCheck.Patrol.BucketDays != 14 || applied.AccountRisk.RSCCheck.Timeout.Value() != 45*time.Second {
		t.Fatalf("apply fanout accountRisk = %#v", applied.AccountRisk.RSCCheck)
	}

	// 旧持久化载荷(整节缺失)沿用文件基线。
	legacy := applyDomainConfig(cfg, settingsdomain.Config{})
	if legacy.AccountRisk.RSCCheck.Enabled != cfg.AccountRisk.RSCCheck.Enabled || legacy.AccountRisk.RSCCheck.Method != cfg.AccountRisk.RSCCheck.Method {
		t.Fatalf("legacy payload must inherit file accountRisk: %#v vs %#v", legacy.AccountRisk.RSCCheck, cfg.AccountRisk.RSCCheck)
	}

	// 未提交该节(旧管理端)时,更新其他字段不得清掉当前值。
	kept := service.Get().Config
	kept.AccountRiskProvided = false
	kept.Audit.FlushInterval = "9s"
	keptSnapshot, err := service.Update(context.Background(), service.Get().Revision, kept)
	if err != nil {
		t.Fatal(err)
	}
	if !keptSnapshot.Config.AccountRisk.Enabled || keptSnapshot.Config.AccountRisk.PatrolBucketDays != 14 {
		t.Fatalf("omitted node must keep current value: %#v", keptSnapshot.Config.AccountRisk)
	}

	// 非法 method / onDenied / 超界值必须被整体校验拒绝。
	bad := service.Get().Config
	bad.AccountRiskProvided = true
	bad.AccountRisk.Method = "rscPayload"
	if _, err := service.Update(context.Background(), service.Get().Revision, bad); err == nil {
		t.Fatal("invalid method must be rejected")
	}
	bad.AccountRisk.Method = "ssoProbe"
	bad.AccountRisk.OnDenied = "explode"
	if _, err := service.Update(context.Background(), service.Get().Revision, bad); err == nil {
		t.Fatal("invalid onDenied must be rejected")
	}
	bad.AccountRisk.OnDenied = "flag"
	bad.AccountRisk.Timeout = "120s"
	if _, err := service.Update(context.Background(), service.Get().Revision, bad); err == nil {
		t.Fatal("out-of-range timeout must be rejected")
	}
}
