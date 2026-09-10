package inference

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chenyme/grok2api/backend/internal/application/gateway"
	"github.com/chenyme/grok2api/backend/internal/transport/http/middleware"
	"github.com/gin-gonic/gin"
)

func TestMediaEncodingFinishesBeforeResultFinalization(t *testing.T) {
	for _, failure := range []string{"none", "body", "trailer"} {
		t.Run(failure, func(t *testing.T) {
			output := &encodingFailureWriter{ResponseWriter: httptest.NewRecorder(), failure: failure}
			source := &writeFailureSource{data: `{"text":"media transcript"}`}
			code := "unfinalized"
			var stats gateway.DeliveryStats
			finalized, recorded := 0, 0
			router := gin.New()
			router.Use(middleware.Gzip())
			router.GET("/", func(c *gin.Context) {
				(&Handler{}).writeMediaResult(c, &gateway.Result{
					StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: source,
					RecordDelivery: func(value gateway.DeliveryStats) {
						stats = value
						recorded++
						if value.Bytes != int64(output.accepted) {
							t.Error("delivery recorded before encoder completed")
						}
					},
					Finalize: func(_ gateway.Usage, _, value string) {
						code = value
						finalized++
						if recorded != 1 {
							t.Error("finalized before delivery record")
						}
					},
				})
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept-Encoding", "gzip")
			router.ServeHTTP(output, req)
			want := ""
			if failure != "none" {
				want = "client_disconnected"
			}
			if !source.closed || finalized != 1 || recorded != 1 || code != want || stats.Bytes != int64(output.accepted) {
				t.Fatalf("encoding finalization: closed=%v finalized=%d recorded=%d code=%q bytes=%d actual=%d", source.closed, finalized, recorded, code, stats.Bytes, output.accepted)
			}
			if failure != "none" && !output.failed {
				t.Fatal("write fault not reached")
			}
			if failure != "none" && output.Header().Get(mediaTransferErrorTrailer) != want {
				t.Errorf("late media encoding error missing from declared trailer: %s", output.Header().Get(mediaTransferErrorTrailer))
			}
		})
	}
}

func TestGzipStreamFlushFailureReachesResultFinalization(t *testing.T) {
	output := &flushFailureWriter{ResponseRecorder: httptest.NewRecorder(), failure: io.ErrClosedPipe}
	code := "unfinalized"
	marked := false
	source := &writeFailureSource{data: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"}
	router := gin.New()
	router.Use(middleware.Gzip())
	router.GET("/", func(c *gin.Context) {
		(&Handler{}).writeProtocolResult(c, &gateway.Result{
			StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: source,
			MarkFirstToken: func() { marked = true },
			Finalize:       func(_ gateway.Usage, _, value string) { code = value },
		}, true, false, streamProtocolChat, "")
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	router.ServeHTTP(output, req)
	if code != "client_disconnected" || marked || source.reads != 1 || !source.closed {
		t.Fatalf("gzip masked stream error: code=%q token=%v reads=%d closed=%v", code, marked, source.reads, source.closed)
	}
}
