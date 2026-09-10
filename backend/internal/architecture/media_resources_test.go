package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Public video reads consume M17's policy; the worker's separate repository
// operations remain claim/checkpoint writes and recovery, not a second GET path.
func TestVideoPublicResourceReadOwnership(t *testing.T) {
	tree, err := parser.ParseFile(token.NewFileSet(), "../application/gateway/video.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]int{}
	for _, decl := range tree.Decls {
		f, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if f.Name.Name == "getVideoJob" {
			t.Error("legacy Gateway resource lookup returned")
		}
		if f.Name.Name != "GetVideo" && f.Name.Name != "OpenVideoContent" {
			continue
		}
		calls[f.Name.Name] = map[string]int{}
		ast.Inspect(f.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name == "GetMediaJob" || sel.Sel.Name == "OpenVideo" {
				t.Errorf("%s bypasses M17 resource policy through %s", f.Name, sel.Sel)
			}
			if receiver, ok := sel.X.(*ast.SelectorExpr); ok && receiver.Sel.Name == "videoResources" {
				calls[f.Name.Name][sel.Sel.Name]++
			}
			return true
		})
	}
	if calls["GetVideo"]["Get"] != 1 || calls["OpenVideoContent"]["Lookup"] != 1 || calls["OpenVideoContent"]["OpenLocal"] != 1 {
		t.Fatalf("public resources must use M17 query/projection/local read: %v", calls)
	}
}
