package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicModelPolicyHasOneApplicationOwner(t *testing.T) {
	consumers := map[string]int{}
	obsolete := map[string]bool{"ModelAliasAdapter": true, "ResolveModelAlias": true, "ModelAliases": true, "appendReasoningModelAliases": true, "filterModelRoutesForClientKey": true, "ListEnabledForClientKey": true, "grokCapabilities": true}
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		path = filepath.ToSlash(path)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && obsolete[ident.Name] {
				t.Errorf("%s retains obsolete public policy %s", path, ident.Name)
			}
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			name := selector.Sel.Name
			if name == "ResolvePublicRoutes" {
				consumers[path]++
				if path != "../application/gateway/service.go" {
					t.Errorf("unexpected public route policy consumer: %s", path)
				}
			}
			if name == "ResolveCompatibilityAlias" && path != "../application/model/resolution.go" {
				t.Errorf("%s resolves hidden public aliases outside M05", path)
			}
			if name == "PublicReasoningAliasNames" && path != "../application/model/discovery.go" {
				t.Errorf("%s publishes effort aliases outside M05", path)
			}
			if strings.HasPrefix(path, "../transport/http/inference/") {
				switch name {
				case "ParseReasoningModelAlias", "SupportsReasoningForProvider", "DefaultReasoningEffortForProvider", "SupportedReasoningEffortsForProvider", "ReasoningAliasPublicIDsForProvider", "AllowsModel":
					t.Errorf("%s derives public product policy in HTTP: %s", path, name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if consumers["../application/gateway/service.go"] != 1 {
		t.Fatalf("Gateway must consume M05 public resolution once: %v", consumers)
	}
}
