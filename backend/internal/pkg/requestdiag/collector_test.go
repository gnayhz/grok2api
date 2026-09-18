package requestdiag

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
)

func TestPromptPrefixesStayComparableWithoutRetainingContent(t *testing.T) {
	ctx := WithCollector(context.Background())
	a := WithPrompt(ctx, []byte(`{"instructions":"fictional secret","prompt_cache_key":"private-session","input":[{"content":"fictional message","n":9007199254740993}],"tools":[]}`))
	b := WithPrompt(ctx, []byte(`{"tools":[],"prompt_cache_key":"private-session","instructions":"fictional secret","input":[{"content":"fictional message","n":9007199254740993},{"content":"next"}]}`))
	pa := a.Value(promptKeyContext{}).(*audit.PromptDiagnostic)
	pb := b.Value(promptKeyContext{}).(*audit.PromptDiagnostic)
	if pa.Instructions != pb.Instructions || pa.Tools != pb.Tools || pa.Session != pb.Session || pa.Prefixes[0] != pb.Prefixes[0] || pb.InputItems != 2 {
		t.Fatal("append changed preceding prefix")
	}
	c := WithPrompt(ctx, []byte(`{"input":[{"content":"fictional message","n":9007199254740992}]}`)).Value(promptKeyContext{}).(*audit.PromptDiagnostic)
	if pa.Prefixes[0].Digest == c.Prefixes[0].Digest {
		t.Fatal("numeric change disappeared")
	}
	raw, _ := json.Marshal(pb)
	for _, secret := range []string{"fictional", "private-session", "9007199254740993"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("content retained")
		}
	}
	items := make([]string, 1000)
	for i := range items {
		items[i] = fmt.Sprintf(`{"n":%d}`, i)
	}
	large := WithPrompt(ctx, []byte(`{"input":[`+strings.Join(items, ",")+`]}`)).Value(promptKeyContext{}).(*audit.PromptDiagnostic)
	if len(large.Prefixes) != 64 || large.Prefixes[32].Items != 969 || large.Prefixes[63].Items != 1000 {
		t.Fatal("unbounded or incorrect prefix ring")
	}
}

func TestNetworkRecordsActualReusedConnectionAndAttempts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	ctx := WithCollector(attemptmeta.WithRequest(context.Background(), "synthetic-request", 1, "test", nil))
	ctx = attemptmeta.WithAccount(ctx, 7, "grok_build", "grok-4.6")
	for range 2 {
		call, finish := Network(attemptmeta.Begin(ctx, attemptmeta.Path{NodeID: 3}), "build", "primary")
		req, _ := http.NewRequestWithContext(call, http.MethodGet, server.URL, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		finish(resp.StatusCode, nil)
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		finish(500, fmt.Errorf("ignored repeated finish"))
	}
	v := Snapshot(ctx)
	if len(v.Exchanges) != 2 || v.Exchanges[0].PhysicalID == v.Exchanges[1].PhysicalID {
		t.Fatal("physical attempts merged")
	}
	reused := false
	for _, e := range v.Exchanges[1].Events {
		if e.Stage == "got_conn" {
			reused = e.Reused
		}
	}
	if !reused {
		t.Fatal("real keepalive reuse missing")
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), server.URL) {
		t.Fatal("endpoint retained")
	}
}

func TestCollectorConcurrentBoundedSnapshots(t *testing.T) {
	ctx := WithCollector(context.Background())
	var wg sync.WaitGroup
	for range 200 {
		wg.Go(func() {
			Stage(ctx, "selection", time.Now())
			Failure(ctx, "history", "store", "store_lock")
			Snapshot(ctx)
		})
	}
	wg.Wait()
	v := Snapshot(ctx)
	if len(v.Failures) != 8 || !v.Truncated || v.Stages[0].Calls != 200 {
		t.Fatal("collector limit or accounting incorrect")
	}
}

func BenchmarkPromptDiagnostics(b *testing.B) {
	for _, size := range []int{1 << 20, 32 << 20} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			payload := []byte(`{"input":[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", size) + `"}]}`)
			b.ReportAllocs()
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for range b.N {
				WithPrompt(WithCollector(context.Background()), payload)
			}
		})
	}
}
