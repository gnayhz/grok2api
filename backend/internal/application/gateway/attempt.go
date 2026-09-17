package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
	"github.com/chenyme/grok2api/backend/internal/domain/audit"
	"github.com/chenyme/grok2api/backend/internal/pkg/jsonpeek"
	neterrorpkg "github.com/chenyme/grok2api/backend/internal/pkg/neterror"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsebuffer"
	"github.com/chenyme/grok2api/backend/internal/pkg/responsecheck"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
)

type failureAttemptRecorder struct {
	mu                  sync.Mutex
	method              string
	path                string
	remainingBodyBudget int
	attempts            []audit.Attempt
}

func newFailureAttemptRecorder(method, path string) *failureAttemptRecorder {
	return &failureAttemptRecorder{method: method, path: sanitizeRequestPath(path), remainingBodyBudget: diagnosticTotalBodyLimit}
}

const (
	diagnosticBodyLimit        = 64 << 10
	diagnosticTotalBodyLimit   = 256 << 10
	diagnosticTextLimit        = 2048
	diagnosticHeadersLimit     = 4 << 10
	diagnosticHeaderValueLimit = 512
	diagnosticErrorFrameLimit  = 8
)

var (
	diagnosticAuthorizationPattern = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]+`)
	diagnosticCookiePattern        = regexp.MustCompile(`(?i)\b(cookie|set-cookie)\b\s*[:=]\s*[^\r\n]+`)
	diagnosticSecretPattern        = regexp.MustCompile(`(?i)(["']?(?:authorization|proxy-authorization|x-api-key|api[_-]?key|access[_-]?token|refresh[_-]?token|id[_-]?token|client[_-]?secret|password|sso[_-]?token|session[_-]?token|token)["']?\s*[:=]\s*["']?)[^"'\s,;}]+`)
	diagnosticURLPattern           = regexp.MustCompile(`https?://[^\s"'<>]+`)
)

func (r *failureAttemptRecorder) captureCredentialFailure(credential accountdomain.Credential, startedAt time.Time, force bool, err error) {
	if err == nil {
		return
	}
	stage := "credential_validation"
	if force {
		stage = "credential_refresh"
	}
	r.append(audit.Attempt{
		Source:         audit.AttemptSourceCredential,
		Stage:          stage,
		AccountID:      auditAccountID(credential.ID),
		AccountName:    credential.Name,
		StartedAt:      startedAt.UTC(),
		DurationMS:     time.Since(startedAt).Milliseconds(),
		TransportError: sanitizeDiagnosticText(err.Error(), diagnosticTextLimit),
		ErrorChain:     errorFrames(err),
	})
}

func (r *failureAttemptRecorder) captureResponse(credential accountdomain.Credential, startedAt time.Time, response *provider.Response, requestErr error) error {
	if requestErr == nil && response != nil && response.RequestValidation != nil {
		return nil
	}
	if requestErr != nil {
		r.append(audit.Attempt{
			Source:         audit.AttemptSourceTransport,
			Stage:          transportStage(requestErr),
			AccountID:      auditAccountID(credential.ID),
			AccountName:    credential.Name,
			Method:         r.method,
			RequestPath:    r.path,
			UpstreamURL:    sanitizeUpstreamURL(errorUpstreamURL(requestErr)),
			StartedAt:      startedAt.UTC(),
			DurationMS:     time.Since(startedAt).Milliseconds(),
			TransportError: sanitizeDiagnosticText(requestErr.Error(), diagnosticTextLimit),
			ErrorChain:     errorFrames(requestErr),
		})
		return requestErr
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}

	statusCode := response.StatusCode
	status := response.Status
	headers := sanitizeDiagnosticHeaders(response.Header)
	var body []byte
	var bodyTruncated bool
	if response.Diagnostic != nil {
		statusCode = response.Diagnostic.StatusCode
		status = response.Diagnostic.Status
		headers = sanitizeDiagnosticHeaders(response.Diagnostic.Header)
		body, bodyTruncated = r.captureBody(response.Diagnostic.Body, response.Diagnostic.BodyTruncated)
	} else {
		var err error
		var replay io.ReadCloser
		body, replay, bodyTruncated, err = readResponseBody(response.Body)
		response.Body = replay
		body, bodyTruncated = r.captureBody(body, bodyTruncated)
		if err != nil {
			r.append(audit.Attempt{
				Source:                audit.AttemptSourceUpstreamHTTP,
				Stage:                 "response_body",
				AccountID:             auditAccountID(credential.ID),
				AccountName:           credential.Name,
				Method:                r.method,
				RequestPath:           r.path,
				UpstreamURL:           sanitizeUpstreamURL(response.UpstreamURL),
				StartedAt:             startedAt.UTC(),
				DurationMS:            time.Since(startedAt).Milliseconds(),
				UpstreamStatusCode:    &statusCode,
				UpstreamStatus:        status,
				ResponseHeaders:       headers,
				ResponseBody:          body,
				ResponseBodyTruncated: bodyTruncated,
				TransportError:        sanitizeDiagnosticText(err.Error(), diagnosticTextLimit),
				ErrorChain:            errorFrames(err),
			})
			return err
		}
	}
	r.append(audit.Attempt{
		Source:                audit.AttemptSourceUpstreamHTTP,
		Stage:                 "upstream_response",
		AccountID:             auditAccountID(credential.ID),
		AccountName:           credential.Name,
		Method:                r.method,
		RequestPath:           r.path,
		UpstreamURL:           sanitizeUpstreamURL(response.UpstreamURL),
		StartedAt:             startedAt.UTC(),
		DurationMS:            time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode:    &statusCode,
		UpstreamStatus:        status,
		ResponseHeaders:       headers,
		ResponseBody:          body,
		ResponseBodyTruncated: bodyTruncated,
	})
	return nil
}

