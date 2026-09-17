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

func TestAccountCredentialWritesHaveObservedMaterial(t *testing.T) {
	applies, transitions, imports, importChecks, identities, identityTransitions, profiles, profileTransitions := 0, 0, 0, 0, 0, 0, 0, 0
	conversionLinks, conversionChecks, deviceClaims, deviceFinishes, deviceClaimPolicies, deviceFinishPolicies := 0, 0, 0, 0, 0, 0
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
		if strings.HasPrefix(path, "../application/account/") || strings.HasPrefix(path, "../application/accountsync/") {
			for _, imported := range file.Imports {
				if imported.Path.Value == `"golang.org/x/sync/singleflight"` {
					t.Errorf("%s restores shared account waiting without a caller cancellation contract", path)
				}
			}
		}
		banned := func(name string) {
			switch name {
			case "UpdateTokens", "UpdateCredentialRefreshFailure", "preserveConcurrentRefreshWrites", "preserveAccountHealth", "WriteTombstones", "writeTombstonesFor", "tombstoneCandidates", "UpdateIdentityMetadata", "MarkWebNSFWEnabled", "MarkWebTermsAccepted", "MarkWebBirthDateSet", "markWebProfileTimestamp", "readDetectBodyForClassification":
				t.Errorf("%s restores removed credential write %s", path, name)
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch v := node.(type) {
			case *ast.FuncDecl:
				banned(v.Name.Name)
				if path == "../infra/persistence/relational/account_repository.go" && v.Name.Name == "Update" {
					t.Error("whole-account Save returned")
				}
			case *ast.CallExpr:
				sel, ok := v.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				banned(sel.Sel.Name)
				switch sel.Sel.Name {
				case "LinkWebToBuild":
					conversionLinks++
					if path != "../application/account/service.go" && path != "../application/account/conversion.go" {
						t.Errorf("%s bypasses the conversion completion use case", path)
					}
				case "MatchWebBuildConversion":
					conversionChecks++
					if path != "../infra/persistence/relational/account_links.go" {
						t.Errorf("%s checks conversion outside the account transaction", path)
					}
				case "ClaimPoll", "FinishPoll":
					if sel.Sel.Name == "ClaimPoll" {
						deviceClaims++
					} else {
						deviceFinishes++
					}
					if path != "../application/account/device.go" {
						t.Errorf("%s bypasses device authorization ownership", path)
					}
				case "ClaimDevicePoll", "CompleteDevicePoll":
					if sel.Sel.Name == "ClaimDevicePoll" {
						deviceClaimPolicies++
					} else {
						deviceFinishPolicies++
					}
					if path != "../infra/runtime/memory/store.go" && path != "../infra/runtime/redis/store.go" {
						t.Errorf("%s applies device policy outside atomic runtime storage", path)
					}
				case "ApplyWebProfile":
					profiles++
					if path != "../application/account/web_account_settings.go" {
						t.Errorf("%s bypasses Web profile completion use case", path)
					}
				case "TransitionWebProfile", "WebProfileIdentityChanged":
					profileTransitions++
					if path != "../infra/persistence/relational/account_web_profile.go" {
						t.Errorf("%s applies Web profile policy outside the account transaction", path)
					}
				case "ApplyIdentity":
					identities++
					if path != "../application/account/provider_links.go" {
						t.Errorf("%s bypasses M07 identity observation use case", path)
					}
				case "TransitionIdentity":
					identityTransitions++
					if path != "../infra/persistence/relational/account_links.go" {
						t.Errorf("%s applies identity outside the locked account transaction", path)
					}
				case "CheckImportIdentity":
					importChecks++
					if path != "../infra/persistence/relational/account_import.go" {
						t.Errorf("%s checks import identity outside the write transaction", path)
					}
				case "ImportAccounts":
					if path != "../application/account/service.go" && path != "../application/account/import.go" && path != "../infra/persistence/relational/account_repository.go" {
						t.Errorf("%s installs account material outside M07 import policy", path)
					}
					if path == "../application/account/service.go" || path == "../application/account/import.go" {
						imports++
					}
				case "UpsertByIdentity", "UpsertManyByIdentity":
					if path != "../infra/persistence/relational/account_repository.go" {
						t.Errorf("%s restores a separate account import consumer", path)
					}
				case "ApplyCredential":
					applies++
					if path != "../application/account/service.go" {
						t.Errorf("%s bypasses M07 credential use case", path)
					}
				case "TransitionCredential":
					transitions++
					if path != "../infra/persistence/relational/account_credential.go" {
						t.Errorf("%s computes credential mutation outside locked persistence", path)
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
	if applies != 3 || transitions != 1 || imports != 3 || importChecks != 1 {
		t.Fatalf("credential owner coverage applies=%d transitions=%d imports=%d importChecks=%d", applies, transitions, imports, importChecks)
	}
	if conversionLinks != 1 || conversionChecks != 1 || deviceClaims != 1 || deviceFinishes != 1 || deviceClaimPolicies != 2 || deviceFinishPolicies != 2 {
		t.Fatalf("conversion/device owner coverage links=%d checks=%d claims=%d finishes=%d claimPolicies=%d finishPolicies=%d", conversionLinks, conversionChecks, deviceClaims, deviceFinishes, deviceClaimPolicies, deviceFinishPolicies)
	}
	if profiles != 1 || profileTransitions != 2 {
		t.Fatalf("Web profile owner coverage applies=%d policies=%d", profiles, profileTransitions)
	}
	if identities != 1 || identityTransitions != 1 {
		t.Fatalf("identity owner coverage applies=%d transitions=%d", identities, identityTransitions)
	}
}
