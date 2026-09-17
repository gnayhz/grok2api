package cli

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOAuthPositiveSecondsCannotWrap(t *testing.T) {
	const maxSeconds = int64(math.MaxInt64) / int64(time.Second)
	for _, field := range []string{"token_expires", "device_expires", "device_interval"} {
		for _, seconds := range []int64{-1, 0, 1, 3600, maxSeconds, maxSeconds + 1, 1 << 62, math.MaxInt64} {
			t.Run(fmt.Sprintf("%s/%d", field, seconds), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					if field == "token_expires" {
						_, _ = fmt.Fprintf(w, `{"access_token":"access","refresh_token":"refresh","expires_in":%d}`, seconds)
					} else {
						expires, interval := int64(1800), int64(5)
						if field == "device_expires" {
							expires = seconds
						} else {
							interval = seconds
						}
						_, _ = io.WriteString(w, fmt.Sprintf(`{"device_code":"local-device","user_code":"ABCD","verification_uri":"https://example.test/device","expires_in":%d,"interval":%d}`, expires, interval))
					}
				}))
				t.Cleanup(server.Close)
				client := newOAuthClient(server.Client(), nil, nil)
				client.tokenURL, client.deviceURL = server.URL, server.URL
				started := time.Now()
				var duration time.Duration
				var err error
				if field == "token_expires" {
					value, callErr := client.refreshWithClientID(context.Background(), "original-refresh", "")
					err = callErr
					duration = value.ExpiresAt.Sub(started)
				} else {
					value, callErr := client.startDevice(context.Background())
					err = callErr
					if field == "device_expires" {
						duration = value.ExpiresIn
					} else {
						duration = value.Interval
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				wantSeconds := seconds
				if seconds <= 0 {
					wantSeconds = 3600
					if field == "device_expires" {
						wantSeconds = 1800
					} else if field == "device_interval" {
						wantSeconds = 5
					}
				}
				want := time.Duration(math.MaxInt64)
				if wantSeconds <= maxSeconds {
					want = time.Duration(wantSeconds) * time.Second
				}
				if duration < want || duration-want > time.Since(started)+time.Millisecond {
					t.Fatalf("duration=%v want=%v", duration, want)
				}
			})
		}
	}
}
