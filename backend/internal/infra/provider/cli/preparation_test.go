package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

// Compare the combined path to the independently used normalization/routing
// contracts, including wire bytes, compatibility metadata and rejection errors.
func TestBuildResponsesPreparationPreservesContracts(t *testing.T) {
	inputs := []string{
		`{"input":"synthetic"}`,
		`{"input":"<synthetic>&","client_metadata":{"path":"fictional"},"unknown_number":9007199254740993}`,
		`{"input":[],"tools":null,"prompt_cache_key":"body-key"}`,
		`{"input":"synthetic","tool_choice":"none"}`,
		`{"input":"synthetic","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`,
		`{"input":"synthetic","tools":[{"type":"x_search"}],"reasoning":{"effort":"high"}}`,
		`{"input":"synthetic","tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]}}]}`,
		`{"input":"synthetic","response_format":{"type":"json_object"}}`,
		`{"input":[{"role":"user","content":"synthetic"},{"type":"compaction_trigger"}]}`,
		`{"input":"synthetic","tools":42}`,
		`{"input":"synthetic","text":42,"response_format":{"type":"json_object"}}`,
		`null`, `[]`, `{`,
	}
	for index, input := range inputs {
		for _, method := range []string{http.MethodPost, http.MethodGet} {
			for _, key := range []string{"", "synthetic-session"} {
				for _, policy := range []inferencedomain.ToolCompatibilityPolicy{inferencedomain.PreserveToolDeclarations, inferencedomain.AllowDisabledCacheTools} {
					t.Run(fmt.Sprintf("input_%d/%s/key_%t/policy_%d", index, method, key != "", policy), func(t *testing.T) {
						var wantMetadata, gotMetadata provider.NormalizedRequestMetadata
						wantBody, wantCompatibility, wantErr := normalizeResponsesRequestWithMetadata([]byte(input), "grok-4.6", &wantMetadata)
						wantRoute := buildPromptCacheRoute{}
						if wantErr == nil && method == http.MethodPost && (wantCompatibility == nil || !wantCompatibility.compactionRequested) {
							wantBody, wantRoute, wantErr = prepareBuildPromptCacheRoute(wantBody, "responses", "grok-4.6", key, policy)
							if wantErr != nil {
								wantErr = fmt.Errorf("准备 Build prompt cache 路由: %w", wantErr)
							}
						}
						gotBody, gotCompatibility, gotRoute, gotErr := prepareBuildResponsesRequest([]byte(input), provider.ResponseResourceRequest{Method: method, Operation: "responses", Model: "grok-4.6", PromptCacheKey: key, ToolCompatibilityPolicy: policy, NormalizedMetadata: &gotMetadata})
						if fmt.Sprint(gotErr) != fmt.Sprint(wantErr) {
							t.Fatalf("error = %v, want %v", gotErr, wantErr)
						}
						if wantErr != nil {
							return
						}
						if !bytes.Equal(gotBody, wantBody) || !reflect.DeepEqual(gotCompatibility, wantCompatibility) || !reflect.DeepEqual(gotRoute, wantRoute) || !reflect.DeepEqual(gotMetadata, wantMetadata) {
							t.Fatalf("combined preparation changed the wire or its metadata\ngot %s\nwant %s", gotBody, wantBody)
						}
					})
				}
			}
		}
	}
}
