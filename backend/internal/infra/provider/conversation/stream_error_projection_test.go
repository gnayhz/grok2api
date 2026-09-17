package conversation

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestStreamFailureProjectionIgnoresUnrelatedOutput(t *testing.T) {
	for _, operation := range []string{OperationChat, OperationMessages} {
		for _, pad := range []int{0, 70 << 10} {
			for _, withError := range []bool{false, true} {
				response := map[string]any{"status": "failed", "output": []any{map[string]any{"encrypted_content": strings.Repeat("secret-cipher", pad/13+1)}}}
				if withError {
					response["error"] = map[string]any{"code": "rate_limit_exceeded", "message": "real failure"}
				}
				// Place the real error beyond the prefix and supply a nested decoy.
				body := map[string]any{"type": "response.failed", "metadata": map[string]any{"error": map[string]any{"message": "decoy"}}, "padding": strings.Repeat("P", pad), "response": response}
				data, err := json.Marshal(body)
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				c := newStreamConverterWithBudget(&output, operation, ResponseOptions{}, nil)
				defer c.releaseResources()
				if err := c.handle("response.failed", data); err != nil {
					t.Fatal(err)
				}
				if err := c.finish(); err != nil {
					t.Fatal(err)
				}
				got := output.String()
				for _, forbidden := range []string{"decoy", "secret-cipher", "encrypted_content", "PPPP"} {
					if strings.Contains(got, forbidden) {
						t.Fatalf("%s leaked %q in error", operation, forbidden)
					}
				}
				want := "Upstream request failed"
				if withError {
					want = "real failure"
				}
				if !strings.Contains(got, want) {
					t.Fatalf("%s missing error %q: %s", operation, want, got)
				}
				if len(got) > 1024 {
					t.Fatalf("error retained unrelated payload: %d bytes", len(got))
				}
			}
		}
	}
}
