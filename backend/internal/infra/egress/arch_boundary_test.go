package egress

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestInfraLayerNeverImportsApplication guards layering: the infrastructure
// layer (infra/) must not import the application layer (application/) —
// dependencies must point strictly downward (infra -> pkg/domain). Historically
// infra/egress imported application/egress for NormalizeProxyURL and
// SanitizeCloudflareCookies; both implementations now live in neutral packages
// (pkg/proxyurl and pkg/cfcookies). This test prevents the same regression,
// mirroring the account layer's arch_boundary_test.
func TestInfraLayerNeverImportsApplication(t *testing.T) {
	root := "." // walk the whole infra tree from this package's directory (../../infra via parent would also work)
	if _, statErr := os.Stat(root); statErr != nil {
		t.Skip("running from unexpected working directory")
	}
	forbiddenPrefix := "github.com/chenyme/grok2api/backend/internal/application/"
	quote := string(rune(34))
	var violations []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			return walkErr
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if parseErr != nil {
			t.Errorf("parse %s: %v", path, parseErr)
			return nil
		}
		for _, imported := range file.Imports {
			importPath := strings.Trim(imported.Path.Value, quote)
			if strings.HasPrefix(importPath, forbiddenPrefix) {
				violations = append(violations, path+" imports "+importPath)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, violation := range violations {
		t.Errorf("layer violation: %s", violation)
	}
}
