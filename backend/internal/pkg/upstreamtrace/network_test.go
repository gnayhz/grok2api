package upstreamtrace

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"net/http/httptrace"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

func TestNetworkOnlyMode(t *testing.T) {
	if os.Getenv("GROK2API_TEST_NETWORK_TRACE_CHILD") == "1" {
		if _, enabled := Enabled(); enabled {
			t.Fatal("network-only mode enabled raw content tracing")
		}
		ctx, finish := Network(context.Background(), "build", "responses")
		hooks := httptrace.ContextClientTrace(ctx)
		if hooks == nil {
			t.Fatal("network-only mode has no trace hooks")
		}
		hooks.GotConn(httptrace.GotConnInfo{Reused: true})
		finish()
		return
	}
	directory := t.TempDir()
	t.Setenv("GROK2API_TEST_NETWORK_TRACE_CHILD", "1")
	t.Setenv("GROK2API_UPSTREAM_TRACE_DIR", "")
	t.Setenv("GROK2API_UPSTREAM_NETWORK_TRACE_DIR", directory)
	command := exec.Command(os.Args[0], "-test.run=^TestNetworkOnlyMode$")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("network-only child: %v\n%s", err, output)
	}
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 {
		t.Fatalf("network-only reports: %d, err=%v", len(files), err)
	}
}

func TestTraceDirectories(t *testing.T) {
	for _, mode := range []string{"disabled", "raw", "network", "both", "invalid"} {
		t.Run(mode, func(t *testing.T) {
			raw, net := "", ""
			root := t.TempDir()
			if mode == "raw" || mode == "both" {
				raw = filepath.Join(root, "raw")
			}
			if mode == "network" || mode == "both" {
				net = filepath.Join(root, "network")
			}
			if mode == "invalid" {
				net = filepath.Join(root, "file")
				if err := os.WriteFile(net, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GROK2API_UPSTREAM_TRACE_DIR", raw)
			t.Setenv("GROK2API_UPSTREAM_NETWORK_TRACE_DIR", net)
			gotRaw, gotNetwork := traceDirectories()
			wantNetwork := net
			if net == "" {
				wantNetwork = raw
			} else if mode == "invalid" {
				wantNetwork = ""
			}
			if gotRaw != raw || gotNetwork != wantNetwork {
				t.Fatalf("directories = %q %q, want %q %q", gotRaw, gotNetwork, raw, wantNetwork)
			}
		})
	}
}

func TestNetworkReportComposesAndFinishesOnce(t *testing.T) {
	directory := t.TempDir()
	observed := 0
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { observed++ }})
	ctx, finish := network(ctx, "build", "responses", directory)
	hooks := httptrace.ContextClientTrace(ctx)
	hooks.GetConn("private-address-must-not-appear")
	hooks.TLSHandshakeDone(tls.ConnectionState{NegotiatedProtocol: "h2", DidResume: true}, nil)
	hooks.GotConn(httptrace.GotConnInfo{Reused: true})
	finish()
	finish()
	hooks.WroteRequest(httptrace.WroteRequestInfo{})
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 || observed != 1 {
		t.Fatalf("files=%d observed=%d err=%v", len(files), observed, err)
	}
	data, err := os.ReadFile(filepath.Join(directory, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private-address")) {
		t.Fatal("network report contains a destination address")
	}
	var report struct {
		Events []networkEvent `json:"events"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Events) != 3 || !report.Events[1].Resumed || !report.Events[2].Reused {
		t.Fatalf("events = %+v", report.Events)
	}
}

func TestNetworkLateCallbacksAreBounded(t *testing.T) {
	directory := t.TempDir()
	ctx, finish := network(context.Background(), "build", "responses", directory)
	hooks := httptrace.ContextClientTrace(ctx)
	var workers sync.WaitGroup
	for range 4 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 256 {
				hooks.GotConn(httptrace.GotConnInfo{Reused: true})
			}
			finish()
		}()
	}
	workers.Wait()
	files, err := os.ReadDir(directory)
	if err != nil || len(files) != 1 {
		t.Fatalf("concurrent finish reports: %d, err=%v", len(files), err)
	}
	data, err := os.ReadFile(filepath.Join(directory, files[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		Events []networkEvent `json:"events"`
	}
	if err := json.Unmarshal(data, &report); err != nil || len(report.Events) != 128 {
		t.Fatalf("bounded events: %d, err=%v", len(report.Events), err)
	}
}
