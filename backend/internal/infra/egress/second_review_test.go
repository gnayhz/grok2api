package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/browsertransport"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// Regression coverage for the second architecture review.
func TestSecondReviewBuildSessionPreservesEnvironmentProxy(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	for _, session := range []string{"", "conversation"} {
		ctx := WithBuildSession(context.Background(), session)
		lease, err := m.AcquireBuildEnvironmentDirect(ctx, "account")
		if err != nil {
			t.Fatal(err)
		}
		tr := lease.client.(*http.Client).Transport.(*http.Transport)
		lease.Release()
		if tr.Proxy == nil {
			t.Errorf("environment proxy policy disappeared when session=%q", session)
		}
	}
}

func TestSecondReviewProbeSharesRequestBudget(t *testing.T) {
	entered, gate := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	unblock := func() { gateOnce.Do(func() { close(gate) }) }
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-gate:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "ip=1.1.1.1\n")
	}))
	defer server.Close()
	defer unblock()
	m := NewManagerWithLimits(egressRepositoryTestStub{}, nil, netbudget.Limits{Requests: 1})
	defer m.Close(context.Background())
	_, release, err := m.transport.network.BeginRequest(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := m.probeEgressEndpoint(context.Background(), preparedEgressProbe{nodeID: 1}, domain.ProbeProviderCloudflare, "ipv4", server.URL)
		done <- err
	}()
	select {
	case <-entered:
		s := m.RuntimeStats()
		t.Errorf("probe reached server while maxRequests=1 was occupied; requests=%d clients=%d activeSockets=%d dialing=%d", s.Network.Requests, s.Network.Clients, s.Network.ActiveConnections, s.Network.Dialing)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe did not resume after request admission")
	}
	s := m.RuntimeStats().Network
	if s.Requests != 1 || s.Clients != 1 || s.ActiveConnections != 1 || s.Dialing != 0 {
		t.Errorf("probe ownership during response: %+v", s)
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("probe did not finish")
	}
	deadline := time.Now().Add(time.Second)
	for {
		s := m.RuntimeStats().Network
		if s.Requests == 0 && s.Clients == 0 && s.Connections == 0 && s.Dialing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe retained resources: %+v", s)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSecondReviewBrowserCapacityIsNotProxyFailure(t *testing.T) {
	for _, capacityErr := range []error{browsertransport.ErrOriginCapacity, browsertransport.ErrDrainingCapacity, netbudget.ErrCapacity, netbudget.ErrClosed, ErrClientRetired} {
		for _, scope := range []domain.Scope{domain.ScopeWeb, domain.ScopeBuild, domain.ScopeConsole} {
			kind, accepted := feedbackKind(scope, 0, neterror.MarkTransport(capacityErr, neterror.PhaseRequest))
			if accepted {
				t.Errorf("local capacity %v classified as %v for %v", capacityErr, kind, scope)
			}
		}
	}
}

