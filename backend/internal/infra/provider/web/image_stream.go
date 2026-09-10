package web

import (
	"context"
	"io"
	"sync"

	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/streampipe"
)

type imageStreamSource struct {
	io.Closer
	cancel context.CancelFunc
	budget *responsebuffer.Budget
	once   sync.Once
	err    error
}

func (s *imageStreamSource) ResponseBudget() *responsebuffer.Budget { return s.budget }

func (s *imageStreamSource) Close() error {
	s.once.Do(func() { s.cancel(); s.err = s.Closer.Close() })
	return s.err
}

func produceImageStream(ctx context.Context, input io.Closer, produce func(context.Context, io.Writer) error) io.ReadCloser {
	ctx, cancel := context.WithCancel(ctx)
	source := &imageStreamSource{Closer: input, cancel: cancel, budget: responsebuffer.FromContext(ctx)}
	return streampipe.Produce(source, func(writer io.Writer) error {
		stop := context.AfterFunc(ctx, func() { _ = source.Close() })
		defer stop()
		return produce(ctx, writer)
	})
}
