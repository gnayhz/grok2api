package egress

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/pkg/netbudget"
	"github.com/gin-gonic/gin"
)

func TestProbeExecutionFailureReturnsServiceUnavailable(t *testing.T) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/v1/egress-nodes/1/test", nil)
	h := &Handler{}
	h.writeError(c, &domain.ProbeExecutionError{Err: fmt.Errorf("proxy credential=private-detail: %w", netbudget.ErrCapacity)})
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "egressProbeUnavailable") {
		t.Fatalf("incomplete probe API response: %d %s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "private-detail") {
		t.Fatal("probe setup details escaped into API response")
	}
}
