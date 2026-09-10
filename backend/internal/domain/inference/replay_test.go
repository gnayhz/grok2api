package inference

import "testing"

func TestReplayCapabilityBoundary(t *testing.T) {
	for _, test := range []struct {
		body        string
		safe, tools bool
	}{
		{`{}`, true, false},
		{`{"tools":[{"type":"function","name":"save"}]}`, true, true},
		{`{"tools":[{"name":"save","input_schema":{"type":"object"}}]}`, true, true},
		{`{"tools":[{"type":"web_search"},{"type":"x_search"}]}`, true, true},
		{`{"tools":[{"type":"mcp"}]}`, false, true},
		{`{"tools":[{"type":"computer"}]}`, false, true},
		{`{"tools":[{"type":"future_tool"}]}`, false, true},
		{`{"tools":[{}]}`, false, true},
		{`{"tools":[{"type":"mcp"}],"tool_choice":"none"}`, true, false},
		{`{"tools":[{"type":"web_search"},{"type":"mcp"}]}`, false, true},
		{`[]`, false, false}, {`null`, false, false}, {`broken`, false, false},
	} {
		got := ReplayPolicyFromRequest([]byte(test.body))
		if got.Safe != test.safe || got.Tools != test.tools {
			t.Fatalf("%s: %+v", test.body, got)
		}
	}
}
