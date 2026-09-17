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

func TestSelectorDoesNotCompleteMediaJobs(t *testing.T) {
	banned := map[string]struct{}{
		"CreateVideo": {}, "finishVideoQuota": {}, "checkpointVideo": {},
		"executeVoice": {}, "GenerateImage": {}, "RunVideoWorkers": {},
		"failVideoJob": {}, "finishImageQuota": {},
	}
	err := filepath.WalkDir("../application/selector", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.Ident:
				if _, ok := banned[v.Name]; ok {
					t.Errorf("%s calls media job completion %s", path, v.Name)
				}
			case *ast.SelectorExpr:
				if _, ok := banned[v.Sel.Name]; ok {
					t.Errorf("%s calls media job completion %s", path, v.Sel.Name)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGatewayDoesNotDeclareSelector(t *testing.T) {
	err := filepath.WalkDir("../application/gateway", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if ok && ts.Name.Name == "Selector" {
					if _, isStruct := ts.Type.(*ast.StructType); isStruct {
						t.Errorf("%s still owns selector implementation", path)
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