func TestSecondReviewOld403CannotInvalidateNewClearanceViaFeedback(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr"})
	c := m.clearance
	version := c.clearanceVersion
	if !c.cacheClearance("direct", clearanceSolution{Cookies: "old", UserAgent: "UA"}, time.Now(), version, "fingerprint", "binding", time.Minute) {
		t.Fatal("cache old")
	}
	lease := &Lease{NodeID: 0, Scope: domain.ScopeWeb, clearanceManager: m, clearanceKey: "direct", clearanceGeneration: c.generationFor("direct"), healthBaseline: domain.HealthState{Health: 1}}
	if !c.cacheClearance("direct", clearanceSolution{Cookies: "fresh", UserAgent: "UA"}, time.Now(), version, "fingerprint", "binding", time.Minute) {
		t.Fatal("cache fresh")
	}
	lease.InvalidateClearance()
	lease.Observe(http.StatusForbidden, nil)
	if err := m.FlushFeedback(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.clearanceMu.Lock()
	state := c.clearances["direct"]
	c.clearanceMu.Unlock()
	if state.invalid {
		t.Fatalf("late 403 feedback invalidated the fresh clearance: cookie=%s oldGeneration=%d currentGeneration=%d", state.cookies, lease.clearanceGeneration, state.generation)
	}
}

// ProxyFromEnvironment caches process configuration, so evaluate the matrix in
// a fresh test process, independent of the runner's own proxy environment.
func TestSecondReviewBuildEnvironmentPolicyMatrix(t *testing.T) {
	if os.Getenv("EGRESS_ENV_POLICY_TEST") != "1" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSecondReviewBuildEnvironmentPolicyMatrix$", "-test.v")
		for _, entry := range os.Environ() {
			key, _, _ := strings.Cut(entry, "=")
			switch strings.ToUpper(key) {
			case "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "REQUEST_METHOD", "EGRESS_ENV_POLICY_TEST":
				continue
			}
			cmd.Env = append(cmd.Env, entry)
		}
		cmd.Env = append(cmd.Env, "EGRESS_ENV_POLICY_TEST=1", "HTTP_PROXY=http://http-proxy.example:8080", "HTTPS_PROXY=http://https-proxy.example:8443", "NO_PROXY=.internal.example")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("environment matrix: %v\n%s", err, output)
		}
		return
	}
	for _, isolated := range []bool{false, true} {
		for _, session := range []string{"", "conversation"} {
			t.Run(fmt.Sprintf("isolated=%t/session=%s", isolated, session), func(t *testing.T) {
				m := NewManager(egressRepositoryTestStub{}, nil)
				defer m.Close(context.Background())
				m.transport.accountIsolated.Store(isolated)
				lease, err := m.AcquireBuildEnvironmentDirect(WithBuildSession(context.Background(), session), "account")
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				tr := lease.client.(*http.Client).Transport.(*http.Transport)
				if tr.Proxy == nil {
					t.Fatal("missing environment policy")
				}
				for _, tc := range []struct{ target, want string }{
					{"http://service.example/path", "http://http-proxy.example:8080"},
					{"https://service.example/path", "http://https-proxy.example:8443"},
					{"http://api.internal.example/path", ""},
					{"https://api.internal.example/path", ""},
				} {
					u, _ := url.Parse(tc.target)
					proxy, err := tr.Proxy(&http.Request{URL: u})
					got := ""
					if proxy != nil {
						got = proxy.String()
					}
					if err != nil || got != tc.want {
						t.Errorf("%s proxy=%q err=%v want=%q", tc.target, got, err, tc.want)
					}
				}
				if session != "" && tr.MaxConnsPerHost != 1 {
					t.Errorf("session lost connection pin: %d", tr.MaxConnsPerHost)
				}
			})
		}
	}
	for _, pinned := range []bool{false, true} {
		for _, proxy := range []string{"http://explicit.example:8080", "socks5://explicit.example:1080"} {
			client, err := newBuildClientConfigured(proxy, time.Second, buildConnectionOptions{environmentProxy: true, sessionPinned: pinned})
			if err != nil {
				t.Fatal(err)
			}
			tr := client.Transport.(*http.Transport)
			u, _ := url.Parse("https://service.example/")
			if strings.HasPrefix(proxy, "socks5:") {
				if tr.Proxy != nil {
					t.Fatal("explicit SOCKS inherited an HTTP environment proxy")
				}
			} else {
				selected, err := tr.Proxy(&http.Request{URL: u})
				if err != nil || selected.String() != proxy {
					t.Fatalf("explicit proxy lost precedence: %v %v", selected, err)
				}
			}
			client.CloseIdleConnections()
		}
	}
}

