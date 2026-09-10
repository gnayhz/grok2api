package history

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestReplayScopePreservesG30MigrationKeys(t *testing.T) {
	data, err := os.ReadFile("testdata/g31-scope-golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err = json.Unmarshal(data, &want); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for _, id := range []uint64{0, 12, 19} {
		for _, plane := range []ReplayPlane{ReplayPlaneBuild, ReplayPlaneXAI} {
			for _, seed := range []string{"session-1", "  session-1  ", "", "  "} {
				out[string(plane)+":"+string(rune('a'+id))+":"+seed] = map[string]any{"key": ReplayScope(seed, id, plane), "legacy": LegacyReplayScopes(seed, id, plane, []uint64{0, 12, 19, 12})}
			}
		}
	}
	encoded, _ := json.Marshal(out)
	var got map[string]any
	_ = json.Unmarshal(encoded, &got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope encoding changed: got=%s want=%s", encoded, data)
	}
	if ReplayScope("explicit", 12, "unknown") != "" || LegacyReplayScopes("explicit", 12, "unknown", []uint64{19}) != nil {
		t.Fatal("unknown plane authorized history")
	}
}
