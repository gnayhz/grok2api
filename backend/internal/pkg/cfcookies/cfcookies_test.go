package cfcookies

import "testing"

func TestSanitizeWhitelistDedupAndLimits(t *testing.T) {
	if got := Sanitize("cf_clearance=abc; session=evil; __cf_bm=x; cf_clearance=dup; _cfuvid="); got != "cf_clearance=abc; __cf_bm=x" {
		t.Fatalf("sanitize = %q", got)
	}
	if got := Sanitize("cf_chl_1=ok; other=no"); got != "cf_chl_1=ok" {
		t.Fatalf("prefix family = %q", got)
	}
	if got := Sanitize(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
	if got := Sanitize("cf_clearance=\t"); got != "" {
		t.Fatalf("control chars rejected, got %q", got)
	}
}
