package cli

import (
	"context"
	"errors"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/infra/provider"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOAuthRefreshPreservesLongRetryAfter(t *testing.T) {
	for _, seconds := range []int64{300, 18446744193} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.FormValue("grant_type") != "refresh_token" || r.FormValue("refresh_token") != "refresh" {
					t.Errorf("unexpected refresh form")
				}
				w.Header().Set("Retry-After", fmt.Sprint(seconds))
				w.WriteHeader(429)
				_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
			}))
			defer server.Close()
			client := newOAuthClient(server.Client(), nil, nil)
			client.tokenURL = server.URL
			_, err := client.refresh(context.Background(), "refresh")
			var refreshErr *provider.CredentialRefreshError
			if !errors.As(err, &refreshErr) || refreshErr.Status != 429 || refreshErr.Permanent || refreshErr.Code != "temporarily_unavailable" || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
			want := 300 * time.Second
			if seconds > 100000 {
				want = time.Duration(1<<63 - 1)
			}
			if refreshErr.RetryAfter != want {
				t.Fatalf("delay=%v want=%v", refreshErr.RetryAfter, want)
			}
		})
	}
}
