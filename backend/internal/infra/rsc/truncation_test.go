package rsc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A flight payload with homepage markers but no terminal close must be error:
// a stream cut mid-flight reads as a clean EOF to io.ReadAll while __next_f
// appears before the user payload, so without the completeness check the
// truncated body would classify as clean (external review 7.md, re-triaged).
func TestTruncatedFlightPayloadIsError(t *testing.T) {
	cut := "<!doctype html><script>self.__next_f.push([1,\"{\"user\":{\"email\":\"a@b.c\""
	if strings.Contains(cut, "</script>") {
		t.Fatal("fixture must be truncated")
	}
	result := ParseRisk(cut)
	if result.Verdict != VerdictError {
		t.Fatalf("truncated flight payload = %#v, want error", result)
	}
	if !strings.Contains(result.Error, "terminal evidence") {
		t.Fatalf("error should name truncation, got %q", result.Error)
	}
}

// A complete document without flags stays clean.
func TestCompleteDocumentWithoutFlagsIsClean(t *testing.T) {
	body := "<!doctype html><html><head></head><body>grok</body></html>"
	result := ParseRisk(body)
	if result.Verdict != VerdictClean {
		t.Fatalf("complete document = %#v, want clean", result)
	}
}

// A closed inline script with flags still parses normally.
func TestCompleteFlightDeniedStillDenied(t *testing.T) {
	body := homepage("{\"user\":{\"botFlagSource\":1,\"botFlagDetails\":\"policy=deny,risk=0.9,event=$registration\"}}")
	result := ParseRisk(body)
	if result.Verdict != VerdictDenied || result.RiskScore != 0.9 {
		t.Fatalf("complete denied fixture = %#v", result)
	}
}

// Whitespace after the closing tag must not flip the completeness check.
func TestTrailingWhitespaceAfterCloseIsComplete(t *testing.T) {
	body := "<html><body>grok</body></html>\n\n"
	result := ParseRisk(body)
	if result.Verdict != VerdictClean {
		t.Fatalf("trailing whitespace fixture = %#v, want clean", result)
	}
}

// ContentLength short read: server announces more bytes than it sends; the
// truncated-body guard must classify error before parsing.
func TestContentLengthShortReadIsError(t *testing.T) {
	full := "<html><body>" + strings.Repeat("x", 10000) + "</body></html>"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "999999")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(full[:len(full)/2]))
	}))
	defer server.Close()
	checker := NewChecker(5 * time.Second)
	checker.baseURL = server.URL
	result := checker.Check(t.Context(), "sso-token")
	if result.Verdict != VerdictError {
		t.Fatalf("short read = %#v, want error", result)
	}
	if !strings.Contains(result.Error, "truncat") && !strings.Contains(result.Error, "EOF") {
		t.Fatalf("short read must not classify parseable, got %q", result.Error)
	}
}

// Close-delimited complete body (no Content-Length) with flags parses fine.
func TestChunkedCompleteBodyParses(t *testing.T) {
	body := homepage("{\"user\":{\"botFlagSource\":1,\"botFlagDetails\":\"policy=deny,risk=0.9,event=$registration\"}}")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	checker := NewChecker(5 * time.Second)
	checker.baseURL = server.URL
	result := checker.Check(context.Background(), "sso-token")
	if result.Verdict != VerdictDenied {
		t.Fatalf("chunked complete body = %#v, want denied", result)
	}
}
