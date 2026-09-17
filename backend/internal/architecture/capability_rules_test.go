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

// R01 executable dependency and capability rules.
//
// These rules protect decision rights that directory-name checks cannot see:
// which mechanisms may appear in contract layers, which layers a package may
// belong to, and which modules may construct or hold other modules' services.
// Every current violation is an enumerated transitional exception bound to the
// remediation task that removes it (R02-R21); R22 verifies the tables are empty.

// contractMechanismException records one tolerated mechanism import in a
// contract layer until its owning task lands.
type contractMechanismException struct {
	path   string // repository-relative file, "/"-separated
	imp    string // exact external import path
	task   string // remediation task that must remove it
	reason string
}

var contractMechanismExceptions = []contractMechanismException{}

func TestCapabilityExceptionTablesStayEmpty(t *testing.T) {
	if n := len(contractMechanismExceptions); n != 0 {
		t.Errorf("contractMechanismExceptions has %d entries; R22 requires an empty table (additions need a bound task and a review of this assertion)", n)
	}
	if n := len(constructionExceptions); n != 0 {
		t.Errorf("constructionExceptions has %d entries; R22 requires an empty table", n)
	}
	if n := len(fieldHoldingExceptions); n != 0 {
		t.Errorf("fieldHoldingExceptions has %d entries; R22 requires an empty table", n)
	}
}

// mechanismImports are external packages that implement a mechanism a contract
// layer must only describe. Data types (e.g. net/http request facts in provider
// contracts) are judged separately by the net/http scoping below.
var mechanismImportPrefixes = []string{
	"github.com/golang-jwt/",
	"golang.org/x/crypto/bcrypt",
	"database/sql",
	"gorm.io/",
	"github.com/redis/",
	"github.com/alicebob/miniredis",
}

// driverProbeImports are driver surfaces the use-case layers must not touch:
// storage adapters classify driver faults into repository store faults at the
// SQL boundary, so application and transport never interpret drivers again.
var driverProbeImports = []string{
	"database/sql",
	"database/sql/driver",
}

// driverProbeSource reports whether a package is a use-case layer that must
// stay driver-free.
func driverProbeSource(pkg string) bool {
	is := func(root string) bool { return pkg == root || strings.HasPrefix(pkg, root+"/") }
	return is("application") || is("transport") || is("quality/court") || is("quality/management")
}

func driverImportViolation(pkg, imp string) string {
	if !driverProbeSource(pkg) {
		return ""
	}
	for _, p := range driverProbeImports {
		if imp == p || strings.HasPrefix(imp, p+"/") {
			return "storage adapters classify driver faults at the SQL boundary; use cases consume repository store faults"
		}
	}
	return ""
}

// contractLayer reports whether a package directory is a contract layer whose
// files must stay mechanism-free.
func contractLayer(pkg string) bool {
	is := func(root string) bool { return pkg == root || strings.HasPrefix(pkg, root+"/") }
	return is("port") || is("domain") || pkg == "repository" || is("repository") || pkg == "quality/model" || is("quality/model")
}

// mechanismImportViolation decides one external import of one contract-layer
// file. It returns "" when allowed.
func mechanismImportViolation(pkg, file, imp string) string {
	for _, p := range mechanismImportPrefixes {
		if imp == p || strings.HasPrefix(imp, strings.TrimSuffix(p, "/")+"/") || strings.HasPrefix(imp, p) {
			for _, ex := range contractMechanismExceptions {
				if ex.path == pkg+"/"+file && ex.imp == imp {
					return ""
				}
			}
			return "contract layers must not depend on the " + p + " mechanism"
		}
	}
	// net/http carries legitimate upstream facts in provider contracts; other
	// contract packages only get it through R04's transitional exception above.
	if imp == "net/http" && (strings.HasPrefix(pkg, "port/") && pkg != "port/provider") {
		for _, ex := range contractMechanismExceptions {
			if ex.path == pkg+"/"+file && ex.imp == imp {
				return ""
			}
		}
		return "only provider contracts may carry net/http data types; other ports must not implement transport"
	}
	return ""
}

// registeredLayers are the layer roots a package under internal/ must belong
// to. A new top-level directory fails until it is consciously assigned.
var registeredLayers = []string{
	"app", "application", "architecture", "buildinfo", "cli", "domain", "infra",
	"pkg", "port", "quality", "repository", "shared", "testsupport", "transport",
}

func layerRegistered(pkg string) bool {
	root := pkg
	if i := strings.Index(pkg, "/"); i >= 0 {
		root = pkg[:i]
	}
	for _, l := range registeredLayers {
		if root == l {
			return true
		}
	}
	return false
}

