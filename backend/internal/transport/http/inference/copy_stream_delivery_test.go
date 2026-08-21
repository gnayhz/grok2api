package inference

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestCopyStreamDeliveredCounts 直接锁定 transport 交付计数（轮26：
// 活体发现流式 events=0，在本包直调 copyStream 复现链路）。
func TestCopyStreamDeliveredCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	sse := "data: {\"a\":1}\n\ndata: {\"b\":2}\n\ndata: [DONE]\n\n"
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	metadata, err := copyStreamWithFallbackModel(ctx.Writer, io.NopCloser(strings.NewReader(sse)), streamProtocolChat, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if metadata.DeliveredEvents != 3 {
		t.Fatalf("DeliveredEvents = %d, want 3", metadata.DeliveredEvents)
	}
	if metadata.DeliveredBytes != int64(len(sse)) {
		t.Fatalf("DeliveredBytes = %d, want %d", metadata.DeliveredBytes, len(sse))
	}
}
