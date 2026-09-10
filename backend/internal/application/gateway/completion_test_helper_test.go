package gateway

import (
	"testing"

	"github.com/google/uuid"
)

// finishTestResult simulates the transport's already validated success boundary.
// Incomplete adapter fixtures use a synthetic ID; completion contract tests call
// CommitCompletion directly and exercise missing identity and commit failures.
func finishTestResult(t *testing.T, result *Result, usage Usage, responseID, code string) {
	t.Helper()
	if code == "" && result.CommitCompletion != nil {
		if responseID == "" {
			responseID = "test-" + uuid.NewString()
		}
		if err := result.CommitCompletion(Completion{Usage: usage, ResponseID: responseID, NativeResponseID: responseID}); err != nil {
			t.Errorf("completion barrier: %v", err)
			code = "completion_commit_failed"
		}
	}
	result.Finalize(usage, responseID, code)
}
