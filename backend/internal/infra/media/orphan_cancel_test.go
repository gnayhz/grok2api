package media

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// Cancel after the walk has started, without depending on filesystem timing.
type cancelDuringWalk struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelDuringWalk) Err() error {
	c.checks++
	if c.checks == 10 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestMediaObjectEnumerationStopsDuringCancellation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "images", "aa")
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if err := os.WriteFile(filepath.Join(path, fmt.Sprintf("img_%03d.png", i)), []byte("object"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	store, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelDuringWalk{Context: base, cancel: cancel}
	objects, temps, err := store.ListMediaObjectFiles(ctx)
	if !errors.Is(err, context.Canceled) || objects != nil || temps != nil {
		t.Fatalf("cancelled enumeration returned complete files: objects=%d temps=%d checks=%d err=%v", len(objects), len(temps), ctx.checks, err)
	}
	if ctx.checks != 10 {
		t.Fatalf("walk continued after cancellation: checks=%d", ctx.checks)
	}
	objects, _, err = store.ListMediaObjectFiles(context.Background())
	if err != nil || len(objects) != 50 {
		t.Fatalf("next enumeration failed: %d %v", len(objects), err)
	}
}
