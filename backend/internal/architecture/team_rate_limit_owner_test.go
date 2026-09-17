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

// Team send throttling is account policy. Gateway consumes its snapshot to
// organize attempts, without retaining a second cross-request policy store.
func TestTeamRateLimitHasOneAccountOwner(t *testing.T) {
	declarations := map[string]int{}
	calls := map[string]int{}
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
		owner := strings.HasPrefix(path, "../application/account/")
		ast.Inspect(file, func(node ast.Node) bool {
			switch value := node.(type) {
			case *ast.FuncDecl:
				switch value.Name.Name {
				case "ActiveTeamModelRateLimit", "ObserveTeamModelRateLimit":
					declarations[value.Name.Name]++
					if !owner {
						t.Errorf("%s declares team rate-limit policy outside M07", path)
					}
				case "activeTeamModelRateLimit", "markTeamModelRateLimit":
					t.Errorf("%s restores retired team rate-limit policy", path)
				case "teamModelRateLimitKey", "rateLimitTeamFingerprint", "shortTeamFingerprint", "pruneTeamModelRateLimitsLocked", "refreshTeamModelRateLimitStateLocked":
					if !owner {
						t.Errorf("%s owns team policy state outside M07", path)
					}
				}
			case *ast.TypeSpec:
				switch value.Name.Name {
				case "teamModelRateLimit", "TeamModelRateLimit", "teamRateLimitIdentity", "teamRateLimitObservation":
					if !owner {
						t.Errorf("%s declares team policy state outside M07", path)
					}
				}
			case *ast.SelectorExpr:
				switch value.Sel.Name {
				case "ActiveTeamModelRateLimit", "ObserveTeamModelRateLimit":
					calls[value.Sel.Name]++
					expected := map[string]string{"ActiveTeamModelRateLimit": "response_attempt.go", "ObserveTeamModelRateLimit": "response_failure.go"}
					if path != "../application/gateway/"+expected[value.Sel.Name] {
						t.Errorf("%s bypasses the logical request's team observation/attempt boundary", path)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ActiveTeamModelRateLimit", "ObserveTeamModelRateLimit"} {
		if declarations[name] != 1 || calls[name] != 1 {
			t.Errorf("%s owners=%d request boundaries=%d", name, declarations[name], calls[name])
		}
	}
}
