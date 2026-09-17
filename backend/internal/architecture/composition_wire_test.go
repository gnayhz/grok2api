package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestCompositionRootWiresWithoutOwningPolicy(t *testing.T) {
	files, err := filepath.Glob("../app/wire_*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 2 {
		t.Fatalf("wire files = %v", files)
	}
	banned := map[string]struct{}{
		"decideQualityRetry": {}, "IsWebChatQuotaMode": {}, "TransitionQuotaRecovery": {},
		"AccountSchedulable": {}, "ClassifyQualityHold": {},
	}
	for _, path := range files {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if fn, ok := node.(*ast.FuncDecl); ok && fn.Name != nil {
				if _, hit := banned[fn.Name.Name]; hit {
					t.Errorf("%s owns policy %s", path, fn.Name.Name)
				}
			}
			return true
		})
	}
}
