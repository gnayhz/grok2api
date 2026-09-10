package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const internalPrefix = "github.com/chenyme/grok2api/backend/internal/"

// These rules protect boundaries that already hold in production. F12 also
// requires ownership and policy migrations; passing imports alone cannot close it.
func forbiddenDependency(source, target string) string {
	is := func(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }
	if (is(source, "quality/investigator") || is(source, "quality/events")) && (is(target, "application") || is(target, "quality/registry")) {
		return "investigation execution consumes measurement and state ports, not Gateway or persistence implementations"
	}
	if is(source, "application/history") && (is(target, "infra") || is(target, "quality") || is(target, "application") && !is(target, "application/history")) {
		return "history owns lineage and recovery proposals; upstream interpretation, execution and network policy arrive through domain contracts"
	}
	if is(source, "application/media") && (is(target, "infra") || is(target, "quality") || is(target, "application") && !is(target, "application/media")) {
		return "media owns local jobs and assets; upstream downloads and request execution remain outside its read and storage policy"
	}
	if is(target, "pkg/reasoningreplay") {
		return "history policy moved to application/history and domain/history; the old technical-package entry must not return"
	}
	if is(target, "pkg/replaypolicy") {
		return "tool side-effect and replay policy belongs to domain/inference; the old technical-package entry must not return"
	}
	if is(source, "pkg") && !is(target, "pkg") && !is(target, "domain") {
		return "technical and wire primitives may consume domain values, not services, persistence or transport implementations"
	}
	if source == "infra/provider" && !is(target, "domain") && !is(target, "pkg") {
		return "upstream contracts and registration must not construct concrete provider, network or persistence implementations"
	}
	if is(source, "application/gateway") {
		if is(target, "infra/provider") && target != "infra/provider" && target != "infra/provider/conversation" {
			return "logical execution consumes upstream contracts and shared protocol conversion, not concrete adapters"
		}
		if is(target, "quality") && target != "quality/model" && target != "quality/guard" {
			return "logical execution consumes admission and measurement contracts, not investigation or persistence services"
		}
		if is(target, "infra/persistence") || is(target, "infra/runtime") || is(target, "application/egress") {
			return "logical execution must not own storage mechanisms or network management commands"
		}
	}
	if is(source, "infra/provider/xaitools") && target != "pkg/jsonvalue" {
		return "xAI tool wire constraints must not depend on providers, history, request execution or network policy"
	}
	if is(source, "infra/config") && is(target, "quality") {
		return "file decoding consumes pure domain policy, not a quality service"
	}
	if is(source, "infra/persistence/relational") && is(target, "quality") && target != "quality/model" && target != "quality/journal" {
		return "atomic legacy migration may consume restriction policy and row schema, not quality services"
	}
	if is(source, "domain") && !is(target, "domain") {
		return "domain must not depend on application or infrastructure"
	}
	if is(source, "repository") && !is(target, "domain") && !is(target, "repository") {
		return "storage ports must depend only on domain and other ports"
	}
	if (is(source, "quality/management") || is(source, "quality/guard") || is(source, "quality/investigator") || is(source, "quality/events")) && (is(target, "app") || is(target, "transport") || is(target, "infra") || is(target, "application/egress")) {
		return "quality configuration owns policy via storage ports; it cannot encode HTTP, SQL or network capacity"
	}
	if is(source, "application") && (is(target, "app") || is(target, "transport")) {
		return "application must not depend on composition or HTTP"
	}
	if is(source, "application/account") || is(source, "application/history") {
		if is(target, "infra/egress") || is(target, "application/egress") {
			return "account and conversation policy must not control network implementation"
		}
	}
	if is(source, "infra/provider") || is(source, "infra/egress") {
		if is(target, "application") || is(target, "app") || is(target, "transport") || is(target, "quality") {
			return "provider and network implementations must use injected upstream policy ports"
		}
	}
	if is(source, "infra/egress") && (is(target, "infra/provider") || is(target, "application/history")) {
		return "network must not interpret provider or conversation policy"
	}
	return ""
}

func TestProductionDependencyBoundaries(t *testing.T) {
	root := ".."
	checked := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		source, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		for _, imported := range file.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if filepath.ToSlash(source) == "app" && (strings.HasPrefix(name, "gorm.io/") || name == "database/sql") {
				t.Errorf("%s: composition must construct SQL owners through their constructors", path)
			}
			if (filepath.ToSlash(source) == "quality/management" || filepath.ToSlash(source) == "quality/guard" || filepath.ToSlash(source) == "quality/investigator" || filepath.ToSlash(source) == "quality/events") && strings.HasPrefix(name, "gorm.io/") {
				t.Errorf("%s: quality management must use a storage port, not GORM", path)
			}
			if !strings.HasPrefix(name, internalPrefix) {
				continue
			}
			checked++
			target := strings.TrimPrefix(name, internalPrefix)
			if reason := forbiddenDependency(filepath.ToSlash(source), target); reason != "" {
				t.Errorf("%s imports %s: %s", path, target, reason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no internal production dependencies inspected")
	}
	t.Logf("inspected %d internal production imports", checked)
}

func TestBoundaryRulesRejectCrossModuleOwnership(t *testing.T) {
	for _, edge := range [][2]string{
		{"domain/account", "infra/security"},
		{"infra/config", "quality/guard"},
		{"infra/provider/xaitools", "infra/provider/cli"},
		{"infra/provider/xaitools", "application/history"},
		{"quality/management", "transport/http/quality"},
		{"quality/guard", "app"},
		{"quality/guard", "infra/persistence/relational"},
		{"quality/management", "application/egress"},
		{"quality/management", "infra/persistence/relational"},
		{"repository", "application/account"},
		{"application/settings", "app"},
		{"application/gateway", "transport/http/inference"},
		{"application/account", "infra/egress"},
		{"application/history", "application/egress"},
		{"application/history", "infra/provider/cli"},
		{"application/history", "application/gateway"},
		{"application/media", "application/gateway"},
		{"application/media", "infra/provider/cli"},
		{"application/media", "infra/persistence/relational"},
		{"application/gateway", "pkg/reasoningreplay"},
		{"application/gateway", "pkg/replaypolicy"},
		{"infra/provider/cli", "pkg/replaypolicy"},
		{"pkg/attemptmeta", "application/gateway"},
		{"pkg/responsecheck", "quality/guard"},
		{"infra/provider", "infra/provider/cli"},
		{"infra/provider", "infra/egress"},
		{"application/gateway", "infra/provider/console"},
		{"application/gateway", "quality/registry"},
		{"application/gateway", "quality/investigator"},
		{"application/gateway", "application/egress"},
		{"application/gateway", "infra/persistence/relational"},
		{"application/gateway", "infra/runtime/redis"},
		{"infra/provider/cli", "quality/registry"},
		{"infra/egress", "infra/provider/cli"},
		{"infra/egress", "application/history"},
	} {
		if forbiddenDependency(edge[0], edge[1]) == "" {
			t.Errorf("undetected forbidden dependency: %v", edge)
		}
	}
}
