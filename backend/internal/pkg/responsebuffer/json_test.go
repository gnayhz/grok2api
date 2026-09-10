package responsebuffer

import (
	"errors"
	"strings"
	"testing"
)

func TestJSONWorkspaceBoundsObjectAmplificationBeforeDecode(t *testing.T) {
	pool := NewPool(1 << 20)
	budget := pool.Request(1 << 20)
	data := []byte("[" + strings.Repeat("{},", 5000) + "{}]")
	if lease, err := JSONWorkspace(budget, data); !errors.Is(err, ErrExhausted) {
		lease.Release()
		t.Fatalf("object amplification admitted: %v", err)
	}
	if pool.Snapshot().Used != 0 {
		t.Fatal("rejected decoder reserved memory")
	}
	// Punctuation in ciphertext/text is not container metadata.
	data = []byte(`{"text":"` + strings.Repeat("{[,:]}", 5000) + `"}`)
	lease, err := JSONWorkspace(budget, data)
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if pool.Snapshot().Used != 0 {
		t.Fatal("workspace retained after decoding")
	}
}

func TestRetainedStateCumulativeAndReusableCaps(t *testing.T) {
	pool := NewPool(32 << 10)
	state := NewState(pool.Request(32<<10), 8192)
	for i := 0; i < 8; i++ {
		if err := state.Grow(1024, 4096); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Grow(1, 4096); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if got := pool.Snapshot().Used; got != 12288 {
		t.Fatalf("used=%d", got)
	}
	state.Close()
	state.Close()
	if pool.Snapshot().Used != 0 {
		t.Fatal("state retained reservations")
	}
}
