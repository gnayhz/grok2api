package cli

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/infra/provider/conversation"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

func TestNativeFilteringWaitsForAdmission(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		raw := `{"output":[{"type":"web_search_call","id":"internal"},{"type":"message","id":"visible","content":[{"type":"output_text","text":"answer"}]}]}`
		if streaming {
			raw = "data: {\"type\":\"response.completed\",\"response\":" + raw + "}\n\n"
		}
		body := &conversionReadCounter{Reader: strings.NewReader(raw)}
		response := &provider.Response{Header: make(http.Header), Body: body}
		prepareBuildClientConversion(response, provider.ResponseResourceRequest{Operation: conversation.OperationResponses, Streaming: streaming}, buildPromptCacheRoute{injectedToolTypes: map[string]struct{}{"web_search": {}}}, nil, conversation.ResponseOptions{})
		if body.reads.Load() != 0 {
			t.Fatal("preparation read raw response before admission")
		}
		var output []byte
		var err error
		if streaming {
			converted := response.ConvertStream(response.Body)
			output, err = io.ReadAll(converted)
			_ = converted.Close()
		} else {
			data, _ := io.ReadAll(response.Body)
			if string(data) != raw {
				t.Fatal("raw response changed before admission")
			}
			output, err = response.ConvertJSON(data)
			_ = response.Body.Close()
		}
		if err != nil || strings.Contains(string(output), "internal") || !strings.Contains(string(output), "visible") {
			t.Fatalf("streaming=%v output=%s err=%v", streaming, output, err)
		}
	}
}

type conversionReadCounter struct {
	io.Reader
	reads atomic.Int64
}

func (r *conversionReadCounter) Read(p []byte) (int, error) { r.reads.Add(1); return r.Reader.Read(p) }
func (*conversionReadCounter) Close() error                 { return nil }
