package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	redisruntime "github.com/chenyme/grok2api/backend/internal/infra/runtime/redis"
	redisclient "github.com/redis/go-redis/v9"
)

// The lifetime is consumed both by the account protocol and Redis TTL/index
// storage. Read through a second client to catch serialization or expiry loss.
func TestHTTPBuildOAuthDeviceLifetimeSurvivesRedis(t *testing.T) {
	address := os.Getenv("TEST_REDIS_ADDRESS")
	if address == "" {
		t.Skip("requires isolated TEST_REDIS_ADDRESS")
	}
	database := 0
	if value := os.Getenv("TEST_REDIS_DATABASE"); value != "" {
		var err error
		database, err = strconv.Atoi(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"expires", "interval"} {
		for _, seconds := range []int64{1800, 1 << 62, math.MaxInt64} {
			t.Run(fmt.Sprintf("%s/%d", field, seconds), func(t *testing.T) {
				ctx := context.Background()
				prefix := fmt.Sprintf("g71-device-%d:", time.Now().UnixNano())
				cfg := redisruntime.Config{Address: address, Username: os.Getenv("TEST_REDIS_USERNAME"), Password: os.Getenv("TEST_REDIS_PASSWORD"), Database: database, KeyPrefix: prefix}
				raw := redisclient.NewClient(&redisclient.Options{Addr: address, Username: cfg.Username, Password: cfg.Password, DB: database})
				t.Cleanup(func() {
					defer raw.Close()
					cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					keys, err := raw.Keys(cleanupCtx, prefix+"*").Result()
					if err != nil {
						t.Error(err)
						return
					}
					if len(keys) > 0 {
						if err := raw.Del(cleanupCtx, keys...).Err(); err != nil {
							t.Error(err)
						}
					}
				})
				first, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = first.Close() })
				second, err := redisruntime.Open(ctx, cfg)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = second.Close() })
				f := newControlDocumentFixture(t, "sqlite", "device_start")
				f.sessions.DeviceSessionRepository = redisruntime.NewDeviceSessionStore(first)
				var issued atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					issued.Add(1)
					expires, interval := int64(1800), int64(5)
					if field == "expires" {
						expires = seconds
					} else {
						interval = seconds
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"device_code":"shared-issued","user_code":"ABCD","verification_uri":"https://example.test/device","expires_in":%d,"interval":%d}`, expires, interval)
				}))
				t.Cleanup(server.Close)
				f.adapter.oauth.deviceURL = server.URL
				started := time.Now()
				output := f.request(http.MethodPost, "/accounts/device/start")
				var envelope struct {
					Data struct {
						SessionID string `json:"sessionId"`
					} `json:"data"`
				}
				if err := json.Unmarshal(output.Body.Bytes(), &envelope); err != nil || output.Code != 201 {
					t.Fatalf("start status=%d err=%v", output.Code, err)
				}
				session, err := redisruntime.NewDeviceSessionStore(second).Get(ctx, envelope.Data.SessionID, time.Now())
				if err != nil || session.DeviceCode != "shared-issued" {
					t.Fatalf("second client lost issued session: %v", err)
				}
				want := time.Duration(math.MaxInt64)
				if seconds == 1800 {
					want = 1800 * time.Second
				}
				duration := session.ExpiresAt.Sub(started)
				if field == "interval" {
					duration = session.Interval
					if session.NextPollAt.Sub(started) < want {
						t.Fatal("shared poll deadline wrapped")
					}
				}
				if duration < want || duration-want > time.Since(started)+time.Millisecond {
					t.Fatalf("shared lifetime=%v want=%v", duration, want)
				}
				ttl, err := raw.PTTL(ctx, prefix+"device:"+envelope.Data.SessionID).Result()
				if err != nil || ttl <= 0 {
					t.Fatalf("shared session TTL=%v err=%v", ttl, err)
				}
				if field == "expires" && ttl < want-time.Since(started)-time.Second {
					t.Fatalf("shared TTL shortened: %v want=%v", ttl, want)
				}
				if issued.Load() != 1 || f.generated.Load() != 0 {
					t.Fatal("device lifetime retried issuance or generated inference")
				}
			})
		}
	}
}
