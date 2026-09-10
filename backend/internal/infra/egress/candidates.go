package egress

import (
	"sync"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

// Selection copies mutable scoring fields out of immutable snapshots. Reuse
// the scratch space: copying 100 rich Node records used to allocate ~64 KiB on
// every request, even when all routing/client caches were warm.
var nodeCandidateBuffers sync.Pool

const maxPooledCandidates = 1024

func borrowNodeCandidates(count int) *[]domain.Node {
	var buffer *[]domain.Node
	if count <= maxPooledCandidates {
		buffer, _ = nodeCandidateBuffers.Get().(*[]domain.Node)
	}
	if buffer == nil {
		buffer = new([]domain.Node)
	}
	if cap(*buffer) < count {
		*buffer = make([]domain.Node, max(128, count))
	}
	*buffer = (*buffer)[:cap(*buffer)]
	return buffer
}

func releaseNodeCandidates(buffer *[]domain.Node) {
	// Do not retain old proxy credentials or snapshot references in idle buffers.
	clear(*buffer)
	if cap(*buffer) <= maxPooledCandidates {
		nodeCandidateBuffers.Put(buffer)
	}
}
