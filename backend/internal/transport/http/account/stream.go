package account

import (
	"sync"
	"sync/atomic"
	"time"

	accountapp "github.com/chenyme/grok2api/backend/internal/application/account"
	"github.com/chenyme/grok2api/backend/internal/transport/http/httphelpers"
	"github.com/gin-gonic/gin"
)

type accountTaskProgressResponse struct {
	Completed int    `json:"completed"`
	Total     int    `json:"total"`
	Phase     string `json:"phase,omitempty"`
}

type accountEventStream struct {
	context   *gin.Context
	mu        sync.Mutex
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func newAccountEventStream(c *gin.Context) *accountEventStream {
	httphelpers.SSEHeaders(c)
	stream := &accountEventStream{context: c, stop: make(chan struct{}), done: make(chan struct{})}
	_ = stream.writeComment("connected")
	go stream.heartbeat()
	return stream
}

func (s *accountEventStream) ProgressObserver() accountapp.BatchProgressObserver {
	return s.PhaseProgressObserver("", nil)
}

func (s *accountEventStream) PhaseProgressObserver(phase string, totalValue *atomic.Int64) accountapp.BatchProgressObserver {
	return func(completed, total int) error {
		if totalValue != nil {
			totalValue.Store(int64(total))
		}
		return s.Write("progress", accountTaskProgressResponse{Completed: completed, Total: total, Phase: phase})
	}
}

func (s *accountEventStream) SyncProgressObserver() func(completed, total int) {
	return func(completed, total int) {
		_ = s.Write("progress", accountTaskProgressResponse{Completed: completed, Total: total, Phase: "syncing"})
	}
}

func (s *accountEventStream) WriteError(code, message string) {
	_ = s.Write("error", gin.H{"code": code, "message": message})
}

func (s *accountEventStream) Write(event string, value any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return httphelpers.SSEEvent(s.context, event, value)
}

func (s *accountEventStream) writeComment(comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return httphelpers.SSEComment(s.context, comment)
}

func (s *accountEventStream) heartbeat() {
	defer close(s.done)
	ticker := time.NewTicker(accountEventHeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.context.Request.Context().Done():
			return
		case <-ticker.C:
			if err := s.writeComment("heartbeat"); err != nil {
				return
			}
		}
	}
}

func (s *accountEventStream) Close() {
	s.closeOnce.Do(func() { close(s.stop) })
	<-s.done
}
