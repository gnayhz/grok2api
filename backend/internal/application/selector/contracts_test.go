package selector

import (
	"testing"
)

func TestLeaseReleaseIsIdempotentAndClosesAttemptResources(t *testing.T) {
	releases := 0
	resources := &AttemptResources{}
	lease := &Lease{resources: resources, release: func() { releases++ }}
	lease.Release()
	lease.Release()
	if !resources.closed {
		t.Fatal("lease release must close attempt resources")
	}
	if releases != 1 {
		t.Fatalf("concurrency slot released %d times", releases)
	}
}
