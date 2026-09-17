package updatecheck

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type releaseTransport func(*http.Request) (*http.Response, error)

func (f releaseTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type releaseBody struct {
	io.Reader
	closed bool
}

func (b *releaseBody) Close() error { b.closed = true; return nil }

func TestGitHubSourceOwnsWireValidationAndBodyLifetime(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		status     int
		wantError  bool
	}{
		{"release", `{"tag_name":" v3.0.1 ","body":" Notes ","html_url":"https://untrusted.invalid/"}`, 200, false},
		{"empty tag", `{"tag_name":" "}`, 200, true},
		{"malformed", `{`, 200, true},
		{"unavailable", `{}`, 503, true},
		{"oversized", strings.Repeat("x", maxReleaseBytes+1), 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &releaseBody{Reader: strings.NewReader(tc.body)}
			source := NewGitHubSource(&http.Client{Transport: releaseTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != latestReleaseAPI || r.Header.Get("User-Agent") != "grok2api/update-check" || r.Header.Get("Accept") != "application/vnd.github+json" {
					t.Fatalf("unexpected release request: %s %v", r.URL, r.Header)
				}
				return &http.Response{StatusCode: tc.status, Body: body, Header: make(http.Header)}, nil
			})})
			got, err := source.LatestRelease(context.Background())
			if (err != nil) != tc.wantError || !body.closed {
				t.Fatalf("err=%v closed=%v", err, body.closed)
			}
			if !tc.wantError && (got.Tag != "v3.0.1" || got.Notes != "Notes" || got.URL != "https://github.com/chenyme/grok2api/releases/tag/v3.0.1") {
				t.Fatalf("release = %+v", got)
			}
		})
	}
}
