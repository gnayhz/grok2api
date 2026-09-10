package egress

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

type sourceBodyReadFunc func([]byte) (int, error)

func (f sourceBodyReadFunc) Read(p []byte) (int, error) { return f(p) }
func (f sourceBodyReadFunc) Close() error               { return nil }

func TestBrowserBodyCleanupPreservesTerminalReadMeaning(t *testing.T) {
	for _, mode := range []string{"normal_eof", "read_failure", "cancel_before", "cancel_during", "deadline_before", "explicit_close"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "deadline_before" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer stop()
			}
			localCtx, localCancel := context.WithCancel(ctx)
			defer localCancel()
			calls, cancels := 0, 0
			original := errors.New("underlying response body closed")
			var inner io.ReadCloser = sourceBodyReadFunc(func(p []byte) (int, error) {
				calls++
				if mode == "cancel_during" {
					cancel()
				}
				return 0, original
			})
			if mode == "normal_eof" {
				inner = io.NopCloser(strings.NewReader("body"))
			}
			body := &cancelOnCloseBody{ReadCloser: inner, ctx: localCtx, cancel: func() { cancels++; localCancel() }}
			if mode == "cancel_before" {
				cancel()
			}
			if mode == "explicit_close" {
				if err := body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			data, err := io.ReadAll(body)
			switch mode {
			case "normal_eof":
				if err != nil || string(data) != "body" {
					t.Fatalf("normal read: %q %v", data, err)
				}
				if _, err := body.Read(make([]byte, 1)); err != io.EOF {
					t.Fatalf("local cleanup changed repeated EOF: %v", err)
				}
			case "cancel_before", "cancel_during":
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation cause lost: %v", err)
				}
			case "deadline_before":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("deadline cause lost: %v", err)
				}
			default:
				if !errors.Is(err, original) {
					t.Fatalf("normal failure replaced: %v", err)
				}
			}
			if (mode == "cancel_before" || mode == "deadline_before") && calls != 0 {
				t.Fatal("read a body after request ended")
			}
			if err := body.Close(); err != nil {
				t.Fatal(err)
			}
			if cancels != 1 {
				t.Fatalf("cleanup calls=%d", cancels)
			}
		})
	}
}
