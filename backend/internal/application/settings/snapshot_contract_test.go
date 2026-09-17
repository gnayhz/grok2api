package settings

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
)

func TestRejectedUpdatePreservesNestedSettings(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "validation", true: "durable_conflict"}[conflict], func(t *testing.T) {
			cfg := testConfig(t)
			repo := &runtimeSettingsRepositoryStub{}
			service := newTestService(cfg, time.Time{}, 0, repo, nil, nil)
			service.SetFileConfig(runtimeOf(cfg))
			// Reset makes the active snapshot share the file baseline unless writes
			// explicitly detach optional sections before editing them.
			if _, err := service.ResetToDefaults(context.Background(), 0); err != nil {
				t.Fatal(err)
			}
			before := service.Get()
			input := before.Config
			input.RequestRetry.Enabled = !input.RequestRetry.Enabled
			input.EgressRotation.MaxGlobalPerHour++
			wantErr := ErrInvalidInput
			if conflict {
				repo.revision++
				wantErr = ErrConflict
			} else {
				input.EgressRotation.ProbeInterval = "invalid"
			}
			if _, err := service.Update(context.Background(), before.Revision, input); !errors.Is(err, wantErr) {
				t.Fatalf("Update error = %v, want %v", err, wantErr)
			}
			after := service.Get()
			if !reflect.DeepEqual(after, before) {
				t.Fatal("rejected update changed the saved snapshot or file baseline")
			}
		})
	}
}

func TestReloadResolvesLegacySettingsBeforePublishing(t *testing.T) {
	cfg := testConfig(t)
	cfg.Frontend.PublicAPIBaseURL = "https://file.example.test/api"
	legacy := runtimeOf(cfg)
	legacy.Server.MaxConcurrentRequests = 0
	legacy.ProviderConsole = settingsdomain.ProviderConsoleConfig{}
	legacy.Routing.CapacityWait = 0
	legacy.Routing.AccountIsolatedConnections = nil
	legacy.Routing.SegmentedSelector = nil
	legacy.EgressRotation = nil
	legacy.RequestRetry = nil
	legacy.Audit.RetentionPeriod = nil
	legacy.Audit.RetentionSource = ""
	days := 3
	legacy.Audit.RetentionDays = &days
	repo := &runtimeSettingsRepositoryStub{value: legacy, revision: 1, found: true}
	var applied settingsdomain.Config
	service := newTestService(cfg, time.Time{}, 0, repo, nil, func(next settingsdomain.Config) { applied = next })
	service.SetFileConfig(runtimeOf(cfg))
	got, err := service.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	startup, _, _, err := loadPersistedConfig(context.Background(), cfg, repo)
	if err != nil {
		t.Fatal(err)
	}
	want := toEditable(runtimeOf(startup))
	want.Audit.FileRetentionPeriod = got.Config.Audit.FileRetentionPeriod
	want.Audit.FileRetentionSource = got.Config.Audit.FileRetentionSource
	if !reflect.DeepEqual(got.Config, want) {
		t.Fatal("reload snapshot differs from the configuration resolved at startup")
	}
	if !reflect.DeepEqual(applied, runtimeOf(startup)) {
		t.Fatal("apply target received the unresolved legacy payload")
	}
}

func TestPublicAPIURLPreservesFileFallback(t *testing.T) {
	cfg := testConfig(t)
	cfg.Frontend.PublicAPIBaseURL = "https://file.example.test/api/"
	service := newTestService(cfg, time.Time{}, 0, &runtimeSettingsRepositoryStub{}, nil, nil)
	service.SetFileConfig(runtimeOf(cfg))
	if got := service.PublicAPIBaseURL(); got != "https://file.example.test/api" {
		t.Fatalf("public URL = %q; file baseline was lost", got)
	}
	input := service.Get().Config
	input.Frontend.PublicAPIBaseURL = "https://override.example.test/"
	if _, err := service.Update(context.Background(), 0, input); err != nil {
		t.Fatal(err)
	}
	input = service.Get().Config
	input.Frontend.PublicAPIBaseURL = ""
	if _, err := service.Update(context.Background(), 1, input); err != nil {
		t.Fatal(err)
	}
	if got := service.PublicAPIBaseURL(); got != "https://file.example.test/api" {
		t.Fatalf("cleared override resolved to %q", got)
	}
}

func TestUpdateDoesNotTreatInvalidValuesAsLegacyOmissions(t *testing.T) {
	for name, mutate := range map[string]func(*EditableConfig){
		"server capacity": func(c *EditableConfig) { c.Server.MaxConcurrentRequests = -1 },
		"routing wait":    func(c *EditableConfig) { c.Routing.CapacityWait = "-1s" },
		"build timeout":   func(c *EditableConfig) { c.ProviderBuild.ResponseHeaderTimeout = "0s" },
		"clearance mode":  func(c *EditableConfig) { c.ProviderWeb.ClearanceProvided = true; c.ProviderWeb.ClearanceMode = "" },
	} {
		t.Run(name, func(t *testing.T) {
			repo := &runtimeSettingsRepositoryStub{}
			service := newTestService(testConfig(t), time.Time{}, 0, repo, nil, nil)
			input := service.Get().Config
			mutate(&input)
			if _, err := service.Update(context.Background(), 0, input); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("Update error = %v; invalid current input was accepted as legacy data", err)
			}
			if repo.found {
				t.Fatal("invalid settings were persisted")
			}
		})
	}
}
