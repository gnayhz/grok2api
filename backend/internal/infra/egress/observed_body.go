package egress

import (
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"io"
)

type observedBody struct{ io.ReadCloser }

func (b *observedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		err = neterrorpkg.MarkTransport(err, neterrorpkg.PhaseResponseBody)
	}
	return n, err
}
