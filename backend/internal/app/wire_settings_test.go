package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/config"
)

func TestSettingsApplyPreservesExplicitClearanceURLClear(t *testing.T) {
	t.Setenv(config.DatabaseURLEnv, "")
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := "secrets:\n  jwtSecret: 'fictional-settings-test-secret-000000000000'\n  credentialEncryptionKey: 'MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA='\n"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	base, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base.Provider.Web.ClearanceMode = config.ClearanceModeManual
	base.Provider.Web.FlareSolverrURL = "http://solver.example.test:8191"
	runtime := config.ToRuntimeSettings(base)
	runtime.ProviderWeb.FlareSolverrURL = ""
	var applied string
	err = settingsApply(base, func(next config.Config) error {
		applied = next.Provider.Web.FlareSolverrURL
		return nil
	})(context.Background(), runtime)
	if err != nil {
		t.Fatal(err)
	}
	if applied != "" {
		t.Fatalf("explicitly cleared URL was restored from the file: %q", applied)
	}
}
