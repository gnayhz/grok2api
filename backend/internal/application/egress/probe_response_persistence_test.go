package egress_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	egressapp "github.com/chenyme/grok2api/backend/internal/application/egress"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/jackc/pgx/v5"
)

func TestCompleteProbeResponseControlsPersistedHealth(t *testing.T) {
	if os.Getenv("GROK_TEST_PROBE_BOUNDARY_CHILD") != "1" {
		certificate, key := probeBoundaryCertificate(t)
		command := exec.Command(os.Args[0], "-test.run=^TestCompleteProbeResponseControlsPersistedHealth$", "-test.v", "-test.count=1", "-test.timeout=3m")
		command.Env = append(os.Environ(), "GROK_TEST_PROBE_BOUNDARY_CHILD=1", "SSL_CERT_FILE="+certificate, "GROK_TEST_PROBE_BOUNDARY_KEY="+key)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated TLS probes: %v\n%s", err, output)
		}
		t.Log(string(output))
		return
	}
	for _, dialect := range []string{"sqlite", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			ctx := context.Background()
			database := probeBoundaryDatabase(t, dialect)
			repo := relational.NewEgressRepository(database)
			cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
			if err != nil {
				t.Fatal(err)
			}
			for _, format := range []string{"json", "trace"} {
				for _, healthy := range []string{"both", "ipv4", "ipv6", "neither"} {
					t.Run(format+"/"+healthy, func(t *testing.T) {
						service := egressapp.NewService(repo, cipher)
						defer service.Close(ctx)
						manager := infraegress.NewManager(repo, cipher)
						defer manager.Close(ctx)
						manager.SetLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
						service.SetNodeProber(manager)
						var calls atomic.Int32
						origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
							calls.Add(1)
							family := "ipv4"
							if strings.Contains(r.Host, "2606:") {
								family = "ipv6"
							}
							ip := "203.0.113.8"
							if family == "ipv6" {
								ip = "2001:db8::8"
							}
							body := fmt.Sprintf(`{"ip":%q}`, ip)
							if format == "trace" {
								body = "ip=" + ip + "\n"
							}
							body += strings.Repeat(" ", (64<<10)-len(body))
							if healthy != "both" && healthy != family {
								body += "!"
							}
							_, _ = io.WriteString(w, body)
						}))
						certificate, err := tls.LoadX509KeyPair(os.Getenv("SSL_CERT_FILE"), os.Getenv("GROK_TEST_PROBE_BOUNDARY_KEY"))
						if err != nil {
							t.Fatal(err)
						}
						origin.TLS = &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
						origin.StartTLS()
						defer origin.Close()
						proxy := probeBoundaryCONNECT(t, origin.Listener.Addr().String())
						node, err := service.Create(ctx, egressapp.Input{Name: format + "-" + healthy, Enabled: true, ProxyURL: &proxy})
						if err != nil {
							t.Fatal(err)
						}
						result, err := service.TestNode(ctx, node.ID)
						if err != nil {
							t.Fatal(err)
						}
						stored, err := repo.GetEgressNode(ctx, node.ID)
						if err != nil {
							t.Fatal(err)
						}
						wantStatus := domain.ProbeStatusHealthy
						if healthy == "neither" {
							wantStatus = domain.ProbeStatusUnhealthy
						}
						if result.Status != wantStatus || stored.ProbeStatus != wantStatus || calls.Load() != 2 {
							t.Errorf("completed probe: result=%+v storedStatus=%s calls=%d", result, stored.ProbeStatus, calls.Load())
						}
						for family, observed := range map[string]domain.ProbeFamilyResult{"ipv4": stored.IPv4Probe, "ipv6": stored.IPv6Probe} {
							valid := healthy == "both" || healthy == family
							if valid {
								if observed.Status != domain.ProbeStatusHealthy || observed.ExitIP == "" {
									t.Errorf("valid %s was lost: %+v", family, observed)
								}
							} else if observed.Status != domain.ProbeStatusUnhealthy || observed.ExitIP != "" {
								t.Errorf("oversized %s changed persisted identity: %+v", family, observed)
							}
						}
						if err := manager.Close(ctx); err != nil {
							t.Fatal(err)
						}
						stats := manager.RuntimeStats().Network
						if stats.Requests != 0 || stats.Clients != 0 || stats.Connections != 0 || stats.Dialing != 0 {
							t.Fatalf("persisted probe retained capacity: %+v", stats)
						}
					})
				}
			}
		})
	}
}

func probeBoundaryDatabase(t *testing.T, dialect string) *relational.Database {
	t.Helper()
	ctx := context.Background()
	var database *relational.Database
	var err error
	if dialect == "postgres" {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("requires isolated TEST_POSTGRES_DSN")
		}
		admin, connectErr := pgx.Connect(ctx, dsn)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		t.Cleanup(func() { _ = admin.Close(ctx) })
		schema := fmt.Sprintf("probe_boundary_%d", time.Now().UnixNano())
		if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
				t.Error(err)
			}
		})
		parsed, parseErr := url.Parse(dsn)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		query := parsed.Query()
		query.Set("search_path", schema)
		parsed.RawQuery = query.Encode()
		database, err = relational.OpenPostgres(ctx, parsed.String(), 8, 2)
	} else {
		database, err = relational.OpenSQLite(ctx, filepath.Join(t.TempDir(), "probe.db"))
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.InitializeSchema(ctx); err != nil {
		t.Fatal(err)
	}
	return database
}

func probeBoundaryCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated probe fixture"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true,
		IPAddresses: []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2606:4700:4700::1111")},
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	certPath, keyPath := filepath.Join(directory, "roots.pem"), filepath.Join(directory, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func probeBoundaryCONNECT(t *testing.T, target string) string {
	t.Helper()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || (r.Host != "1.1.1.1:443" && r.Host != "[2606:4700:4700::1111]:443") {
			http.Error(w, "unexpected fixture target", http.StatusBadGateway)
			return
		}
		upstream, err := net.Dial("tcp", target)
		if err != nil {
			http.Error(w, "fixture unavailable", http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		client, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			return
		}
		done := make(chan struct{})
		go func() { _, _ = io.Copy(upstream, buffered); _ = upstream.Close(); close(done) }()
		_, _ = io.Copy(client, upstream)
		_ = client.Close()
		<-done
	}))
	t.Cleanup(proxy.Close)
	return proxy.URL
}
