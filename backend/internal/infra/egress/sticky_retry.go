package egress

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"

	"github.com/chenyme/grok2api/backend/internal/pkg/attemptmeta"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
)

// do retries only the connection phase of a proxy-pool request.
// Once the request is written to the upstream tunnel, replaying a POST could
// duplicate generation or billing and is therefore never attempted here.
func (l *Lease) do(request *http.Request) (*http.Response, error) {
	if l == nil || l.client == nil {
		return nil, errors.New("出口客户端未初始化")
	}
	if !l.proxyPool {
		return l.submitPhysical(request)
	}
	current := request
	for attempt := 0; ; attempt++ {
		if err := current.Context().Err(); err != nil {
			if current.Body != nil {
				_ = current.Body.Close()
			}
			return nil, err
		}
		var written atomic.Bool
		trace := &httptrace.ClientTrace{WroteHeaders: func() { written.Store(true) }, WroteRequest: func(httptrace.WroteRequestInfo) {
			// The callback also fires when writing fails after a partial write;
			// treat that as submitted because the upstream may have received it.
			written.Store(true)
		}}
		traced := current.WithContext(httptrace.WithClientTrace(current.Context(), trace))
		if attempt > 0 {
			path := attemptmeta.FromContext(request.Context()).Path
			traced = traced.WithContext(attemptmeta.Begin(WithPhysicalCallStage(traced.Context(), "connection_retry"), path))
		}
		response, err := l.submitPhysical(traced)
		if err == nil && !retryableResinResponse(response) {
			return response, nil
		}
		if attempt >= proxyPoolRetryLimit || written.Load() || !safeProxyConnectionFailure(err, response) {
			if safeProxyConnectionFailure(err, response) {
				l.client.CloseIdleConnections()
			}
			if err != nil {
				return nil, err
			}
			return response, nil
		}
		next, cloneErr := cloneRequestBody(request)
		if cloneErr != nil {
			l.client.CloseIdleConnections()
			if err != nil {
				return nil, err
			}
			return response, nil
		}
		if response != nil && response.Body != nil {
			_ = response.Body.Close()
		}
		l.client.CloseIdleConnections()
		current = next
	}
}

func (l *Lease) submitPhysical(request *http.Request) (*http.Response, error) {
	finish := func() {}
	if l.clientHandle != nil {
		ctx, done, err := l.clientHandle.begin(request.Context())
		if err != nil {
			if request.Body != nil {
				_ = request.Body.Close()
			}
			return nil, err
		}
		finish = done
		request = request.WithContext(ctx)
	}
	if err := beginPhysicalCall(request.Context()); err != nil {
		finish()
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, err
	}
	response, err := l.client.Do(request)
	if err != nil || response == nil || response.Body == nil {
		finish()
	} else {
		response.Body = &completionBody{ReadCloser: response.Body, finish: finish}
	}
	err = MarkPhysicalExecutionError(request.Context(), neterrorpkg.MarkTransport(err, neterrorpkg.PhaseRequest))
	if err == nil && response != nil && response.Body != nil {
		response.Body = &observedBody{ReadCloser: response.Body}
	}
	attemptmeta.Attach(response, request)
	recordPhysicalCall(request.Context(), response, err)
	if err != nil && response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return response, err
}

func cloneRequestBody(request *http.Request) (*http.Request, error) {
	if request == nil {
		return nil, errors.New("请求为空")
	}
	if request.Body == nil || request.Body == http.NoBody {
		return request.Clone(request.Context()), nil
	}
	if request.GetBody == nil {
		return nil, errors.New("请求体不可重放")
	}
	body, err := request.GetBody()
	if err != nil {
		return nil, err
	}
	cloned := request.Clone(request.Context())
	cloned.Body = body
	return cloned, nil
}

func safeProxyConnectionFailure(err error, response *http.Response) bool {
	if runtimeCapacityError(err) || errors.Is(err, context.Canceled) {
		return false
	}
	if response != nil {
		resinError := strings.ToUpper(strings.TrimSpace(response.Header.Get("X-Resin-Error")))
		return response.StatusCode >= http.StatusBadGateway && (resinError == "UPSTREAM_CONNECT_FAILED" || resinError == "NO_AVAILABLE_NODES")
	}
	if err == nil {
		return false
	}
	value := strings.ToLower(err.Error())
	for _, marker := range []string{
		"proxyconnect", "socks connect", "socks5: authentication", "tls handshake timeout",
		"connection refused", "no route to host",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	var tlsError *tls.RecordHeaderError
	return errors.As(err, &tlsError)
}

func retryableResinResponse(response *http.Response) bool {
	if response == nil {
		return false
	}
	resinError := strings.ToUpper(strings.TrimSpace(response.Header.Get("X-Resin-Error")))
	return (resinError == "UPSTREAM_CONNECT_FAILED" || resinError == "NO_AVAILABLE_NODES") && response.StatusCode >= 502
}