func TestSecondReviewClearanceGenerationTravelsWithCookie(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager(egressRepositoryTestStub{}, cipher)
	defer m.Close(context.Background())
	cfg := ClearanceConfig{Mode: "flaresolverr", TargetURL: "https://grok.com", RefreshInterval: time.Hour}
	m.UpdateClearanceConfig(cfg)
	c := m.clearance
	cache := func(cookie string) {
		t.Helper()
		if !c.cacheClearance("direct", clearanceSolution{Cookies: cookie, UserAgent: DefaultUserAgent}, time.Now(), c.clearanceVersion, clearanceFingerprint(cfg, ""), clearanceBindingFingerprint(cfg, ""), time.Hour) {
			t.Fatal("cache failed")
		}
	}
	cache("old")
	oldGeneration := c.generationFor("direct")
	entered, gate := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	unblock := func() { gateOnce.Do(func() { close(gate) }) }
	defer unblock()
	m.transport.newBrowserClient = func(proxy, ua string) (*browserClient, error) {
		close(entered)
		<-gate
		return newBrowserClientWithBudget(proxy, ua, m.transport.network)
	}
	completed := make(chan *Lease, 1)
	errors := make(chan error, 1)
	go func() {
		lease, _, err := m.leaseForNode(context.Background(), domain.ScopeWeb, "account", "", true, domain.Node{Health: 1})
		completed <- lease
		errors <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("client factory did not start")
	}
	cache("fresh")
	unblock()
	lease := <-completed
	if err := <-errors; err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	if lease.CFCookies != "old" || lease.clearanceGeneration != oldGeneration {
		t.Fatalf("cookie and generation separated: cookie=%q generation=%d want=%d", lease.CFCookies, lease.clearanceGeneration, oldGeneration)
	}
	lease.InvalidateClearance()
	lease.Observe(http.StatusForbidden, nil)
	if err := m.FlushFeedback(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.clearanceMu.Lock()
	state := c.clearances["direct"]
	c.clearanceMu.Unlock()
	if state.invalid || state.cookies != "fresh" {
		t.Fatalf("late rejection invalidated fresh cookie: %+v", state)
	}
	// The next real acquisition must use the cached refresh and the existing
	// transport. Re-entering the solver here would fail (no solver is configured).
	fresh, _, err := m.leaseForNode(context.Background(), domain.ScopeWeb, "account", "", true, domain.Node{Health: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Release()
	if fresh.CFCookies != "fresh" || fresh.clearanceGeneration != state.generation || fresh.client != lease.client {
		t.Fatalf("late rejection discarded refresh or transport: cookie=%q generation=%d reusedClient=%t", fresh.CFCookies, fresh.clearanceGeneration, fresh.client == lease.client)
	}
}

type reviewCountingClient struct{ closes atomic.Int32 }

func (*reviewCountingClient) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused test client")
}
func (c *reviewCountingClient) CloseIdleConnections() { c.closes.Add(1) }

func TestSecondReviewLateClearanceRejectionsAreIsolated(t *testing.T) {
	for _, tc := range []struct {
		key    string
		nodeID uint64
		pool   bool
	}{{"direct", 0, false}, {"node:7", 7, false}, {"node:7:account:first", 7, true}} {
		t.Run(tc.key, func(t *testing.T) {
			m := NewManager(egressRepositoryTestStub{}, nil)
			defer m.Close(context.Background())
			m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr"})
			c := m.clearance
			cache := func(cookie string) {
				if !c.cacheClearance(tc.key, clearanceSolution{Cookies: cookie, UserAgent: "UA"}, time.Now(), c.clearanceVersion, "fp", "binding", time.Minute) {
					t.Fatal("cache failed")
				}
			}
			cache("old")
			client := &reviewCountingClient{}
			lease := &Lease{NodeID: tc.nodeID, Scope: domain.ScopeWeb, clearanceManager: m, clearanceKey: tc.key, clearanceGeneration: c.generationFor(tc.key), client: client, proxyPool: tc.pool, healthBaseline: domain.HealthState{Health: 1}}
			cache("fresh")
			var workers sync.WaitGroup
			for range 32 {
				workers.Add(1)
				go func() { defer workers.Done(); lease.InvalidateClearance(); lease.Observe(http.StatusForbidden, nil) }()
			}
			workers.Wait()
			if err := m.FlushFeedback(context.Background()); err != nil {
				t.Fatal(err)
			}
			c.clearanceMu.Lock()
			state := c.clearances[tc.key]
			c.clearanceMu.Unlock()
			if state.invalid || client.closes.Load() != 0 {
				t.Fatalf("late rejections touched replacement: invalid=%t closes=%d", state.invalid, client.closes.Load())
			}
		})
	}
}

func TestSecondReviewQueued403DoesNotInvalidateRefresh(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	m.UpdateClearanceConfig(ClearanceConfig{Mode: "flaresolverr"})
	c := m.clearance
	cache := func(cookie string) {
		if !c.cacheClearance("direct", clearanceSolution{Cookies: cookie, UserAgent: "UA"}, time.Now(), c.clearanceVersion, "fp", "binding", time.Minute) {
			t.Fatal("cache failed")
		}
	}
	cache("old")
	entered, gate := make(chan struct{}), make(chan struct{})
	var gateOnce sync.Once
	unblock := func() { gateOnce.Do(func() { close(gate) }) }
	defer unblock()
	m.health.process = func(ctx context.Context, report healthReport) (domain.HealthState, error) {
		close(entered)
		select {
		case <-gate:
		case <-ctx.Done():
			return domain.HealthState{}, ctx.Err()
		}
		return m.persistHealthReport(ctx, report)
	}
	client := &reviewCountingClient{}
	lease := &Lease{Scope: domain.ScopeWeb, clearanceManager: m, clearanceKey: "direct", clearanceGeneration: c.generationFor("direct"), client: client}
	lease.InvalidateClearance()
	lease.Observe(http.StatusForbidden, nil)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("health worker did not start")
	}
	cache("fresh")
	unblock()
	if err := m.FlushFeedback(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.clearanceMu.Lock()
	state := c.clearances["direct"]
	c.clearanceMu.Unlock()
	if state.invalid || client.closes.Load() != 1 {
		t.Fatalf("queued health event repeated invalidation: invalid=%t closes=%d", state.invalid, client.closes.Load())
	}
}

func TestSecondReviewProbeOperationalFailures(t *testing.T) {
	for _, reason := range []string{"tasks", "shutdown", "canceled", "deadline", "invalid-config"} {
		t.Run(reason, func(t *testing.T) {
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			m := NewManager(egressRepositoryTestStub{}, cipher)
			defer m.Close(context.Background())
			ctx := context.Background()
			var cause error
			switch reason {
			case "tasks":
				for range 8 {
					done, err := m.tasks.begin("probe")
					if err != nil {
						t.Fatal(err)
					}
					defer done()
				}
				cause = netbudget.ErrCapacity
			case "shutdown":
				m.BeginDrain()
				cause = netbudget.ErrClosed
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				cause = context.Canceled
			case "deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				cause = context.DeadlineExceeded
			}
			node := domain.Node{ID: 7, EncryptedProxyURL: encryptedProxy(t, cipher, "http://proxy.invalid:8080")}
			if reason == "invalid-config" {
				node.EncryptedProxyURL = "damaged-encryption"
			}
			result, err := m.ProbeEgressNode(ctx, node)
			var executionErr *domain.ProbeExecutionError
			if !errors.As(err, &executionErr) || result.Status != domain.ProbeStatusUnknown {
				t.Fatalf("local %s became a health result: %+v %v", reason, result, err)
			}
			if cause != nil && !errors.Is(err, cause) {
				t.Fatalf("lost cause %v: %v", cause, err)
			}
		})
	}
}

func TestSecondReviewProbeCancelAndShutdownReleaseOwnership(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			entered := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-r.Context().Done()
			}))
			defer server.Close()
			m := NewManager(egressRepositoryTestStub{}, nil)
			defer m.Close(context.Background())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := m.probeEgressEndpoint(ctx, preparedEgressProbe{nodeID: 1}, domain.ProbeProviderCloudflare, "ipv4", server.URL)
				done <- err
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("probe did not start")
			}
			if shutdown {
				_ = m.Close(context.Background())
			} else {
				cancel()
			}
			select {
			case err := <-done:
				var executionErr *domain.ProbeExecutionError
				if !errors.As(err, &executionErr) {
					t.Fatalf("canceled probe became a network failure: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("probe failed to stop")
			}
			deadline := time.Now().Add(time.Second)
			for {
				s := m.RuntimeStats().Network
				if s.Requests == 0 && s.Clients == 0 && s.Connections == 0 && s.Dialing == 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("probe retained resources: %+v", s)
				}
				time.Sleep(time.Millisecond)
			}
		})
	}
}

func TestSecondReviewProxyPoolCapacityDoesNotRetryOrReportFailure(t *testing.T) {
	m := NewManager(egressRepositoryTestStub{}, nil)
	defer m.Close(context.Background())
	client := &scriptedRequestClient{do: func(_ int, _ *http.Request) (*http.Response, error) {
		return nil, fmt.Errorf("proxyconnect tcp: %w", netbudget.ErrCapacity)
	}}
	lease := &Lease{NodeID: 7, Scope: domain.ScopeBuild, client: client, proxyPool: true, clearanceManager: m}
	request, _ := http.NewRequest(http.MethodGet, "https://unused.invalid/", nil)
	_, err := lease.Do(request)
	lease.Observe(0, err)
	if err := m.FlushFeedback(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(err, netbudget.ErrCapacity) || client.calls != 1 || client.closedIdle != 0 {
		t.Fatalf("capacity retried or evicted proxy: calls=%d closes=%d err=%v", client.calls, client.closedIdle, err)
	}
	m.health.shard(7).mu.Lock()
	entries := len(m.health.shard(7).entries)
	m.health.shard(7).mu.Unlock()
	if entries != 0 {
		t.Fatal("capacity produced a health observation")
	}
}
