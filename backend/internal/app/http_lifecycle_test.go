package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientkeyapp "github.com/chenyme/grok2api/backend/internal/application/clientkey"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/infra/config"
	"github.com/chenyme/grok2api/backend/internal/infra/persistence/relational"
)

func newLifecycleApplication(t *testing.T, configure ...func(*config.Config)) *Application {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	raw := "secrets:\n  jwtSecret: '12345678901234567890123456789012'\n  credentialEncryptionKey: 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA='\n"
	if err := os.WriteFile(configPath, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg.BootstrapAdmin.Username = "test-admin"
	cfg.BootstrapAdmin.Password = "fixture-admin-password"
	disabled := false
	cfg.Server.UpdateCheckEnabled = &disabled
	for _, apply := range configure {
		apply(&cfg)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Server.Listen = listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := New(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	a.httpDrainTimeout = 100 * time.Millisecond
	return a
}

func lifecycleGET(t *testing.T, a *Application) *http.Response {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := client.Get("http://" + a.server.Addr)
		if err == nil {
			return response
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunCancelsLongHTTPAndAcceptsFinalAuditBeforeStopping(t *testing.T) {
	a := newLifecycleApplication(t)
	key, err := a.clientKeys.Create(context.Background(), clientkeyapp.CreateInput{Name: "shutdown-billing", Enabled: true, BillingLimitUSDTicks: 30})
	if err != nil {
		t.Fatal(err)
	}
	if reserved, err := a.clientKeys.ReserveBilling(context.Background(), key.Key, "evt_http_shutdown", 20, time.Minute); err != nil || !reserved {
		t.Fatalf("reserve final bill: %v %v", reserved, err)
	}
	release := make(chan struct{})
	finished := make(chan error, 1)
	a.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-release:
		}
		finishCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		finished <- a.audits.Create(finishCtx, audit.Record{EventID: "evt_http_shutdown", RequestID: "http-shutdown", ClientKeyID: key.Key.ID, ModelRouteID: 1, Provider: "grok_build", Operation: "responses", UsageSource: "upstream", EstimatedCostInUSDTicks: 7, StatusCode: 499})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()
	response := lifecycleGET(t, a)
	defer response.Body.Close()
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(22 * time.Second):
		close(release)
		_ = a.server.Close()
		t.Fatal("Run did not finish within shutdown budget")
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("request completion was rejected before writer shutdown: %v", err)
		}
	default:
		close(release)
		err := <-finished
		_ = a.server.Close()
		t.Fatalf("Run returned with a live handler; its later audit result: %v", err)
	}
	rows, count, err := relational.NewAuditRepository(a.database).List(context.Background(), 0, 10)
	if err != nil || count != 1 || len(rows) != 1 || rows[0].EventID != "evt_http_shutdown" {
		t.Fatalf("final audit not settled: rows=%v count=%d err=%v", rows, count, err)
	}
	stored, err := a.clientKeys.Get(context.Background(), key.Key.ID)
	if err != nil || stored.BilledUsageUSDTicks != 7 || stored.ReservedUsageUSDTicks != 0 {
		t.Fatalf("final bill or reservation lost: %+v %v", stored, err)
	}
}
