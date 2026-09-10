package egress

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
)

func TestProbeResponseRequiresCompleteBoundedDocument(t *testing.T) {
	const limit = 64 << 10
	for _, format := range []string{"json", "trace"} {
		for _, family := range []string{"ipv4", "ipv6"} {
			for _, scenario := range []string{"ordinary", "exact_limit", "overflow", "overflow_then_read_error", "short_read_error"} {
				t.Run(format+"/"+family+"/"+scenario, func(t *testing.T) {
					ip := "203.0.113.8"
					if family == "ipv6" {
						ip = "2001:db8::8"
					}
					body := fmt.Sprintf(`{"ip":%q}`, ip)
					if format == "trace" {
						body = "ip=" + ip + "\n"
					}
					switch scenario {
					case "exact_limit", "overflow", "overflow_then_read_error":
						body += strings.Repeat(" ", limit-len(body))
					}
					if scenario == "overflow" || scenario == "overflow_then_read_error" {
						body += "!"
					}
					origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						if strings.HasSuffix(scenario, "read_error") {
							w.Header().Set("Content-Length", fmt.Sprint(len(body)+10))
						}
						_, _ = io.WriteString(w, body)
					}))
					defer origin.Close()
					manager := NewManager(egressRepositoryTestStub{}, nil)
					defer manager.Close(context.Background())
					manager.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
					result, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{nodeID: 1}, domain.ProbeProviderCloudflare, family, origin.URL)
					valid := scenario == "ordinary" || scenario == "exact_limit"
					if valid {
						if err != nil || result.Status != domain.ProbeStatusHealthy || result.ExitIP != ip {
							t.Errorf("complete document rejected: result=%+v error=%v", result, err)
						}
					} else if err == nil || result.Status == domain.ProbeStatusHealthy || result.ExitIP != "" {
						t.Errorf("incomplete document became a healthy exit: result=%+v error=%v", result, err)
					}
					if err := manager.Close(context.Background()); err != nil {
						t.Fatal(err)
					}
					stats := manager.RuntimeStats().Network
					if stats.Requests != 0 || stats.Clients != 0 || stats.Connections != 0 || stats.Dialing != 0 {
						t.Fatalf("probe retained network capacity after close: %+v", stats)
					}
				})
			}
		}
	}
}

func BenchmarkProbeCompleteResponse(b *testing.B) {
	for _, format := range []string{"json", "trace"} {
		for _, size := range []string{"ordinary", "exact_limit"} {
			b.Run(format+"/"+size, func(b *testing.B) {
				body := `{"ip":"203.0.113.8"}`
				if format == "trace" {
					body = "ip=203.0.113.8\n"
				}
				if size == "exact_limit" {
					body += strings.Repeat(" ", (64<<10)-len(body))
				}
				origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					_, _ = io.WriteString(w, body)
				}))
				defer origin.Close()
				manager := NewManager(egressRepositoryTestStub{}, nil)
				defer manager.Close(context.Background())
				manager.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					result, err := manager.probeEgressEndpoint(context.Background(), preparedEgressProbe{nodeID: 1}, domain.ProbeProviderCloudflare, "ipv4", origin.URL)
					if err != nil || result.Status != domain.ProbeStatusHealthy {
						b.Fatalf("probe: %+v, %v", result, err)
					}
				}
				b.StopTimer()
				b.ReportMetric(1, "requests/op")
			})
		}
	}
}
