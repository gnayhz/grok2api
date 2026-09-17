package inference

import (
	"github.com/chenyme/grok2api/backend/internal/application/selector"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"testing"
	"time"
)

func TestSelectionRetryAfterDoesNotWrapLongestWait(t *testing.T) {
	for _, c := range []struct {
		delay time.Duration
		want  string
	}{
		{1500 * time.Millisecond, "2"},
		{time.Duration(1<<63 - 1), "9223372037"},
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		encodeClientError(ctx, classifyClientError(&selector.SelectionUnavailableError{Reason: selector.SelectionCooling, RetryAfter: c.delay}), false)
		if got := recorder.Header().Get("Retry-After"); got != c.want {
			t.Errorf("delay=%v got=%s want=%s", c.delay, got, c.want)
		}
	}
}
