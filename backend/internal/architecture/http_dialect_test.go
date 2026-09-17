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

func TestHTTPInferenceAccountAvoidsUpstreamDialectTypes(t *testing.T) {
	banned := map[string]struct{}{
		"VideoOperation": {}, "VideoOperationGenerate": {}, "VideoOperationEdit": {}, "VideoOperationExtend": {},
		"ThinkingEvidenceComment": {}, "TTSOutputFormat": {}, "TTSRequest": {},
	}
	roots := []string{"../transport/http/inference", "../transport/http/account"}
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			providerAlias := map[string]bool{}
			for _, imp := range file.Imports {
				name, _ := strconvUnquote(imp.Path.Value)
				if strings.HasSuffix(name, "/port/provider") || strings.HasSuffix(name, "/infra/provider") || strings.Contains(name, "/infra/provider/") {
					alias := "provider"
					if imp.Name != nil {
						alias = imp.Name.Name
					}
					providerAlias[alias] = true
				}
			}
			ast.Inspect(file, func(node ast.Node) bool {
				sel, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				ident, ok := sel.X.(*ast.Ident)
				if !ok || !providerAlias[ident.Name] {
					return true
				}
				if _, hit := banned[sel.Sel.Name]; hit {
					t.Errorf("%s uses upstream dialect %s.%s", path, ident.Name, sel.Sel.Name)
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func strconvUnquote(s string) (string, error) {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1], nil
	}
	return s, nil
}