// constructionException tolerates one constructor call until its task lands.
type constructionException struct {
	source string // package directory of the caller, "/"-separated
	target string // imported internal package path relative to internal/
	symbol string // constructor name
	task   string
	reason string
}

var constructionExceptions = []constructionException{}

// perAttemptValueConstructors are context-scoped value objects created where
// attempts run, not composed services: the composition root cannot construct
// them because they exist per execution attempt. Extending this set with a
// service constructor is a rule violation by review.
var perAttemptValueConstructors = map[string]map[string]bool{
	"application/selector": {"NewAttemptResources": true, "NewAdmissionBody": true},
	"application/history":  {"NewIdentityRequest": true},
	// mediajob.Execution is a stateless command bundle (store + two pure
	// functions) built per video execution step; the App cannot pre-construct
	// it because the quota finalizer is bound to the executing service.
	"application/mediajob": {"NewExecution": true},
}

// forbiddenConstructionTargets lists, per source prefix, internal package
// prefixes whose constructors the source must not call.
var forbiddenConstructionTargets = map[string][]string{
	// HTTP encodes protocols; use cases and adapters arrive via App injection.
	"transport/": {"application/", "infra/"},
	// Logical execution must not assemble other domains' services, and the
	// physical ledger factory is injected by App: a gateway-local fallback
	// would be a second wiring path that hides a missing injection.
	"application/gateway": {"application/history", "application/media", "application/mediajob", "application/selector", "application/execution", "infra/"},
	// The catalog consumes credential capability, not the account service.
	"application/model": {"application/account"},
}

func constructionEdgeViolation(source, target, symbol string) string {
	for sp, targets := range forbiddenConstructionTargets {
		spNorm := strings.TrimSuffix(sp, "/")
		if source != spNorm && !strings.HasPrefix(source, spNorm+"/") {
			continue
		}
		for _, tp := range targets {
			tpNorm := strings.TrimSuffix(tp, "/")
			if target != tpNorm && !strings.HasPrefix(target, tpNorm+"/") {
				continue
			}
			allowed := false
			if valueSet := perAttemptValueConstructors[target]; valueSet != nil && valueSet[symbol] {
				allowed = true
			}
			for _, ex := range constructionExceptions {
				if ex.source == source && ex.target == target && ex.symbol == symbol {
					allowed = true
				}
			}
			if !allowed {
				return "construct " + symbol + " from " + target + "; composition belongs to the App root, this module must receive the collaborator"
			}
		}
	}
	return ""
}

// fieldHoldingException tolerates one full-service field until its task lands.
type fieldHoldingException struct {
	source   string
	typeName string // qualified type as written, e.g. "accountapp.Service"
	task     string
	reason   string
}

var fieldHoldingExceptions = []fieldHoldingException{}

// forbiddenFieldTypes maps source prefixes to qualified full-service types the
// source must not hold as struct fields (whole-service capability widening).
var forbiddenFieldTypes = map[string][]string{
	"application/gateway":  {"accountapp.Service", "historyapp.Service", "mediaapp.Service", "mediajobapp.Service", "selectorapp.Service", "clientkeyapp.Service", "auditapp.Service", "egressapp.Service", "modelapp.Service"},
	"application/model":    {"accountapp.Service"},
	"application/mediajob": {"gatewayapp.Service", "selectorapp.Service"},
}

func fieldHoldingViolation(source, qualified string) string {
	for sp, types := range forbiddenFieldTypes {
		if source == sp || strings.HasPrefix(source, sp+"/") {
			for _, ty := range types {
				if qualified == ty || strings.HasSuffix(qualified, "."+ty) {
					for _, ex := range fieldHoldingExceptions {
						if ex.source == source && (ex.typeName == qualified || strings.HasSuffix(qualified, "."+ex.typeName)) {
							return ""
						}
					}
					return "holds full service " + qualified
				}
			}
		}
	}
	return ""
}

// bigConsumerInterfaces are repository aggregates a consumer interface must
// not embed (capability widening by embedding).
var bigConsumerInterfaces = []string{
	"AccountRepository", "ModelRepository", "MediaJobRepository", "MediaAssetRepository",
	"MediaUploadTicketRepository", "ClientKeyRepository", "AuditRepository",
}

// interfaceEmbedViolation reports interfaces that widen capability by embedding
// a repository aggregate. Declared inside repository or persistence itself it
// is composition; elsewhere it is a violation.
func interfaceEmbedViolation(pkg, file, name string) string {
	is := func(root string) bool { return pkg == root || strings.HasPrefix(pkg, root+"/") }
	if is("repository") || is("infra/persistence") || is("architecture") || is("testsupport") || strings.HasSuffix(file, "_test.go") {
		return ""
	}
	for _, big := range bigConsumerInterfaces {
		if name == big {
			return "embeds aggregate " + name
		}
	}
	return ""
}

