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

func qualityApplication(path string) bool {
	for _, root := range []string{"quality/court", "quality/enforcement", "quality/management", "quality/investigator", "quality/events", "quality/guard"} {
		if path == root || strings.HasPrefix(path, root+"/") {
			return true
		}
	}
	return false
}

// Import rules protect inward dependencies. Capability and conditional-write
// tests separately enforce ownership; package names alone do not establish it.
func forbiddenDependency(source, target string) string {
	is := func(path, root string) bool { return path == root || strings.HasPrefix(path, root+"/") }
	if (is(source, "application") || is(source, "transport")) && is(target, "infra") {
		return "use cases and HTTP depend on inward contracts; app injects infrastructure implementations"
	}
	qualityUseCase := qualityApplication(source)
	if qualityUseCase && (is(target, "quality/registry") || is(target, "quality/evidence") || is(target, "quality/journal") || is(target, "infra") || is(target, "app") || is(target, "transport")) {
		return "quality use cases consume model values and owned ports; storage is assembled in app"
	}
	if (is(source, "quality/registry") || is(source, "quality/evidence") || is(source, "quality/journal")) && (qualityApplication(target) || is(target, "application") || is(target, "app") || is(target, "transport")) {
		return "quality storage implements atomic commands; it cannot orchestrate business use cases"
	}

	if is(source, "quality/model") && !is(target, "quality/model") && !is(target, "domain") && !is(target, "pkg/attemptmeta") {
		return "quality facts and deterministic rules cannot depend on services or storage"
	}

	if (is(source, "quality/investigator") || is(source, "quality/events") || is(source, "quality/court") || is(source, "quality/enforcement")) && is(target, "application") {
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
	if source == "infra/provider" && !is(target, "domain") && !is(target, "pkg") && !is(target, "port") {
		return "upstream contracts and registration must not construct concrete provider, network or persistence implementations"
	}
	if is(source, "port") && !is(target, "port") && !is(target, "domain") && !is(target, "pkg") {
		return "inward ports may consume domain values and technical primitives, not services or implementations"
	}
	if is(source, "application/mediajob") && (is(target, "application/gateway") || is(target, "application/selector")) {
		return "mediajob must not depend on gateway orchestration or selector"
	}
	if is(source, "application/selector") && is(target, "application/mediajob") {
		return "selector must not call media jobs"
	}
	if is(source, "application/admission") && is(target, "application/gateway") {
		return "admission must not depend on gateway orchestration"
	}
	if is(source, "application/selector") && is(target, "application/gateway") {
		return "selector must not depend on gateway orchestration"
	}
	if is(source, "application/gateway") {
		if is(target, "quality") && target != "quality/model" && target != "quality/guard" {
			return "logical execution consumes admission and measurement contracts, not investigation or persistence services"
		}
		if is(target, "application/egress") {
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
	if is(source, "infra") && is(target, "application") {
		return "infrastructure implements inward ports; version and release contracts live in port/, not application/"
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
			layer := filepath.ToSlash(source)
			if (strings.HasPrefix(layer, "application/") || strings.HasPrefix(layer, "domain/") || layer == "repository" || strings.HasPrefix(layer, "repository/") || strings.HasPrefix(layer, "port/") || layer == "quality/model" || qualityApplication(layer)) && (strings.HasPrefix(name, "gorm.io/") || strings.HasPrefix(name, "github.com/gin-gonic/") || strings.HasPrefix(name, "github.com/redis/")) {
				t.Errorf("%s imports concrete framework %s", path, name)
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
		{"application/updatecheck", "infra/updatecheck"},
		{"infra/updatecheck", "application/updatecheck"},
		{"application/model", "infra/persistence/relational"},
		{"transport/http/system", "infra/updatecheck"},
		{"quality/court", "quality/registry"},
		{"quality/court", "quality/evidence"},
		{"quality/enforcement", "quality/registry"},
		{"quality/management", "quality/evidence"},
		{"quality/events", "quality/journal"},
		{"quality/model", "quality/registry"},
		{"quality/registry", "quality/court"},
		{"quality/evidence", "application/gateway"},

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
		{"application/settings", "infra/config"},
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
		{"application/gateway", "infra/provider"},
		{"application/account", "infra/provider"},
		{"application/model", "infra/provider"},
		{"application/accountsync", "infra/provider"},
		{"transport/http/inference", "infra/provider"},
		{"transport/http/inference", "infra/provider/conversation"},
		{"application/gateway", "infra/provider/conversation"},
		{"application/gateway", "infra/provider/console"},
		{"application/gateway", "quality/registry"},
		{"application/gateway", "quality/investigator"},
		{"application/gateway", "infra/egress"},
		{"application/account", "infra/security"},
		{"application/adminauth", "infra/security"},
		{"application/quotarecovery", "infra/observability"},
		{"transport/http/egress", "infra/egress"},
		{"transport/http/middleware", "infra/security"},
		{"application/mediajob", "application/gateway"},
		{"application/selector", "application/mediajob"},
		{"application/admission", "application/gateway"},
		{"application/selector", "application/gateway"},
		{"application/gateway", "application/egress"},
		{"application/gateway", "infra/persistence/relational"},
		{"application/gateway", "infra/runtime/redis"},
		{"infra/provider/cli", "quality/registry"},
		{"infra/egress", "infra/provider/cli"},
		{"infra/egress", "application/history"},
		{"port/provider", "application/gateway"},
		{"port/provider", "infra/provider/cli"},
		{"port/provider", "transport/http/inference"},
		{"domain/account", "port/provider"},
		{"pkg/attemptmeta", "port/provider"},
	} {
		if forbiddenDependency(edge[0], edge[1]) == "" {
			t.Errorf("undetected forbidden dependency: %v", edge)
		}
	}
}

func TestBoundaryRulesAllowProviderErrorPort(t *testing.T) {
	for _, edge := range [][2]string{
		{"infra/provider", "port/provider"},
		{"infra/provider/cli", "port/provider"},
		{"application/gateway", "port/provider"},
		{"application/gateway", "port/physical"},
		{"application/gateway", "application/selector"},
		{"application/gateway", "application/admission"},
		{"application/gateway", "application/mediajob"},
		{"application/mediajob", "domain/media"},
		{"application/admission", "domain/guard"},
		{"application/selector", "domain/account"},
		{"application/account", "port/crypto"},
		{"infra/security", "port/crypto"},
		{"application/quotarecovery", "port/lifecycle"},
		{"transport/http/middleware", "port/crypto"},
		{"infra/egress", "port/physical"},
		{"application/account", "port/provider"},
		{"transport/http/inference", "port/provider"},
		{"infra/provider/conversation", "port/provider"},
	} {
		if reason := forbiddenDependency(edge[0], edge[1]); reason != "" {
			t.Errorf("provider error port should be importable: %v: %s", edge, reason)
		}
	}
}
