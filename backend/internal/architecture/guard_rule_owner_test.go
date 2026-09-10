package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

func TestGuardExplanationAndVersionUseAdmissionOwner(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "../application/gateway/quality_retry.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found, judge := false, false
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "qualityHoldRule" {
			continue
		}
		found = true
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.CallExpr:
				if sel, ok := value.Fun.(*ast.SelectorExpr); ok {
					if owner, ok := sel.X.(*ast.Ident); ok && owner.Name == "qualityguard" && sel.Sel.Name == "Judge" {
						judge = true
					}
				}
			case *ast.BasicLit:
				if value.Kind == token.STRING {
					literal, _ := strconv.Unquote(value.Value)
					switch literal {
					case "thinking", "item_done", "outrun", "terminal", "wait":
						t.Errorf("gateway independently explains admission rule %q", literal)
					}
				}
			}
			return true
		})
	}
	if !found || !judge {
		t.Fatal("quality fingerprint must consume the admission rule explanation")
	}
	file, err = parser.ParseFile(token.NewFileSet(), "../app/quality_guard.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	version := false
	ast.Inspect(file, func(node ast.Node) bool {
		kv, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		name, ok := kv.Key.(*ast.Ident)
		if !ok || name.Name != "RuleVersion" {
			return true
		}
		sel, ok := kv.Value.(*ast.SelectorExpr)
		if ok {
			owner, ok := sel.X.(*ast.Ident)
			version = ok && owner.Name == "qualityguard" && sel.Sel.Name == "RuleVersion"
		}
		if !version {
			t.Error("application must bind the admission owner's rule version")
		}
		return true
	})
	if !version {
		t.Fatal("missing actual rule-version consumer")
	}
}