func (r *failureAttemptRecorder) captureStreamFailure(credential accountdomain.Credential, startedAt time.Time, response *provider.Response, diagnostic StreamFailureDiagnostic) {
	if response == nil {
		return
	}
	statusCode := response.StatusCode
	body, bodyTruncated := r.captureBody(diagnostic.Body, diagnostic.BodyTruncated)
	r.append(audit.Attempt{
		Source:                audit.AttemptSourceUpstreamHTTP,
		Stage:                 "response_stream",
		AccountID:             auditAccountID(credential.ID),
		AccountName:           credential.Name,
		Method:                r.method,
		RequestPath:           r.path,
		UpstreamURL:           sanitizeUpstreamURL(response.UpstreamURL),
		StartedAt:             startedAt.UTC(),
		DurationMS:            time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode:    &statusCode,
		UpstreamStatus:        response.Status,
		ResponseHeaders:       sanitizeDiagnosticHeaders(response.Header),
		ResponseBody:          body,
		ResponseBodyTruncated: bodyTruncated,
	})
}

func (r *failureAttemptRecorder) captureQualityDegraded(credential accountdomain.Credential, startedAt time.Time, response *provider.Response, fp qualityHoldFingerprint) {
	// Upstream HTTP is still 200; the gateway withheld. Parent audit is the
	// request outcome (success after retry, or 503 if exhausted).
	status := http.StatusOK
	var headers map[string][]string
	if response != nil {
		headers = sanitizeDiagnosticHeaders(response.Header)
	}
	r.append(audit.Attempt{
		Source:             audit.AttemptSourceUpstreamHTTP,
		Stage:              "quality_hold",
		AccountID:          auditAccountID(credential.ID),
		AccountName:        credential.Name,
		Method:             r.method,
		RequestPath:        r.path,
		StartedAt:          startedAt.UTC(),
		DurationMS:         time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode: &status,
		UpstreamStatus:     "200 OK",
		ResponseHeaders:    headers,
		ResponseBody:       fp.json(),
		TransportError:     ErrorQualityDegraded,
	})
}

// captureQualityIdle 记录守卫空闲路径中止的尝试（empty/evidence-timeout/
// created-timeout）——此前该路径不写 attempt 明细，多账号轮换轨迹在审计
// 里不可见（round 41 活体发现 938 号审计零 attempt；对照 quality_hold 有）。
func (r *failureAttemptRecorder) captureQualityIdle(credential accountdomain.Credential, startedAt time.Time, errorCode string, response *provider.Response, fp qualityHoldFingerprint) {
	status := http.StatusOK
	if errorCode == "" {
		errorCode = ErrorQualityDegraded
	}
	var headers map[string][]string
	if response != nil {
		headers = sanitizeDiagnosticHeaders(response.Header)
	}
	r.append(audit.Attempt{
		Source:             audit.AttemptSourceUpstreamHTTP,
		Stage:              "quality_idle",
		AccountID:          auditAccountID(credential.ID),
		AccountName:        credential.Name,
		Method:             r.method,
		RequestPath:        r.path,
		StartedAt:          startedAt.UTC(),
		DurationMS:         time.Since(startedAt).Milliseconds(),
		UpstreamStatusCode: &status,
		UpstreamStatus:     "200 OK",
		ResponseHeaders:    headers,
		ResponseBody:       fp.json(),
		TransportError:     errorCode,
	})
}

