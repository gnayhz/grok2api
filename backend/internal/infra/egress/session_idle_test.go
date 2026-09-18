package egress

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	settingsdomain "github.com/chenyme/grok2api/backend/internal/domain/settings"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
)

func TestSessionIdleHotUpdatePreservesActiveHTTP2AndUnchangedClients(t *testing.T) {
	released := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(released) }) }
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Test-Connection", r.RemoteAddr)
		if r.URL.Path == "/active" {
			_, _ = io.WriteString(w, "before-")
			w.(http.Flusher).Flush()
			select {
			case <-released:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, "complete")
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()
	defer release()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	defer m.Close(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = WithBuildSession(ctx, "synthetic-session")
	acquire := func() *Lease {
		t.Helper()
		lease, err := m.Acquire(ctx, domain.ScopeBuild, "synthetic-account")
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	old := acquire()
	defer old.Release()
	trustPolicyTestServer(t, old, upstream)
	transport := old.client.(*http.Client).Transport.(*http.Transport)
	if transport.IdleConnTimeout != 5*time.Minute {
		t.Fatalf("default = %s", transport.IdleConnTimeout)
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL+"/active", nil)
	response, err := old.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.ProtoMajor != 2 {
		t.Fatal("expected real HTTP/2")
	}
	prefix := make([]byte, 7)
	if _, err := io.ReadFull(response.Body, prefix); err != nil || string(prefix) != "before-" {
		t.Fatalf("prefix: %q %v", prefix, err)
	}
	shared, err := m.transport.clientForContext(ctx, 0, domain.ScopeBuild, "", "", "", false, "", clientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := m.transport.clientForContext(ctx, 7, domain.ScopeBuild, "", "", "", false, "", clientOptions{sessionKey: "synthetic", freshTunnel: true})
	if err != nil {
		t.Fatal(err)
	}
	m.UpdateBuildTransportSettings(0, 8*time.Minute)
	next := acquire()
	defer next.Release()
	if next.client == old.client || next.client.(*http.Client).Transport.(*http.Transport).IdleConnTimeout != 8*time.Minute {
		t.Fatal("new session did not adopt timeout")
	}
	if transport.IdleConnTimeout != 5*time.Minute {
		t.Fatal("mutated active transport")
	}
	trustPolicyTestServer(t, next, upstream)
	send := func() string {
		t.Helper()
		r, _ := http.NewRequestWithContext(ctx, http.MethodGet, upstream.URL, nil)
		res, err := next.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || string(body) != "complete" {
			t.Fatalf("body: %q %v", body, err)
		}
		return res.Header.Get("X-Test-Connection")
	}
	conn := send()
	if conn == response.Header.Get("X-Test-Connection") || send() != conn {
		t.Fatal("replacement connection did not establish then reuse")
	}
	m.UpdateBuildTransportSettings(0, 8*time.Minute)
	same := acquire()
	if same.client != next.client {
		t.Fatal("saving unchanged value discarded warm client")
	}
	same.Release()
	sharedAfter, err := m.transport.clientForContext(ctx, 0, domain.ScopeBuild, "", "", "", false, "", clientOptions{})
	if err != nil || sharedAfter.client != shared.client {
		t.Fatalf("shared client changed: %v", err)
	}
	freshAfter, err := m.transport.clientForContext(ctx, 7, domain.ScopeBuild, "", "", "", false, "", clientOptions{sessionKey: "synthetic", freshTunnel: true})
	if err != nil || freshAfter.client != fresh.client || !freshAfter.policy.Fresh {
		t.Fatalf("fresh policy changed: %v", err)
	}
	release()
	body, err := io.ReadAll(response.Body)
	if err != nil || string(body) != "complete" {
		t.Fatalf("active response interrupted: %q %v", body, err)
	}
	response.Body.Close()
	old.Release()
	if _, owned := m.transport.owned.Load(old.client); owned {
		t.Fatal("retired transport leaked after completion")
	}
}

func TestSessionIdleHotUpdateRejectsStaleConstruction(t *testing.T) {
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{})
	defer m.Close(context.Background())
	key := clientCacheKey{scope: domain.ScopeBuild, sessionKey: "synthetic", buildHeaderTimeout: settingsdomain.DefaultBuildResponseHeaderTimeout, sessionIdleTimeout: 5 * time.Minute}
	m.UpdateBuildTransportSettings(0, 8*time.Minute)
	_, err := m.transport.createAndCacheClient(key, 0, domain.ScopeBuild, "", "", false, key.buildHeaderTimeout, clientOptions{sessionKey: key.sessionKey})
	if !errors.Is(err, errClientCacheInvalidated) {
		t.Fatalf("stale construction published: %v", err)
	}
}
