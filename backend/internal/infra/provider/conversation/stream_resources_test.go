package conversation

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
)

func TestConverterResourceLimitClosesAllRetainedState(t *testing.T) {
	pool := responsebuffer.NewPool(256 << 10)
	budget := pool.Request(256 << 10)
	raw := strings.Repeat("data: {\"type\":\"response.reasoning_summary_text.delta\",\"item_id\":\"r1\",\"delta\":\""+strings.Repeat("x", 512)+"\"}\n\n", 200)
	stream := responseflow.New(io.NopCloser(strings.NewReader(raw)), budget)
	body := ConvertResponseStream(stream, OperationChat)
	_, err := io.Copy(io.Discard, body)
	_ = body.Close()
	if !errors.Is(err, responsebuffer.ErrExhausted) {
		t.Fatalf("converter should exhaust capacity: %v", err)
	}
	if got := pool.Snapshot().Used; got != 0 {
		t.Fatalf("retained after converter close: %d", got)
	}
}

func TestConverterBoundsUniqueReasoningIDs(t *testing.T) {
	pool := responsebuffer.NewPool(32 << 20)
	converter := newStreamConverterWithBudget(io.Discard, OperationChat, ResponseOptions{}, pool.Request(32<<20))
	defer converter.releaseResources()
	for i := 0; i <= maxConverterItems; i++ {
		data := []byte(fmt.Sprintf(`{"type":"response.reasoning_text.delta","item_id":"item_%d","delta":"%d"}`, i, i))
		err := converter.handle("", data)
		if i == maxConverterItems {
			if !errors.Is(err, responsebuffer.ErrLimit) {
				t.Fatalf("too many IDs: %v", err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
	}
	converter.releaseResources()
	if pool.Snapshot().Used != 0 {
		t.Fatal("ID state retained after close")
	}
}