func TestContractLayersExcludeMechanisms(t *testing.T) {
	root := ".."
	var checked int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		if !contractLayer(pkg) {
			return nil
		}
		fset := token.NewFileSet()
		fileAst, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range fileAst.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(name, internalPrefix) {
				continue
			}
			checked++
			if reason := mechanismImportViolation(pkg, filepath.Base(rel), name); reason != "" {
				t.Errorf("%s imports %s: %s", rel, name, reason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no external contract-layer imports inspected")
	}
}

func TestUseCaseLayersStayDriverFree(t *testing.T) {
	root := ".."
	var checked int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		fset := token.NewFileSet()
		fileAst, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imported := range fileAst.Imports {
			name, err := strconv.Unquote(imported.Path.Value)
			if err != nil {
				return err
			}
			if strings.HasPrefix(name, internalPrefix) {
				continue
			}
			checked++
			if reason := driverImportViolation(pkg, name); reason != "" {
				t.Errorf("%s imports %s: %s", rel, name, reason)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked == 0 {
		t.Fatal("no external use-case imports inspected")
	}
}

func TestInternalPackagesBelongToRegisteredLayers(t *testing.T) {
	root := ".."
	seen := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, err := filepath.Rel(root, filepath.Dir(path))
		if err != nil {
			return err
		}
		pkg := filepath.ToSlash(rel)
		if seen[pkg] {
			return nil
		}
		seen[pkg] = true
		if !layerRegistered(pkg) {
			t.Errorf("package %s does not belong to a registered layer; assign it consciously or move it", pkg)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) < 10 {
		t.Fatalf("too few packages inspected: %d", len(seen))
	}
}

func TestCrossModuleConstructionAndHolding(t *testing.T) {
	root := ".."
	var checked int
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		pkg := filepath.ToSlash(filepath.Dir(rel))
		fset := token.NewFileSet()
		fileAst, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}

		// Map local import names to internal target packages.
		imports := map[string]string{}
		for _, imp := range fileAst.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil || !strings.HasPrefix(name, internalPrefix) {
				continue
			}
			local := ""
			if imp.Name != nil {
				local = imp.Name.Name
			} else {
				local = name[strings.LastIndex(name, "/")+1:]
			}
			imports[local] = strings.TrimPrefix(name, internalPrefix)
		}

		ast.Inspect(fileAst, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CallExpr:
				sel, ok := node.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				id, ok := sel.X.(*ast.Ident)
				if !ok {
					return true
				}
				target, ok := imports[id.Name]
				if !ok || !strings.HasPrefix(sel.Sel.Name, "New") {
					return true
				}
				checked++
				if reason := constructionEdgeViolation(pkg, target, sel.Sel.Name); reason != "" {
					t.Errorf("%s: %s", rel, reason)
				}
			case *ast.Field:
				// struct fields holding a full foreign service type
				if node.Names == nil {
					return true
				}
				var qual string
				switch ty := node.Type.(type) {
				case *ast.StarExpr:
					if sel, ok := ty.X.(*ast.SelectorExpr); ok {
						if id, ok := sel.X.(*ast.Ident); ok {
							qual = id.Name + "." + sel.Sel.Name
						}
					}
				case *ast.SelectorExpr:
					if id, ok := ty.X.(*ast.Ident); ok {
						qual = id.Name + "." + ty.Sel.Name
					}
				}
				if qual != "" {
					if reason := fieldHoldingViolation(pkg, qual); reason != "" {
						t.Errorf("%s: %s", rel, reason)
					}
				}
			case *ast.InterfaceType:
				for _, field := range node.Methods.List {
					if len(field.Names) == 0 {
						if id, ok := field.Type.(*ast.Ident); ok {
							if reason := interfaceEmbedViolation(pkg, filepath.Base(rel), id.Name); reason != "" {
								t.Errorf("%s: interface %s", rel, reason)
							}
						}
						if sel, ok := field.Type.(*ast.SelectorExpr); ok {
							if reason := interfaceEmbedViolation(pkg, filepath.Base(rel), sel.Sel.Name); reason != "" {
								t.Errorf("%s: interface %s", rel, reason)
							}
						}
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
	if checked == 0 {
		t.Fatal("no constructor calls inspected")
	}
}

// TestCapabilityRulesRejectCounterExamples proves the rules above fail on
// adversarial inputs instead of only pattern-matching today's tree: aliased
// imports, dot imports, subpackage detours, re-exports by embedding, and
// unknown layers.
func TestCapabilityRulesRejectCounterExamples(t *testing.T) {
	cases := []struct {
		name string
		got  string // "" means the rule wrongly allowed it
	}{
		{"jwt aliased into port", mechanismImportViolation("port/crypto", "evil.go", "github.com/golang-jwt/jwt/v5")},
		{"jwt via renamed module path", mechanismImportViolation("port/crypto", "evil.go", "github.com/golang-jwt/jwt/v5/token")},
		{"bcrypt into domain", mechanismImportViolation("domain/account", "evil.go", "golang.org/x/crypto/bcrypt")},
		{"sql driver into repository contract", mechanismImportViolation("repository", "evil.go", "database/sql")},
		{"gorm into quality model", mechanismImportViolation("quality/model", "evil.go", "gorm.io/gorm")},
		{"redis client into port", mechanismImportViolation("port/lifecycle", "evil.go", "github.com/redis/go-redis/v9")},
		{"net/http body tracking into port/physical after R04", mechanismImportViolation("port/physical", "ledger.go", "net/http")},
		{"net/http into port/crypto", mechanismImportViolation("port/crypto", "evil.go", "net/http")},
		{"exception cannot be re-targeted to another file", mechanismImportViolation("port/crypto", "other.go", "github.com/golang-jwt/jwt/v5")},
		{"unknown layer root", func() string {
			if layerRegistered("rogue") {
				return ""
			}
			return "rejected"
		}()},
		{"unknown subpackage of unknown root", func() string {
			if layerRegistered("rogue/sub") {
				return ""
			}
			return "rejected"
		}()},
		{"HTTP constructs a use case", constructionEdgeViolation("transport/http/system", "application/audit", "NewService")},
		{"HTTP update fallback construction stays forbidden", constructionEdgeViolation("transport/http/system", "application/updatecheck", "NewService")},
		{"HTTP via subpackage still constructs use case", constructionEdgeViolation("transport/http/rogue", "application/audit", "NewService")},
		{"gateway constructs history service", constructionEdgeViolation("application/gateway", "application/history", "NewService")},
		{"gateway constructs infra adapter", constructionEdgeViolation("application/gateway", "infra/persistence/relational", "NewDatabase")},
		{"value constructor set cannot admit selector service", constructionEdgeViolation("application/gateway", "application/selector", "NewSelector")},
		{"model constructs account service", constructionEdgeViolation("application/model", "application/account", "NewService")},
		{"exception cannot widen to another symbol", constructionEdgeViolation("transport/http/system", "application/updatecheck", "NewOther")},
		{"gateway holds full account service", fieldHoldingViolation("application/gateway", "accountapp.Service")},
		{"gateway holds full audit service", fieldHoldingViolation("application/gateway", "auditapp.Service")},
		{"mediajob holds gateway service", fieldHoldingViolation("application/mediajob", "gatewayapp.Service")},
		{"mediajob holds selector service", fieldHoldingViolation("application/mediajob", "selectorapp.Service")},
		{"consumer embeds big repository", interfaceEmbedViolation("application/rogue", "evil.go", "AccountRepository")},
		{"consumer embeds media job repository", interfaceEmbedViolation("application/media", "evil.go", "MediaJobRepository")},
		{"use case imports sql driver", driverImportViolation("application/selector", "database/sql")},
		{"use case imports sql driver subpackage", driverImportViolation("application/selector/sub", "database/sql/driver")},
		{"transport imports sql driver", driverImportViolation("transport/http/system", "database/sql")},
		{"quality court imports sql driver", driverImportViolation("quality/court", "database/sql/driver")},
	}
	for _, tc := range cases {
		if tc.got == "" {
			t.Errorf("counter-example %q was allowed by the rules", tc.name)
		}
	}

	// Positive controls: the currently enumerated exceptions and legitimate
	// uses must keep passing so the rules cannot silently over-forbid.
	positives := []struct {
		name string
		got  string
	}{
		{"provider contracts may carry http facts", mechanismImportViolation("port/provider", "dto.go", "net/http")},
		{"app layer is registered", func() string {
			if layerRegistered("app") {
				return ""
			}
			return "rejected"
		}()},
		{"new subpackage of registered layer", func() string {
			if layerRegistered("application/newdomain") {
				return ""
			}
			return "rejected"
		}()},
		{"gateway keeps domain value constructor", constructionEdgeViolation("application/gateway", "domain/inference", "NewAttemptBudget")},
		{"repository composes its own aggregates", interfaceEmbedViolation("repository", "account.go", "AccountRepository")},
	}
	for _, tc := range positives {
		if tc.got != "" {
			t.Errorf("positive control %q was rejected: %s", tc.name, tc.got)
		}
	}
}
