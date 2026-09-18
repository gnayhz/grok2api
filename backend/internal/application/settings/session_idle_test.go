package settings

import (
	"context"
	"errors"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestSessionIdleSettingPersistsHotAppliesAndPreservesLegacyInput(t *testing.T) {
	cfg := testConfig(t)
	repo := &runtimeSettingsRepositoryStub{}
	var applied settingsdomain.Config
	service := newTestService(cfg, time.Time{}, 0, repo, nil, func(next settingsdomain.Config) { applied = next })
	if service.Get().Config.ProviderBuild.SessionIdleConnTimeout != "5m" {
		t.Fatal("default must be five minutes")
	}
	input := service.Get().Config
	input.ProviderBuild.SessionIdleConnTimeout = "8m"
	snapshot, err := service.Update(context.Background(), 0, input)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.RestartRequired) != 0 || applied.ProviderBuild.SessionIdleConnTimeout != 8*time.Minute {
		t.Fatalf("not hot applied: %+v", snapshot.RestartRequired)
	}
	loaded, _, _, err := loadPersistedConfig(context.Background(), cfg, repo)
	if err != nil || loaded.Provider.Build.SessionIdleConnTimeout.Value() != 8*time.Minute {
		t.Fatalf("reload: %v, %v", loaded.Provider.Build.SessionIdleConnTimeout, err)
	}
	input = snapshot.Config
	input.ProviderBuild.SessionIdleConnTimeout = "" // old client omits field
	if _, err := service.Update(context.Background(), snapshot.Revision, input); err != nil {
		t.Fatal(err)
	}
	if repo.value.ProviderBuild.SessionIdleConnTimeout != 8*time.Minute {
		t.Fatal("legacy submission reset the value")
	}
}

func TestSessionIdleSettingRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"0s", "-1s", "29s", "30m1s", "invalid"} {
		t.Run(value, func(t *testing.T) {
			repo := &runtimeSettingsRepositoryStub{}
			s := newTestService(testConfig(t), time.Time{}, 0, repo, nil, nil)
			input := s.Get().Config
			input.ProviderBuild.SessionIdleConnTimeout = value
			if _, err := s.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("error = %v", err)
			}
			if repo.found {
				t.Fatal("invalid value persisted")
			}
		})
	}
}

func TestSessionIdleOldPersistedSettingsUseDefault(t *testing.T) {
	cfg := testConfig(t)
	legacy := config.ToRuntimeSettings(cfg)
	legacy.ProviderBuild.SessionIdleConnTimeout = 0
	repo := &runtimeSettingsRepositoryStub{found: true, value: legacy, revision: 1}
	loaded, _, _, err := loadPersistedConfig(context.Background(), cfg, repo)
	if err != nil || loaded.Provider.Build.SessionIdleConnTimeout.Value() != 5*time.Minute {
		t.Fatalf("legacy load: %v %v", loaded.Provider.Build.SessionIdleConnTimeout, err)
	}
	if _, err := config.ApplyRuntimeSnapshot(cfg, legacy); err == nil {
		t.Fatal("explicit invalid snapshot was defaulted")
	}
}
