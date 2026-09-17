package egress

import (
	"errors"
	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	physical "github.com/chenyme/grok2api/backend/internal/port/physical"
	"testing"
)

func TestCanonicalUsageObjectIgnoresNestedCounters(t *testing.T) {
	for _, payload := range []string{
		`{"usage":{"input_tokens":20,"metadata":{"usage":{"input_tokens":999}}}}`,
		`{"usage":{"input_tokens":20,"metadata":{"input_tokens_details":{"cached_tokens":999}}}}`,
		`{"usage":{"input_tokens":20,"metadata":{"context_details":{"input_tokens":999}}}}`,
	} {
		ctx := attemptmeta.Begin(ledgerContext(), attemptmeta.Path{})
		if err := beginPhysicalCall(ctx); err != nil {
			t.Fatal(err)
		}
		recordPhysicalCall(ctx, nil, errors.New("test close"))
		ObservePhysicalPayload(ctx, attemptmeta.FromContext(ctx).ID, []byte(payload))
		fact := physical.PhysicalFacts(ctx)[0]
		if fact.Usage.Input != 20 || fact.Usage.Cached != 0 || fact.Usage.ContextInput != 0 {
			t.Fatalf("nested counters changed canonical usage: %+v", fact.Usage)
		}
	}
}
