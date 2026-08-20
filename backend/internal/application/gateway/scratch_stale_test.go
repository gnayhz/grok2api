package gateway

import "testing"

// TestScratchReuseDoesNotResurrectStaleFields: encoding/json reuses slice
// backing arrays and does NOT reset fields absent from the new JSON. With the
// scratch-reuse optimization, frame N's content must not be recounted when
// frame N+1 omits the content field (stale-element resurrection).
func TestScratchReuseDoesNotResurrectStaleFields(t *testing.T) {
	t.Parallel()
	state := &qualityScanState{protocol: qualityProtocolChat}
	frameFull := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello world padding\"},\"finish_reason\":null}]}\n\n")
	frameEmpty := []byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":null}]}\n\n")
	ObserveQualityChunk(state, frameFull)
	first := state.visibleRunes
	if first == 0 {
		t.Fatal("setup: first frame must record visible runes")
	}
	ObserveQualityChunk(state, frameEmpty)
	if state.visibleRunes != first {
		t.Fatalf("stale field resurrection: visible runes grew %d -> %d on a frame with no content", first, state.visibleRunes)
	}
}
