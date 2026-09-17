package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func TestStatsigQuotaWaiterDeadlineDoesNotWaitForLeader(t *testing.T) {
	var quotaCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/index" {
			_, _ = io.WriteString(w, `<meta name="grok-site-verification" content="synthetic-meta">`)
			return
		}
		if r.URL.Path != "/rest/rate-limits" {
			t.Errorf("unexpected upstream %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		quotaCalls.Add(1)
		_, _ = io.WriteString(w, `{"totalQueries":30,"remainingQueries":4,"windowSizeSeconds":7200}`)
	}))
	defer upstream.Close()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	signerServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"x-statsig-id": base64.RawStdEncoding.EncodeToString(make([]byte, 70))})
	}))
	defer signerServer.Close()
	cipher, err := security.NewCipher(base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if err != nil {
		t.Fatal(err)
	}
	token, err := cipher.Encrypt("synthetic-statsig-credential")
	if err != nil {
		t.Fatal(err)
	}
	manager := infraegress.NewManagerWithLimits(egressRepositoryStub{}, cipher, netbudget.Limits{})
	defer func() { _ = manager.Close(context.Background()) }()
	adapter := NewAdapter(Config{BaseURL: upstream.URL, StatsigMode: "url", StatsigSignerURL: signerServer.URL, QuotaTimeout: 2 * time.Second}, manager, cipher, nil, nil)
	adapter.statsig.client = signerServer.Client()
	adapter.statsig.validateEndpoint = func(context.Context, string) error { return nil }
	credential := account.Credential{ID: 1, Provider: account.ProviderWeb, AuthType: account.AuthTypeSSO, EncryptedAccessToken: token}
	leader := make(chan error, 1)
	go func() { _, err := adapter.SyncQuotaMode(context.Background(), credential, "fast"); leader <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("leader never reached actual signer")
	}
	waiterCtx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	waiter := make(chan error, 1)
	started := time.Now()
	go func() { _, err := adapter.SyncQuotaMode(waiterCtx, credential, "fast"); waiter <- err }()
	timely := false
	var waiterErr error
	select {
	case waiterErr = <-waiter:
		timely = true
	case <-time.After(500 * time.Millisecond):
	}
	elapsed := time.Since(started)
	once.Do(func() { close(release) })
	if !timely {
		waiterErr = <-waiter
	}
	if err := <-leader; err != nil {
		t.Errorf("leader failed: %v", err)
	}
	if !timely || !errors.Is(waiterErr, context.DeadlineExceeded) || quotaCalls.Load() != 1 {
		t.Fatalf("waiter respected own deadline=%v elapsed=%v err=%v physical quota calls=%d", timely, elapsed, waiterErr, quotaCalls.Load())
	}
}
