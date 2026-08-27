package settings

import (
	"context"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func boolPtr(value bool) *bool { return &value }

// 回归:mergeEditable 不得原地修改与热配置共享的 BuildProbe 指针。曾因
// 浅拷贝后直接改指针,一次校验失败(非法 timeout)的保存也把内存里的开关
// 翻掉,随后任何一次成功保存都会把这次失败的开关一并持久化。
func TestFailedUpdateMustNotMutateLiveBuildProbe(t *testing.T) {
	cfg := testConfig(t)
	cfg.AccountRisk.RSCCheck.BuildProbe = &config.AccountRiskBuildProbeConfig{Enabled: true}
	repository := &runtimeSettingsRepositoryStub{}
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) {})

	input := service.Get().Config
	input.AccountRiskProvided = true
	input.AccountRisk.BuildProbeEnabled = boolPtr(false)
	input.AccountRisk.Timeout = "120s" // 超界,Update 必须整体失败
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err == nil {
		t.Fatal("out-of-range timeout must be rejected")
	}

	if got := service.Get().Config.AccountRisk.BuildProbeEnabled; got == nil || !*got {
		t.Fatalf("failed update must not flip the live BuildProbe switch: %v", got)
	}

	// 随后一次无关的成功保存也不得把失败提交的开关持久化。
	ok := service.Get().Config
	ok.AccountRiskProvided = false
	ok.Audit.FlushInterval = "9s"
	if _, err := service.Update(context.Background(), service.Get().Revision, ok); err != nil {
		t.Fatal(err)
	}
	if got := service.Get().Config.AccountRisk.BuildProbeEnabled; got == nil || !*got {
		t.Fatalf("unrelated successful save must not persist the rejected switch: %v", got)
	}
}

// 回归:旧持久化载荷带 AccountRisk 节却没有 BuildProbeEnabled 字段时,
// 必须继承文件基线——曾因整节赋值把 yaml 的 buildProbe.enabled: true
// 静默清掉。显式提交(含 false)仍然覆盖基线。
func TestApplyDomainKeepsFileBuildProbeWhenPayloadOmitsField(t *testing.T) {
	cfg := testConfig(t)
	cfg.AccountRisk.RSCCheck.BuildProbe = &config.AccountRiskBuildProbeConfig{Enabled: true}

	legacy := settingsdomain.Config{AccountRisk: &settingsdomain.AccountRiskConfig{
		Enabled: true, Method: "ssoProbe", Concurrency: 2, Timeout: 30 * time.Second,
		OnDenied: "flag", PatrolEnabled: false, PatrolBucketDays: 30,
	}}
	merged := applyDomainConfig(cfg, legacy)
	if !merged.AccountRisk.RSCCheck.BuildProbeEnabled() {
		t.Fatal("legacy payload without buildProbeEnabled must inherit the file baseline")
	}

	explicitFalse := legacy
	explicitFalse.AccountRisk.BuildProbeEnabled = boolPtr(false)
	merged = applyDomainConfig(cfg, explicitFalse)
	if merged.AccountRisk.RSCCheck.BuildProbeEnabled() {
		t.Fatal("explicit false must override the file baseline")
	}
}

// 回归:管理端提交 accountRisk 节但漏掉 buildProbeEnabled 字段(旧客户端
// 或部分更新)时,不得把已打开的探针误关——开关打开会消耗账号额度,
// 误关与误开都不该由字段缺省引发。
func TestUpdateWithoutBuildProbeFieldKeepsCurrentSwitch(t *testing.T) {
	cfg := testConfig(t)
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	// 先打开 Build 探针。
	on := service.Get().Config
	on.AccountRiskProvided = true
	on.AccountRisk.BuildProbeEnabled = boolPtr(true)
	if _, err := service.Update(context.Background(), service.Get().Revision, on); err != nil {
		t.Fatal(err)
	}

	// 再提交一个不带该字段的整体节点:开关保持打开。
	omit := service.Get().Config
	omit.AccountRiskProvided = true
	omit.AccountRisk.BuildProbeEnabled = nil
	omit.AccountRisk.Concurrency = 5
	snapshot, err := service.Update(context.Background(), service.Get().Revision, omit)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Config.AccountRisk.BuildProbeEnabled; got == nil || !*got {
		t.Fatalf("omitted buildProbeEnabled must keep the current switch: %v", got)
	}
	if !applied.AccountRisk.RSCCheck.BuildProbeEnabled() {
		t.Fatal("apply fanout must keep buildProbe enabled")
	}
	if snapshot.Config.AccountRisk.Concurrency != 5 {
		t.Fatalf("other fields must still apply: %#v", snapshot.Config.AccountRisk)
	}
}
