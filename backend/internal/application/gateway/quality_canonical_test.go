package gateway

import (
	"context"
	"errors"
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responseflow"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestCanonicalAdmissionSharesRawEventsWithClientEncoder(t *testing.T) {
	pool := responsebuffer.NewPool(1 << 20)
	raw := "\xef\xbb\xbfevent: response.reasoning_text.delta\r\ndata: {\"type\":\"response.reasoning_text.delta\",\r\ndata: \"delta\":\"plan\"}\r\n\r\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	stream := responseflow.New(io.NopCloser(strings.NewReader(raw)), pool.Request(1<<20))
	observed := 0
	stream.Observe(func(*responseflow.Event) { observed++ })
	_, resources := selector.NewAttemptResources(context.Background())
	replay, verdict, _, _, err := peekQualityStreamReport(context.Background(), resources.Own(stream), qualityProtocolResponses, QualityRetryRuntime{})
	if err != nil || verdict != QualityDeliver || observed != 1 {
		t.Fatalf("verdict=%s err=%v observed=%d", verdict, err, observed)
	}
	response := &provider.Response{Body: replay, ConvertStream: func(body io.ReadCloser) io.ReadCloser {
		return conversation.ConvertResponseStreamWithOptions(body, conversation.OperationChat, conversation.ResponseOptions{})
	}}
	if err := applyDeferredStreamConversion(response); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	resources.Close()
	if err != nil || !strings.Contains(string(data), "plan") || observed != 2 {
		t.Fatalf("converted=%s err=%v observed=%d", data, err, observed)
	}
	if pool.Snapshot().Used != 0 {
		t.Fatal("canonical converted stream retained bytes")
	}
}

func TestCanonicalAdmissionStopsAtFirstDecisiveEvent(t *testing.T) {
	degraded := "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"reasoning\"}}\n\n"
	healthy := "data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"late plan\"}\n\n"
	pool := responsebuffer.NewPool(1 << 20)
	stream := responseflow.New(io.NopCloser(strings.NewReader(degraded+healthy)), pool.Request(1<<20))
	observed := 0
	stream.Observe(func(*responseflow.Event) { observed++ })
	body, verdict, _, _, err := peekQualityStreamReport(context.Background(), stream, qualityProtocolResponses, QualityRetryRuntime{})
	_ = body.Close()
	if err != nil || verdict != QualityWithhold || observed != 1 {
		t.Fatalf("verdict=%s err=%v observed=%d", verdict, err, observed)
	}
	if pool.Snapshot().Used != 0 {
		t.Fatal("withheld canonical stream retained bytes")
	}
}

func TestCanonicalAdmissionDeadlineClosesIncompleteEvent(t *testing.T) {
	pool := responsebuffer.NewPool(1 << 20)
	reader, writer := io.Pipe()
	stream := responseflow.New(reader, pool.Request(1<<20))
	go func() { _, _ = writer.Write([]byte("data: {\"type\":\"response.created\"}")) }()
	body, verdict, _, _, err := peekQualityStreamReport(context.Background(), stream, qualityProtocolResponses, QualityRetryRuntime{CreatedTimeout: 20 * time.Millisecond, EvidenceTimeout: time.Second})
	_ = body.Close()
	_ = writer.Close()
	if verdict != QualityWait || !errors.Is(err, errQualityCreatedTimeout) {
		t.Fatalf("verdict=%s err=%v", verdict, err)
	}
	if pool.Snapshot().Used != 0 {
		t.Fatal("timeout retained bytes")
	}
}