func (r *failureAttemptRecorder) append(attempt audit.Attempt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	attempt.Number = len(r.attempts) + 1
	r.attempts = append(r.attempts, attempt)
}

// captureBody 在单次和单请求预算内保留可读的脱敏正文片段。
func (r *failureAttemptRecorder) captureBody(body []byte, alreadyTruncated bool) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(body) == 0 {
		return nil, alreadyTruncated
	}
	if !utf8.Valid(body) {
		return nil, true
	}
	limit := min(diagnosticBodyLimit, r.remainingBodyBudget)
	if limit <= 0 {
		return nil, true
	}
	truncated := alreadyTruncated || len(body) > limit
	if len(body) > limit {
		body = body[:limit]
	}
	result := []byte(sanitizeDiagnosticText(string(body), limit))
	r.remainingBodyBudget -= len(result)
	return result, truncated
}

func (r *failureAttemptRecorder) snapshot() []audit.Attempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]audit.Attempt(nil), r.attempts...)
}

func auditAccountID(id uint64) *uint64 {
	if id == 0 {
		return nil
	}
	return &id
}

type replayReadCloser struct {
	io.Reader
	source io.Closer
}

func (r *replayReadCloser) Close() error { return r.source.Close() }

func qualityPeekProtocol(operation audit.Operation, response *provider.Response) string {
	if response != nil && (response.ConvertStream != nil || response.ConvertJSON != nil) {
		return qualityProtocolResponses
	}
	return qualityProtocolForOperation(operation)
}

var errResponseConversion = errors.New("upstream response protocol conversion failed")
var errResponseTerminalFailure = errors.New("upstream response did not complete successfully")

// checkBufferedCompletion observes a completed JSON response independently of
// admission. A visible thinking field cannot turn a failed or partial terminal
// state into a successful delivery, nor can conversion erase that state.
func checkBufferedCompletion(data []byte) error {
	if !jsonpeek.Valid(data) {
		return nil // Syntax/shape validation belongs to the decoder/converter.
	}
	var status, responseType []byte
	var hasError bool
	jsonpeek.ObjectFields(data, func(key, value []byte) bool {
		switch string(key) {
		case "status":
			status = jsonpeek.UnquoteBytes(value)
		case "type":
			responseType = jsonpeek.UnquoteBytes(value)
		case "error":
			hasError = !bytes.Equal(bytes.TrimSpace(value), []byte("null"))
		}
		return true
	})
	if hasError || string(responseType) == "error" {
		return errResponseTerminalFailure
	}
	switch string(status) {
	case "failed", "incomplete", "cancelled", "canceled", "queued", "in_progress":
		return errResponseTerminalFailure
	}
	return responsecheck.JSON(data)
}

// applyDeferredStreamConversion prepares the client protocol after admission.
// A conversion failure cannot fall back to a different protocol or an empty
// successful response. Callers must abort before confirming cache writes.
func prepareResponseDelivery(response *provider.Response, streaming bool, generation *textGeneration) error {
	if response == nil {
		return nil
	}
	if !streaming && response.StatusCode >= 200 && response.StatusCode < 300 && response.Body != nil {
		if _, release, buffered := responsebuffer.Borrow(response.Body); buffered {
			release()
		} else {
			body, err := responsebuffer.ReadAll(response.Body, responsebuffer.BudgetOf(response.Body), responsebuffer.JSONLimit)
			_ = response.Body.Close()
			response.Body = body
			if err != nil {
				return errors.Join(errResponseConversion, err)
			}
		}
	}
	if data, release, buffered := responsebuffer.Borrow(response.Body); buffered {
		generation.observeJSON(response, data)
		release()
	}
	return applyDeferredStreamConversion(response)
}

