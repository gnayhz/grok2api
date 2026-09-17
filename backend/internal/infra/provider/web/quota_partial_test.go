package web

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func partialQuotaAdapter(t *testing.T, endpoint string) (*Adapter, account.Credential) {
	t.Helper()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-quota-credential")
	if err != nil {
		t.Fatal(err)
	}
	manager := infraegress.NewManagerWithLimits(egressRepositoryStub{}, cipher, netbudget.Limits{})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	return NewAdapter(Config{BaseURL: endpoint, StatsigMode: "manual", StatsigManualValue: base64.RawStdEncoding.EncodeToString(make([]byte, 70))}, manager, cipher, nil, nil), account.Credential{ID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: token}
}

func TestWebFullQuotaPreservesAuthoritativeWeeklyFallback(t *testing.T) {
	weeklyBody, err := hex.DecodeString(capturedWeeklyCreditsHex)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name                                     string
		tier                                     account.WebTier
		failed, successfulTotal                  int
		weeklyFailure, imagineFailure, wantError bool
		weeklyCalls                              int32
	}{
		{name: "new_paid_signal_auto", tier: account.WebTierBasic, failed: 2, successfulTotal: 50, weeklyCalls: 1},
		{name: "new_paid_signal_fast", tier: account.WebTierBasic, failed: 1, successfulTotal: 140, weeklyCalls: 1},
		{name: "known_super_no_modes", tier: account.WebTierSuper, failed: 3, weeklyCalls: 1},
		{name: "known_heavy_no_modes", tier: account.WebTierHeavy, failed: 3, weeklyCalls: 1},
		{name: "basic_no_modes", tier: account.WebTierBasic, failed: 3, wantError: true},
		{name: "unknown_no_modes", tier: account.WebTierAuto, failed: 3, wantError: true},
		{name: "old_paid_new_basic_partial", tier: account.WebTierSuper, failed: 2, successfulTotal: 7, wantError: true},
		{name: "old_paid_new_unknown_partial", tier: account.WebTierSuper, failed: 2, successfulTotal: 9, wantError: true},
		{name: "weekly_failure", tier: account.WebTierSuper, failed: 1, successfulTotal: 140, weeklyFailure: true, wantError: true, weeklyCalls: 1},
		{name: "imagine_failure", tier: account.WebTierSuper, failed: 1, successfulTotal: 140, imagineFailure: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/rest/rate-limits":
					var input struct {
						ModelName string `json:"modelName"`
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						return
					}
					bit := 1
					if input.ModelName == "fast" {
						bit = 2
					}
					if tc.failed&bit != 0 {
						http.Error(w, "temporary mode failure", 503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]int{"remainingQueries": 3, "totalQueries": tc.successfulTotal, "windowSizeSeconds": 7200})
				case "/rest/media/imagine/quota_info":
					if tc.imagineFailure {
						http.Error(w, "temporary imagine failure", 503)
						return
					}
					writeEmptyImagineQuota(w)
				case "/grok_api_v2.GrokBuildBilling/GetGrokCreditsConfig":
					calls.Add(1)
					if tc.weeklyFailure {
						http.Error(w, "temporary weekly failure", 503)
						return
					}
					_, _ = w.Write(weeklyBody)
				default:
					t.Errorf("unexpected endpoint %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			adapter, credential := partialQuotaAdapter(t, upstream.URL)
			credential.WebTier = tc.tier
			got, err := adapter.SyncQuota(context.Background(), credential)
			if (err != nil) != tc.wantError || calls.Load() != tc.weeklyCalls {
				t.Fatalf("snapshot=%+v weekly=%d err=%v", got, calls.Load(), err)
			}
			if !tc.wantError && (len(got.Windows) != 1 || got.Windows[0].Mode != "weekly" || got.Windows[0].Remaining != 8900) {
				t.Fatalf("incomplete weekly snapshot=%+v", got)
			}
			if tc.wantError && len(got.Windows) != 0 {
				t.Fatalf("failed refresh returned windows=%+v", got.Windows)
			}
		})
	}
}

func TestWebFullQuotaCancellationDoesNotPublishPartialResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		calls.Add(1)
		cancel()
		<-r.Context().Done()
	}))
	defer upstream.Close()
	adapter, credential := partialQuotaAdapter(t, upstream.URL)
	start := time.Now()
	got, err := adapter.SyncQuota(ctx, credential)
	if !errors.Is(err, context.Canceled) || len(got.Windows) != 0 || calls.Load() != 1 || time.Since(start) > time.Second {
		t.Fatalf("canceled full quota: calls=%d snapshot=%+v err=%v", calls.Load(), got, err)
	}
}

func TestWebIncrementalQuotaDoesNotRequireOtherChatModes(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path == "/rest/media/imagine/quota_info" {
			writeEmptyImagineQuota(w)
			return
		}
		var input struct {
			ModelName string `json:"modelName"`
		}
		if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
			t.Error(err)
			return
		}
		if input.ModelName != "fast" {
			t.Errorf("unexpected mode %s", input.ModelName)
			http.Error(w, "no", 503)
			return
		}
		_, _ = io.WriteString(w, `{"totalQueries":30,"remainingQueries":4,"windowSizeSeconds":7200}`)
	}))
	defer upstream.Close()
	adapter, credential := partialQuotaAdapter(t, upstream.URL)
	got, err := adapter.SyncQuotaMode(context.Background(), credential, "fast")
	if err != nil || got.Remaining != 4 || calls.Load() != 1 {
		t.Fatalf("incremental mode=%+v calls=%d err=%v", got, calls.Load(), err)
	}
	group, err := adapter.SyncQuotaGroup(context.Background(), credential, account.QuotaGroupWebImagine)
	if err != nil || group.Group != account.QuotaGroupWebImagine || calls.Load() != 2 {
		t.Fatalf("incremental group=%+v calls=%d err=%v", group, calls.Load(), err)
	}
}
