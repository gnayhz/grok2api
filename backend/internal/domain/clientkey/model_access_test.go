package clientkey

import "testing"

func TestModelAuthorizationRequiresExplicitScope(t *testing.T) {
	for _, tc := range []struct {
		name    string
		key     Key
		allowed bool
	}{
		{"unspecified", Key{}, false},
		{"invalid", Key{ModelScope: "invalid"}, false},
		{"all", Key{ModelScope: ModelScopeAll}, true},
		{"inconsistent_all", Key{ModelScope: ModelScopeAll, AllowedModels: []uint64{1}}, false},
		{"restricted_empty", Key{ModelScope: ModelScopeRestricted}, false},
		{"restricted_member", Key{ModelScope: ModelScopeRestricted, AllowedModels: []uint64{1}}, true},
		{"restricted_other", Key{ModelScope: ModelScopeRestricted, AllowedModels: []uint64{2}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.key.AllowsModel(1); got != tc.allowed {
				t.Fatalf("allows=%v want=%v", got, tc.allowed)
			}
			if tc.key.AllowsModel(0) {
				t.Fatal("zero route ID authorized")
			}
		})
	}
}