func applyDeferredStreamConversion(response *provider.Response) error {
	if response == nil {
		return nil
	}
	if data, release, buffered := responsebuffer.Borrow(response.Body); buffered {
		err := checkBufferedCompletion(data)
		release()
		if err != nil {
			return err
		}
	}
	if response.ConvertStream != nil && response.ConvertJSON != nil {
		return errResponseConversion
	}
	if (response.ConvertStream != nil || response.ConvertJSON != nil) && response.Body == nil {
		return errResponseConversion
	}
	if response.ConvertStream != nil {
		converted := response.ConvertStream(response.Body)
		if converted == nil {
			return errResponseConversion
		}
		response.Body = converted
		response.ConvertStream = nil
	}
	if response.ConvertJSON == nil {
		return nil
	}
	convert := response.ConvertJSON
	response.ConvertJSON = nil
	budget := responsebuffer.BudgetOf(response.Body)
	data, release, buffered := responsebuffer.Borrow(response.Body)
	if !buffered {
		body, err := responsebuffer.ReadAll(response.Body, budget, responsebuffer.JSONLimit)
		_ = response.Body.Close()
		response.Body = body
		if err != nil {
			return errors.Join(errResponseConversion, err)
		}
		data, release, _ = body.BorrowBytes()
	}
	defer release()
	_ = response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(nil))
	workspace, workspaceErr := responsebuffer.JSONWorkspace(budget, data)
	if workspaceErr != nil {
		return errors.Join(errResponseConversion, workspaceErr)
	}
	defer workspace.Release()
	if err := checkBufferedCompletion(data); err != nil {
		return err
	}
	converted, convErr := convert(data)
	if convErr != nil {
		return errors.Join(errResponseConversion, convErr)
	}
	if len(converted) == 0 {
		return errResponseConversion
	}
	output := responsebuffer.New(budget, responsebuffer.JSONLimit)
	if _, err := output.Write(converted); err != nil {
		_ = output.Close()
		return errors.Join(errResponseConversion, err)
	}
	response.Body = output.Body()
	if response.Header == nil {
		response.Header = http.Header{}
	}
	response.Header.Set("Content-Length", strconv.Itoa(len(converted)))
	response.Header.Set("Content-Type", "application/json")
	return nil
}

// readResponseBody 只读取诊断上限，同时把已读取前缀接回原始响应供后续错误处理。
func readResponseBody(body io.ReadCloser) ([]byte, io.ReadCloser, bool, error) {
	if body == nil {
		return nil, io.NopCloser(bytes.NewReader(nil)), false, nil
	}
	data, err := io.ReadAll(io.LimitReader(body, diagnosticBodyLimit+1))
	truncated := len(data) > diagnosticBodyLimit
	if truncated || err != nil {
		captured := data
		if len(captured) > diagnosticBodyLimit {
			captured = captured[:diagnosticBodyLimit]
		}
		replay := &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(data), body), source: body}
		return captured, replay, truncated, err
	}
	closeErr := body.Close()
	return data, io.NopCloser(bytes.NewReader(data)), false, closeErr
}

func errorFrames(err error) []audit.ErrorFrame {
	frames := make([]audit.ErrorFrame, 0, 4)
	appendErrorFrames(&frames, err)
	return frames
}

func appendErrorFrames(frames *[]audit.ErrorFrame, err error) {
	if err == nil || len(*frames) >= diagnosticErrorFrameLimit {
		return
	}
	*frames = append(*frames, audit.ErrorFrame{Type: truncateDiagnosticText(reflect.TypeOf(err).String(), 256), Message: sanitizeDiagnosticText(err.Error(), 512)})
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, nested := range joined.Unwrap() {
			appendErrorFrames(frames, nested)
		}
		return
	}
	appendErrorFrames(frames, errors.Unwrap(err))
}

func sanitizeDiagnosticHeaders(headers http.Header) map[string][]string {
	result := make(map[string][]string)
	remaining := diagnosticHeadersLimit
	for name, values := range headers {
		if remaining <= 0 {
			break
		}
		lowerName := strings.ToLower(name)
		if !isAllowedDiagnosticHeader(lowerName) {
			continue
		}
		cleanValues := make([]string, 0, min(len(values), 8))
		for _, value := range values {
			if len(cleanValues) == 8 || remaining <= len(name) {
				break
			}
			cleanValue := sanitizeDiagnosticText(value, min(diagnosticHeaderValueLimit, remaining-len(name)))
			cleanValues = append(cleanValues, cleanValue)
			remaining -= len(name) + len(cleanValue)
		}
		if len(cleanValues) > 0 {
			result[http.CanonicalHeaderKey(name)] = cleanValues
		}
	}
	return result
}

