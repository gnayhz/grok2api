package gateway

import (
	"bytes"
	"strings"
	"testing"
)

// TestScannerScratchNoStringAliasing locks the scratch-reuse safety property:
// encoding/json copies decoded strings out of the payload buffer, so strings
// observed in earlier frames must not change when a later frame overwrites
// the same payload buffer. A regression to zero-copy slicing would corrupt
// visible-rune accounting under buffer reuse.
func TestScannerScratchNoStringAliasing(t *testing.T) {
	t.Parallel()
	payload1 := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"first-frame-text\"}}]}\n\n")
	payload2 := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"second-frame-text-overwrites-buffer\"}}]}\n\n")
	state := &qualityScanState{protocol: qualityProtocolChat}
	buf := append([]byte(nil), payload1...)
	observeQualityChunk(state, buf)
	visibleAfterFirst := state.visibleRunes
	buf = append(buf[:0], payload2...)
	observeQualityChunk(state, buf)
	if state.visibleRunes <= visibleAfterFirst {
		t.Fatalf("second frame must add visible runes: before=%d after=%d", visibleAfterFirst, state.visibleRunes)
	}
	fresh := &qualityScanState{protocol: qualityProtocolChat}
	observeQualityChunk(fresh, append([]byte(nil), payload2...))
	deltaReused := state.visibleRunes - visibleAfterFirst
	deltaFresh := fresh.visibleRunes
	if deltaReused != deltaFresh {
		t.Fatalf("buffer reuse changed accounting: reused=%d fresh=%d (string aliasing)", deltaReused, deltaFresh)
	}
	if strings.Contains(string(buf), "first-frame-text") {
		t.Fatal("buffer hygiene: first frame content must be fully overwritten")
	}
	if !bytes.Contains(buf, []byte("second-frame-text")) {
		t.Fatal("sanity: second frame content must be present")
	}
}
