package history_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	historydomain "github.com/chenyme/grok2api/backend/internal/domain/history"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestCompactionPreparationPreservesInputValuesAndLegacyEnvelope(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	// Encode the documented existing v1 envelope directly, as a previous process
	// would. The current codec must not require a migration or process identity.
	legacy, err := cipher.Encrypt(`{"version":1,"session":"old-cache-hint","summary":"SYNTHETIC_LEGACY_SUMMARY"}`)
	if err != nil {
		t.Fatal(err)
	}
	codec := historydomain.NewCompactionCodec(cipher)
	for _, kind := range []string{`compaction`, `\u0063ompaction`, `comp\u0061ction`, `\u0063\u006f\u006d\u0070\u0061\u0063\u0074\u0069\u006f\u006e`} {
		t.Run(kind, func(t *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":[{"type":"%s","encrypted_content":%q},{"type":"function_call_output","call_id":"call_id","output":{"id":9007199254740993,"fraction":0.1234567890123456789}}],"metadata":{"unchanged":9007199254740993},"instructions":"retain me"}`, kind, "g2a_compact_v1."+legacy))
			plan, err := codec.Prepare(body, "new-cache-hint")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Unavailable != 0 || plan.SessionDrifted != 1 {
				t.Fatalf("plan loss=%d drift=%d", plan.Unavailable, plan.SessionDrifted)
			}
			for _, value := range []string{"SYNTHETIC_LEGACY_SUMMARY", "9007199254740993", "0.1234567890123456789", `"instructions":"retain me"`} {
				if !strings.Contains(string(plan.Body), value) {
					t.Fatalf("lost %q in %s", value, plan.Body)
				}
			}
			if strings.Contains(string(plan.Body), legacy) {
				t.Fatal("gateway ciphertext reached candidate wire")
			}
			if !strings.Contains(string(body), legacy) {
				t.Fatal("preparation mutated caller-owned input")
			}
		})
	}
}

func TestCompactionPreparationMakesUnavailableStateExplicit(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	codec := historydomain.NewCompactionCodec(cipher)
	for _, raw := range []string{`"foreign-state"`, `"g2a_compact_v1.invalid"`, `17`, `null`, `{}`, `[]`} {
		t.Run(raw, func(t *testing.T) {
			plan, err := codec.Prepare([]byte(`{"input":[{"type":"compaction","encrypted_content":`+raw+`},{"role":"user","content":"continue"}]}`), "hint")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Unavailable != 1 || plan.SessionDrifted != 0 || !strings.Contains(string(plan.Body), "retained conversation messages") {
				t.Fatalf("plan=%+v", plan)
			}
		})
	}
	// Similar user-controlled names and nested JSON are ordinary content.
	for _, raw := range []string{`{"input":[{"Type":"compaction","encrypted_content":"foreign"}]}`, `{"input":[{"type":"message","content":{"type":"compaction","value":17}}]}`, `{"input":[{"type":"message","content":"\u0063ompaction"}]}`, `{"input":[{"type":"message","content":"plain"}]}`} {
		body := []byte(raw)
		plan, err := codec.Prepare(body, "hint")
		if err != nil || plan.Unavailable != 0 || &plan.Body[0] != &body[0] {
			t.Fatalf("ordinary content changed: %+v %v", plan, err)
		}
	}
}

func TestCompactionCodecNewProcessRetainsSummaryWithDifferentHint(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	old := historydomain.NewCompactionCodec(cipher)
	blob, err := old.Encode("old", "synthetic summary")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"input": []any{map[string]any{"type": "compaction", "encrypted_content": blob}}})
	if err != nil {
		t.Fatal(err)
	}
	next := historydomain.NewCompactionCodec(cipher)
	plan, err := next.Prepare(body, "new")
	if err != nil || plan.Unavailable != 0 || plan.SessionDrifted != 1 || !strings.Contains(string(plan.Body), "synthetic summary") {
		t.Fatalf("restart plan=%+v err=%v", plan, err)
	}
}