func isAllowedDiagnosticHeader(name string) bool {
	if strings.HasPrefix(name, "x-ratelimit-") || strings.HasPrefix(name, "ratelimit-") {
		return true
	}
	switch name {
	// cf-mitigated 是 Cloudflare 拦截的直接标记（challenge/block）——
	// 403 归因的关键证据：存在=CF 层拦截，缺失=源站策略（round 54，视频
	// 403 诊断因该头不在白名单而丢失现场）。cf-cache-status 辅助判定
	// 响应是否来自边缘缓存。
	case "content-length", "content-type", "date", "retry-after", "server", "cf-ray", "cf-mitigated", "cf-cache-status", "request-id", "traceparent", "tracestate", "via", "x-correlation-id", "x-request-id", "x-grok2api-compatibility-warnings":
		return true
	default:
		return false
	}
}

func sanitizeDiagnosticText(value string, limit int) string {
	value = diagnosticCookiePattern.ReplaceAllString(value, "$1: [REDACTED]")
	value = diagnosticAuthorizationPattern.ReplaceAllString(value, "$1 [REDACTED]")
	value = diagnosticSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
	value = redactURLEncodedSecretPairs(value)
	value = diagnosticURLPattern.ReplaceAllStringFunc(value, sanitizeUpstreamURL)
	return truncateDiagnosticText(value, limit)
}

// redactURLEncodedSecretPairs 处理 %3D（'=' 编码）的敏感对：明文模式匹配
// 不到 key%3Dvalue 形态（round 23 PoC 实证 access_token%3Dsecret 原样存活）。
// 命中敏感键的片段键保留、值 [REDACTED]；不含 %3d 或无敏感键的文本零改动
// （普通 %XX 转义的 URL 不受影响）。camelCase 明文形态由既有模式的裸 token
// 子词 + (?i) 覆盖（PoC 实证，清单的 camelCase 缺口描述不成立）。
var diagnosticURLEncodedSecretPattern = regexp.MustCompile(`(?i)(["']?(?:access[_-]?token|refresh[_-]?token|id[_-]?token|sso[_-]?token|session[_-]?token|client[_-]?secret|api[_-]?key|x-api-key|password)["']?%3D)[^&\s"'<>]+`)

func redactURLEncodedSecretPairs(value string) string {
	if !strings.Contains(strings.ToLower(value), "%3d") {
		return value
	}
	return diagnosticURLEncodedSecretPattern.ReplaceAllString(value, "$1[REDACTED]")
}

func truncateDiagnosticText(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(value) <= limit {
		return value
	}
	for limit > 0 && !utf8.ValidString(value[:limit]) {
		limit--
	}
	return value[:limit]
}

func sanitizeRequestPath(value string) string {
	parsed, err := url.ParseRequestURI(value)
	if err != nil {
		return truncateDiagnosticText(strings.SplitN(value, "?", 2)[0], 2048)
	}
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	return truncateDiagnosticText(parsed.String(), 2048)
}

func sanitizeUpstreamURL(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.ForceQuery = false
	parsed.Fragment = ""
	parsed.RawFragment = ""
	return truncateDiagnosticText(parsed.String(), 4096)
}

func errorUpstreamURL(err error) string {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.URL
	}
	return ""
}

func transportStage(err error) string {
	switch {
	case neterrorpkg.IsResponseHeaderTimeout(err):
		return "response_header_timeout"
	case errors.Is(err, context.Canceled):
		return "request_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "request_timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns_lookup"
	}
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return "tls_verification"
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return "tls_verification"
	}
	var recordHeaderError tls.RecordHeaderError
	if errors.As(err, &recordHeaderError) {
		return "tls_handshake"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "network_timeout"
	}
	var operationError *net.OpError
	if errors.As(err, &operationError) && operationError.Op != "" {
		return operationError.Op
	}
	return "transport"
}
