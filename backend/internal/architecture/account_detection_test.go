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

func TestAccountDetectionConsumesProviderCompletionFacts(t *testing.T) {
	inspections := 0
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		path = filepath.ToSlash(path)
		ast.Inspect(file, func(n ast.Node) bool {
			if f, ok := n.(*ast.FuncDecl); ok && f.Name.Name == "readDetectBodyForClassification" {
				t.Errorf("%s restores swallowing detector body failures", path)
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if selector.Sel.Name == "InspectResponsesProbe" {
				inspections++
				if path != "../application/account/build_detect.go" {
					t.Errorf("%s bypasses account detection use case", path)
				}
			}
			if path == "../application/account/build_detect.go" && selector.Sel.Name == "MarkReauthRequired" {
				t.Error("detection discards the credential transition outcome")
			}
			if path == "../application/account/build_detect.go" && (selector.Sel.Name == "JSONGeneration" || selector.Sel.Name == "JSON" || selector.Sel.Name == "RawValue") {
				t.Error("M07 detection reinterprets native completion outside Provider")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspections != 1 {
		t.Fatalf("detector completion consumers=%d", inspections)
	}
}
