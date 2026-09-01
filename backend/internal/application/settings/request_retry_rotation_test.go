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
	cfg.RequestRetry.Enabled = true
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	input := service.Get().Config
	if !input.RequestRetry.Enabled {
		t.Fatal("GET must surface file requestRetry.enabled")
	}
	input.RequestRetryProvided = true
	input.RequestRetry.Enabled = false
	input.RequestRetry.CreatedTimeout = "12s"
	input.EgressRotationProvided = true
	input.EgressRotation.MinNodeInterval = "3m"

	snapshot, err := service.Update(context.Background(), service.Get().Revision, input)
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Config.RequestRetry.CreatedTimeout; got != "12s" {
		t.Fatalf("createdTimeout = %q, want 12s", got)
	}
	if snapshot.Config.RequestRetry.Enabled {
		t.Fatal("overlay enabled=false must win over file true")
	}
	if got := snapshot.Config.EgressRotation.MinNodeInterval; got != "3m" {
		t.Fatalf("minNodeInterval = %q, want 3m", got)
	}
	if applied.RequestRetry.CreatedTimeout.Value() != 12*time.Second {
		t.Fatalf("apply fanout createdTimeout = %v", applied.RequestRetry.CreatedTimeout)
	}
	if applied.RequestRetry.Enabled {
		t.Fatal("apply fanout enabled=false must win over file true")
	}
	if applied.Egress.Rotation.MinNodeInterval.Value() != 3*time.Minute {
		t.Fatalf("apply fanout minNodeInterval = %v", applied.Egress.Rotation.MinNodeInterval)
	}
	persisted := repository.value.RequestRetry
	if persisted == nil {
		t.Fatal("persist must keep requestRetry section")
	}
	if persisted.Enabled {
		t.Fatal("persist overlay must write enabled=false")
	}
	if applyDomainConfig(cfg, repository.value).RequestRetry.Enabled {
		t.Fatal("apply of persisted overlay must keep enabled=false")
	}

	// 旧持久化载荷(整节缺失)沿用文件基线。
	legacy := applyDomainConfig(cfg, settingsdomain.Config{})
	if legacy.RequestRetry.CreatedTimeout.Value() != cfg.RequestRetry.CreatedTimeout.Value() {
		t.Fatalf("legacy payload must inherit file requestRetry: %v vs %v", legacy.RequestRetry.CreatedTimeout, cfg.RequestRetry.CreatedTimeout)
	}
	if !legacy.RequestRetry.Enabled {
		t.Fatal("legacy nil persist must inherit file enabled=true")
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
	if snapshot2.Config.RequestRetry.Enabled {
		t.Fatal("omitted section must keep overlay enabled=false")
	}
}

func sameStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// guardedModels 是 yaml 级白名单，不在管理端 DTO 内。GET→PUT 与旧持久化
// 节（字段缺省 / 空切片 / 陈旧名单）都不得覆盖文件基线。overlay
// Enabled=false 仍覆盖文件 true。
func TestGuardedModelsStayOnFileBaselineAcrossAdminRoundtrip(t *testing.T) {
	want := []string{"grok-4.5", "grok-4.6"}
	cfg := testConfig(t)
	cfg.RequestRetry.Enabled = true
	cfg.RequestRetry.GuardedModels = append([]string(nil), want...)

	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	input := service.Get().Config
	if !input.RequestRetryProvided {
		t.Fatal("GET must mark requestRetry provided")
	}
	if !input.RequestRetry.Enabled {
		t.Fatal("GET must surface file requestRetry.enabled")
	}
	input.RequestRetry.CreatedTimeout = "12s"
	if _, err := service.Update(context.Background(), service.Get().Revision, input); err != nil {
		t.Fatal(err)
	}
	if !sameStrings(applied.RequestRetry.GuardedModels, want) {
		t.Fatalf("GET/PUT must keep yaml guardedModels, got %#v", applied.RequestRetry.GuardedModels)
	}
	if !applied.RequestRetry.Enabled {
		t.Fatal("GET/PUT must keep overlay enabled=true")
	}
	persisted := repository.value.RequestRetry
	if persisted == nil {
		t.Fatal("persist must keep requestRetry section")
	}
	if !persisted.Enabled {
		t.Fatal("persist must write overlay enabled=true")
	}
	if len(persisted.GuardedModels) != 0 {
		t.Fatalf("persist must not write yaml-only guardedModels, got %#v", persisted.GuardedModels)
	}
	merged := applyDomainConfig(cfg, repository.value)
	if !sameStrings(merged.RequestRetry.GuardedModels, want) {
		t.Fatalf("apply of persisted overlay must still keep yaml, got %#v", merged.RequestRetry.GuardedModels)
	}
	if !merged.RequestRetry.Enabled {
		t.Fatal("apply of persisted overlay must keep enabled=true")
	}

	off := service.Get().Config
	off.RequestRetryProvided = true
	off.RequestRetry.Enabled = false
	if _, err := service.Update(context.Background(), service.Get().Revision, off); err != nil {
		t.Fatal(err)
	}
	if !sameStrings(applied.RequestRetry.GuardedModels, want) {
		t.Fatalf("PUT enabled=false must keep yaml guardedModels, got %#v", applied.RequestRetry.GuardedModels)
	}
	if applied.RequestRetry.Enabled {
		t.Fatal("PUT overlay enabled=false must win over file true")
	}
	persistedOff := repository.value.RequestRetry
	if persistedOff == nil {
		t.Fatal("persist must keep requestRetry section after enabled=false")
	}
	if persistedOff.Enabled {
		t.Fatal("persist must write overlay enabled=false")
	}
	if len(persistedOff.GuardedModels) != 0 {
		t.Fatalf("persist must not write yaml-only guardedModels, got %#v", persistedOff.GuardedModels)
	}
	mergedOff := applyDomainConfig(cfg, repository.value)
	if !sameStrings(mergedOff.RequestRetry.GuardedModels, want) {
		t.Fatalf("apply after PUT enabled=false must still keep yaml, got %#v", mergedOff.RequestRetry.GuardedModels)
	}
	if mergedOff.RequestRetry.Enabled {
		t.Fatal("apply after PUT enabled=false must keep overlay false")
	}
	if applied.RequestRetry.CreatedTimeout.Value() != 12*time.Second {
		t.Fatalf("PUT enabled=false must keep overlay createdTimeout 12s, got %v", applied.RequestRetry.CreatedTimeout)
	}
	if persistedOff.CreatedTimeout != 12*time.Second {
		t.Fatalf("persist after PUT enabled=false must keep createdTimeout 12s, got %v", persistedOff.CreatedTimeout)
	}
	if mergedOff.RequestRetry.CreatedTimeout.Value() != 12*time.Second {
		t.Fatalf("apply after PUT enabled=false must keep overlay createdTimeout 12s, got %v", mergedOff.RequestRetry.CreatedTimeout)
	}
	if got := service.Get().Config.RequestRetry.CreatedTimeout; got != "12s" {
		t.Fatalf("GET after PUT enabled=false must keep overlay createdTimeout 12s, got %q", got)
	}

	blank := service.Get().Config
	blank.RequestRetryProvided = true
	blank.RequestRetry.CreatedTimeout = ""
	if _, err := service.Update(context.Background(), service.Get().Revision, blank); err == nil {
		t.Fatal("PUT empty createdTimeout must be rejected")
	}
	if got := service.Get().Config.RequestRetry.CreatedTimeout; got != "12s" {
		t.Fatalf("rejected empty createdTimeout must keep overlay 12s, got %q", got)
	}
	if repository.value.RequestRetry == nil || repository.value.RequestRetry.CreatedTimeout != 12*time.Second {
		t.Fatalf("rejected empty createdTimeout must not persist-wipe createdTimeout, got %#v", repository.value.RequestRetry)
	}

	blankEv := service.Get().Config
	if blankEv.RequestRetry.EvidenceTimeout != "3.5s" {
		t.Fatalf("GET evidenceTimeout = %q, want 3.5s before empty PUT", blankEv.RequestRetry.EvidenceTimeout)
	}
	blankEv.RequestRetryProvided = true
	blankEv.RequestRetry.EvidenceTimeout = ""
	if _, err := service.Update(context.Background(), service.Get().Revision, blankEv); err == nil {
		t.Fatal("PUT empty evidenceTimeout must be rejected")
	}
	if got := service.Get().Config.RequestRetry.EvidenceTimeout; got != "3.5s" {
		t.Fatalf("rejected empty evidenceTimeout must keep overlay 3.5s, got %q", got)
	}
	if got := service.Get().Config.RequestRetry.CreatedTimeout; got != "12s" {
		t.Fatalf("rejected empty evidenceTimeout must keep overlay createdTimeout 12s, got %q", got)
	}
	if repository.value.RequestRetry == nil || repository.value.RequestRetry.EvidenceTimeout != 3500*time.Millisecond {
		t.Fatalf("rejected empty evidenceTimeout must not persist-wipe evidenceTimeout, got %#v", repository.value.RequestRetry)
	}

	blankIdle := service.Get().Config
	wantIdle := blankIdle.RequestRetry.IdleAccountCooldown
	keptIdle := repository.value.RequestRetry.IdleAccountCooldown
	blankIdle.RequestRetryProvided = true
	blankIdle.RequestRetry.IdleAccountCooldown = ""
	if _, err := service.Update(context.Background(), service.Get().Revision, blankIdle); err == nil {
		t.Fatal("PUT empty idleAccountCooldown must be rejected")
	}
	if got := service.Get().Config.RequestRetry.IdleAccountCooldown; got != wantIdle {
		t.Fatalf("rejected empty idleAccountCooldown must keep overlay %q, got %q", wantIdle, got)
	}
	if got := service.Get().Config.RequestRetry.CreatedTimeout; got != "12s" {
		t.Fatalf("rejected empty idleAccountCooldown must keep overlay createdTimeout 12s, got %q", got)
	}
	if repository.value.RequestRetry == nil || repository.value.RequestRetry.IdleAccountCooldown != keptIdle {
		t.Fatalf("rejected empty idleAccountCooldown must not persist-wipe idleAccountCooldown, got %#v", repository.value.RequestRetry)
	}

	omitted := settingsdomain.Config{RequestRetry: &settingsdomain.RequestRetryConfig{
		Enabled: false, MaxAttempts: 2, OnExhausted: "fail_closed",
	}}
	mergedOmitted := applyDomainConfig(cfg, omitted)
	if got := mergedOmitted.RequestRetry.GuardedModels; !sameStrings(got, want) {
		t.Fatalf("nil overlay guardedModels must keep yaml, got %#v", got)
	}
	if mergedOmitted.RequestRetry.Enabled {
		t.Fatal("omitted overlay enabled=false must win over file true")
	}

	empty := omitted
	empty.RequestRetry = &settingsdomain.RequestRetryConfig{
		Enabled: false, MaxAttempts: 2, OnExhausted: "fail_closed", GuardedModels: []string{},
	}
	mergedEmpty := applyDomainConfig(cfg, empty)
	if got := mergedEmpty.RequestRetry.GuardedModels; !sameStrings(got, want) {
		t.Fatalf("empty overlay guardedModels must keep yaml, got %#v", got)
	}
	if mergedEmpty.RequestRetry.Enabled {
		t.Fatal("empty overlay enabled=false must win over file true")
	}

	stale := omitted
	stale.RequestRetry = &settingsdomain.RequestRetryConfig{
		Enabled: false, MaxAttempts: 2, OnExhausted: "fail_closed", GuardedModels: []string{"grok-4.3"},
	}
	mergedStale := applyDomainConfig(cfg, stale)
	if got := mergedStale.RequestRetry.GuardedModels; !sameStrings(got, want) {
		t.Fatalf("stale persist must not override yaml guardedModels, got %#v", got)
	}
	if mergedStale.RequestRetry.Enabled {
		t.Fatal("stale overlay enabled=false must win over file true")
	}
}
