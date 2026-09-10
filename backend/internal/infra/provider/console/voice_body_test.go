package console

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestVoiceBodyRequiresEOFWithinLimit(t *testing.T) {
	const limit = 4096
	for _, size := range []int{0, limit - 1, limit, limit + 1, limit * 4} {
		body := bytes.NewReader(bytes.Repeat([]byte{' '}, size))
		data, err := readConsoleVoiceBody(body, limit)
		if size <= limit {
			if err != nil || len(data) != size || body.Len() != 0 {
				t.Fatalf("complete body size=%d got=%d unread=%d err=%v", size, len(data), body.Len(), err)
			}
		} else if err == nil || data != nil || body.Size()-int64(body.Len()) != limit+1 {
			t.Fatalf("oversized body size=%d accepted=%d unread=%d err=%v", size, len(data), body.Len(), err)
		}
	}
	for _, cause := range []error{io.ErrUnexpectedEOF, errors.New("read canceled")} {
		body := io.MultiReader(bytes.NewReader([]byte(`{"text":"prefix"}`)), voiceReadFailure{cause})
		data, err := readConsoleVoiceBody(body, limit)
		if !errors.Is(err, cause) || data != nil {
			t.Fatalf("partial body escaped after read failure: data=%q err=%v", data, err)
		}
	}
}

type voiceReadFailure struct{ err error }

func (r voiceReadFailure) Read([]byte) (int, error) { return 0, r.err }
