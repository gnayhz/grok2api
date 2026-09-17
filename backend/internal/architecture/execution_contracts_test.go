package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Gateway passes explicit execution constraints and reads network receipts. It
// cannot construct a Manager, change pools or install admission configuration.
// Keep the accepted contract finite even though these types share a package
// with implementations used by composition and provider adapters.
func TestGatewayConsumesExecutionContracts(t *testing.T) {
	contracts := map[string]map[string]bool{
		"port/physical": {
			"Trace": true, "WithTrace": true,
			"WithNodeExclusions": true, "WithQualityVerificationNode": true,
			"ErrClientRetired": true, "ErrPhysicalCallLimit": true,
			"IsPhysicalCallAdmissionError": true, "MaxPhysicalCalls": true,
			"WithPhysicalCallTrace": true, "WithPhysicalCallBudget": true,
			"WithPhysicalCallPermit": true, "WithPhysicalCallStage": true,
			"WithPhysicalCallBatches": true, "PhysicalCallCount": true,
			"PhysicalFacts": true, "PhysicalObservations": true,
			"ConfirmPhysicalFacts": true, "ObservePhysicalGeneration": true,
			"ObservePhysicalPayload": true, "ObservePhysicalUsage": true,
			"JournalFactory": true, "ObserveCanonicalPhysicalUsage": true,
		},
		"quality/guard": {"Judge": true, "Signals": true},
	}
	err := filepath.WalkDir("../application/gateway", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		imports := map[string]string{}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			name = strings.TrimPrefix(name, internalPrefix)
			if _, ok := contracts[name]; !ok {
				continue
			}
			alias := filepath.Base(name)
			if spec.Name != nil {
				alias = spec.Name.Name
			}
			if alias == "." || alias == "_" {
				t.Errorf("%s: execution contracts require an explicit package reference", path)
			}
			imports[alias] = name
		}
		ast.Inspect(file, func(node ast.Node) bool {
			selector, ok := node.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := selector.X.(*ast.Ident)
			if !ok {
				return true
			}
			if owner := imports[ident.Name]; owner != "" && !contracts[owner][selector.Sel.Name] {
				t.Errorf("%s: %s.%s is outside the execution contract", path, owner, selector.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
