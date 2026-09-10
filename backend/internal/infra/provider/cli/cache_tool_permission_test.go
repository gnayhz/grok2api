package cli

import (
	"encoding/json"
	inferencedomain "github.com/chenyme/grok2api/backend/internal/domain/inference"
	"testing"
)

func TestCacheRoutePreservesCallableToolPermission(t *testing.T) {
	for _, kind := range []string{"web_search", "function", "custom"} {
		for _, choice := range []string{`null`, `"auto"`, `"required"`, `{"type":"function","name":"lookup"}`, `{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"lookup"}]}`} {
			for _, compatibilityPolicy := range []inferencedomain.ToolCompatibilityPolicy{inferencedomain.PreserveToolDeclarations, inferencedomain.AllowDisabledCacheTools} {
				t.Run(kind+"/"+choice+"/policy="+string(mustJSON(compatibilityPolicy)), func(t *testing.T) {
					original := []byte(`{"input":"hello","tools":[{"type":"` + kind + `","name":"lookup"}],"tool_choice":` + choice + `}`)
					body, route, err := prepareBuildPromptCacheRoute(original, "responses", "grok-4.6", "cache-session", compatibilityPolicy)
					if err != nil {
						t.Fatal(err)
					}
					var before, after map[string]json.RawMessage
					_ = json.Unmarshal(original, &before)
					_ = json.Unmarshal(body, &after)
					if string(before["tools"]) != string(after["tools"]) || string(before["tool_choice"]) != string(after["tool_choice"]) || len(route.injectedToolTypes) != 0 {
						t.Fatalf("cache routing changed callable tools: %s", body)
					}
				})
			}
		}
	}
}

func TestCacheRouteRequiresExplicitPolicyAndDoesNotChangeForcedChoices(t *testing.T) {
	for _, policy := range []inferencedomain.ToolCompatibilityPolicy{inferencedomain.PreserveToolDeclarations, inferencedomain.AllowDisabledCacheTools} {
		for _, choice := range []string{`"none"`, `"auto"`, `"required"`, `{"type":"web_search"}`} {
			body, route, err := prepareBuildPromptCacheRoute([]byte(`{"input":"hello","tool_choice":`+choice+`}`), "responses", "grok-4.6", "cache", policy)
			if err != nil {
				t.Fatal(err)
			}
			wantCache := policy == inferencedomain.AllowDisabledCacheTools && (choice == `"none"` || choice == `"auto"`)
			if (len(route.plan.AddedCacheTools) > 0) != wantCache {
				t.Fatalf("policy=%d choice=%s plan=%+v", policy, choice, route.plan)
			}
			if !wantCache {
				var payload map[string]json.RawMessage
				_ = json.Unmarshal(body, &payload)
				if string(payload["tool_choice"]) != choice || len(payload["tools"]) != 0 {
					t.Fatalf("unauthorized cache declarations: %s", body)
				}
			}
		}
	}
}
