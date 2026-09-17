package egress

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ControlTransportOwner lets the managed runtime account for control-plane
// connections created by control calls (rotation webhooks, subscription
// fetches). It mirrors the application-side owner contract structurally.
type ControlTransportOwner interface {
	ManageHTTPTransport(context.Context, *http.Transport) (http.RoundTripper, func(), error)
}

// RotationWebhookExecutor performs outbound rotation webhook calls. It owns
// the HTTP client, retry pacing and response draining; the caller keeps
// reservation, lease and binding-generation policy.
type RotationWebhookExecutor struct {
	owner ControlTransportOwner
}

func NewRotationWebhookExecutor(owner ControlTransportOwner) *RotationWebhookExecutor {
	return &RotationWebhookExecutor{owner: owner}
}

// Notify posts the rotation trigger with the configured timeout and bounded
// retries. The webhook endpoint may be private per the product contract, so
// no SSRF narrowing is applied here — unlike subscription fetches.
func (e *RotationWebhookExecutor) Notify(ctx context.Context, url string, timeout time.Duration, retries int) error {
	client := &http.Client{Timeout: timeout}
	if e.owner != nil {
		managed, closeTransport, err := e.owner.ManageHTTPTransport(ctx, http.DefaultTransport.(*http.Transport).Clone())
		if err != nil {
			return err
		}
		defer closeTransport()
		client.Transport = managed
	}
	var lastErr error
	for attempt := 0; attempt <= retries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader([]byte("{}")))
		if err != nil {
			return err
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		response.Body.Close()
		if response.StatusCode >= 200 && response.StatusCode < 300 {
			return nil
		}
		lastErr = fmt.Errorf("webhook HTTP %d", response.StatusCode)
	}
	return lastErr
}
