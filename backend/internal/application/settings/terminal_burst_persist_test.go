package settings

import (
	"context"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

// TestTerminalBurstThresholdPersistsAcrossSaveAndReload locks the guard
// threshold through GET, Update persist, and domain reload. toDomainConfig
// previously omitted the field so any admin save stored 0 (runtime default 3).
func TestTerminalBurstThresholdPersistsAcrossSaveAndReload(t *testing.T) {
	cfg := testConfig(t)
	cfg.RequestRetry.TerminalBurstThreshold = 5
	repository := &runtimeSettingsRepositoryStub{}
	var applied config.Config
	service := NewService(cfg, time.Time{}, 0, repository, nil, func(next config.Config) { applied = next })

	if got := service.Get().Config.RequestRetry.TerminalBurstThreshold; got != 5 {
		t.Fatalf("GET dropped terminalBurstThreshold: %d", got)
	}

	snapshot, err := service.Update(context.Background(), service.Get().Revision, service.Get().Config)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Config.RequestRetry.TerminalBurstThreshold != 5 {
		t.Fatalf("update snapshot dropped terminalBurstThreshold: %d", snapshot.Config.RequestRetry.TerminalBurstThreshold)
	}
	if applied.RequestRetry.TerminalBurstThreshold != 5 {
		t.Fatalf("apply fanout dropped terminalBurstThreshold: %d", applied.RequestRetry.TerminalBurstThreshold)
	}
	if repository.value.RequestRetry == nil || repository.value.RequestRetry.TerminalBurstThreshold != 5 {
		t.Fatalf("persisted domain dropped terminalBurstThreshold: %+v", repository.value.RequestRetry)
	}

	reloaded := applyDomainConfig(cfg, repository.value)
	if reloaded.RequestRetry.TerminalBurstThreshold != 5 {
		t.Fatalf("reload from persisted domain dropped terminalBurstThreshold: %d", reloaded.RequestRetry.TerminalBurstThreshold)
	}
}
