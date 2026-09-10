package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Orphan absence must be proven for the actual file keys, never inferred from
// omissions in mutable offset pages. Capacity cleanup has a different purpose.
func TestMediaOrphanAbsenceUsesCandidateLookup(t *testing.T) {
	tree, err := parser.ParseFile(token.NewFileSet(), "../application/media/orphan.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	lookup := false
	ast.Inspect(tree, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "FindMediaAssetStorageKeys":
			lookup = true
		case "ListOldestMediaAssets", "ListMediaAssets":
			t.Errorf("orphan absence inferred from mutable listing: %s", selector.Sel.Name)
		}
		return true
	})
	if !lookup {
		t.Fatal("orphan sweep lost explicit candidate reference lookup")
	}
}
