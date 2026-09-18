package responsebuffer

import (
	"strings"
	"testing"
)

func TestBorrowedJSONWorkspaceCountsKeysAndStructure(t *testing.T) {
	key := strings.Repeat("k", 1<<20)
	value := strings.Repeat("x", 1<<20)
	base := BorrowedJSONWorkspaceSize([]byte(`{"k":"x"}`))
	if got := BorrowedJSONWorkspaceSize([]byte(`{"k":"` + value + `"}`)); got != base {
		t.Fatalf("borrowed value charged as a decoded string: %d vs %d", got, base)
	}
	if got := BorrowedJSONWorkspaceSize([]byte(`{"` + key + `":"x"}`)); got < 4*len(key) {
		t.Fatal("large decoded key missing from budget")
	}
	if got := BorrowedJSONWorkspaceSize([]byte(`{"k":[` + strings.Repeat(`{},`, 9999) + `{}]}`)); got < 3<<20 {
		t.Fatal("structural amplification missing from budget")
	}
	escaped := []byte(`{"key\" ": "value : {}", "nested":{"long\u006bey":true}}`)
	if BorrowedJSONWorkspaceSize(escaped) <= BorrowedJSONWorkspaceSize([]byte(`{"key":"value", "nested":{}}`)) {
		t.Fatal("escaped/nested keys not charged")
	}
}
