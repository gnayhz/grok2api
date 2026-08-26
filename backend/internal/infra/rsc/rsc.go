// Package rsc classifies registration risk for a Web SSO identity.
//
// The primary transport is the SSO thinking probe (SSOProbeChecker, see
// ssoprobe.go): since grok.com stopped rendering botFlag fields into its
// Next.js RSC payload, the surviving account-level signal is the reasoning
// stream of a real (temporary) mgw conversation. The legacy homepage RSC
// payload parser below is a Go port of the regc verify --checks rsc
// implementation and stays available for rollback via
// accountRisk.rscCheck.method=homepage.
//
// Risk verdicts never recover, so callers may cache a denied/flagged result
// forever.
package rsc

import (
	"context"
	"fmt"
	"io"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tlsclient "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

// Verdict classifies one RSC check.
type Verdict string

const (
	// VerdictClean means a real grok.com homepage exposed no bot-flag fields.
	VerdictClean Verdict = "clean"
	// VerdictDenied means policy=deny with the $registration event.
	VerdictDenied Verdict = "denied"
	// VerdictFlagged means botFlagSource 1 or 2 without an explicit deny.
	VerdictFlagged Verdict = "flagged"
	// VerdictError means the check could not classify (transport, challenge,
	// or structure change). It must never be treated as clean.
	VerdictError Verdict = "error"
)

// Result is the outcome of one RSC check.
type Result struct {
	Verdict        Verdict
	BotFlagSource  int
	BotFlagDetails string
	RiskScore      float64
	HasRiskScore   bool
	HTTPStatus     int
	Error          string
	CheckedAt      time.Time
	// Suppressed marks a denied downgraded to error by the SSO probe's
	// channel-vocabulary breaker; callers may answer with witness
	// re-validation instead of retrying blindly.
	Suppressed bool
}

// Risky reports whether the verdict marks the identity as risk.
func (r Result) Risky() bool {
	return r.Verdict == VerdictDenied || r.Verdict == VerdictFlagged
}

var (
	detailsPattern = regexp.MustCompile(`botFlagDetails"\s*:\s*(?:null|"([^"]*)")`)
	// The account payload nests one level deeper than the flag itself.
	sourceValuePattern = regexp.MustCompile(`botFlagSource"\s*:\s*(null|-?\d+)`)
	// 用户载荷对象的稳定邻居字段(2026-08-24 双账号实测: botFlagSource 与
	// botFlagDetails 总是相邻出现在同一 user 对象里, 前后有 emailConfirmed/
	// experienceAcls/createTime)。锚定防止未来页面改版把字段名挪到营销文案
	// 或组件枚举时被误读为风险状态。
	userPayloadAnchors  = []string{"emailConfirmed", "experienceAcls", "canUseDebugTools"}
	payloadAnchorWindow = 600
)

// anchoredBotFlagMatch 在所有 botFlagSource 出现点中挑选落在用户载荷对象里
// 的那一个: 要求同窗内存在 botFlagDetails 与至少一个邻居锚字段。全部出现点
// 都不满足锚定时返回 nil——调用方按 error 处理(结构突变时宁可重试也不误判)。
func anchoredBotFlagMatch(normalized string) (source []string, details []string) {
	for _, loc := range sourceValuePattern.FindAllStringSubmatchIndex(normalized, -1) {
		windowStart := max(0, loc[0]-payloadAnchorWindow)
		windowEnd := min(len(normalized), loc[1]+payloadAnchorWindow)
		window := normalized[windowStart:windowEnd]
		if !strings.Contains(window, "botFlagDetails") {
			continue
		}
		anchored := false
		for _, anchor := range userPayloadAnchors {
			if strings.Contains(window, anchor) {
				anchored = true
				break
			}
		}
		if !anchored {
			continue
		}
		source = sourceValuePattern.FindStringSubmatch(normalized[loc[0]:windowEnd])
		details = detailsPattern.FindStringSubmatch(normalized[loc[0]:windowEnd])
		return source, details
	}
	return nil, nil
}

// looksComplete reports whether the body carries terminal evidence that the
// render finished: a closing </html> for document renders or a closed inline
// script for flight payloads. A stream truncated mid-push ends inside string
// content and fails this check, so a cut body can never read as clean.
func looksComplete(normalized string) bool {
	trimmed := strings.TrimRight(normalized, " \t\r\n")
	return strings.HasSuffix(trimmed, "</html>") || strings.HasSuffix(trimmed, "</script>")
}

// looksLikeGrokHome reports whether the body is a real grok.com homepage
// (Next.js RSC payload). A challenge page or error shell must never be read
// as "no flags = clean".
func looksLikeGrokHome(normalized string) bool {
	return strings.Contains(normalized, "__next_f") ||
		strings.Contains(normalized, "self.__next") ||
		strings.Contains(normalized, "next_f.push") ||
		(strings.Contains(normalized, "grok") &&
			(strings.Contains(normalized, "application/json") ||
				strings.Contains(normalized, "</html>") ||
				strings.Contains(normalized, "<html")))
}

// ParseRisk extracts the registration-risk state from a grok.com homepage
// body. The returned verdict is Clean only when the body looks like a real
// homepage and exposes no bot-flag fields.
func ParseRisk(body string) Result {
	normalized := strings.ReplaceAll(body, "\\\"", "\"")
	sourceMatch, detailsMatch := anchoredBotFlagMatch(normalized)
	// 出现过 botFlagSource 字样但没有一处满足载荷锚: 视为结构突变, 交由
	// error 重试语义而不是把无锚匹配当 clean/风险。
	unanchored := sourceMatch == nil && strings.Contains(normalized, "botFlagSource")

	result := Result{Verdict: VerdictClean}
	found := sourceMatch != nil || detailsMatch != nil
	if sourceMatch != nil && sourceMatch[1] != "null" {
		if value, err := strconv.Atoi(sourceMatch[1]); err == nil {
			result.BotFlagSource = value
		}
	}
	if detailsMatch != nil {
		result.BotFlagDetails = detailsMatch[1]
		details := strings.ToLower(result.BotFlagDetails)
		if strings.Contains(details, "policy=deny") && strings.Contains(details, "$registration") {
			result.Verdict = VerdictDenied
		}
		for _, item := range strings.Split(result.BotFlagDetails, ",") {
			key, value, ok := strings.Cut(item, "=")
			if ok && strings.EqualFold(strings.TrimSpace(key), "risk") {
				// strconv.ParseFloat accepts NaN/Inf/hex floats; a non-finite score
				// would poison GORM writes and json.Marshal downstream (fuzz-found).
				if score, err := strconv.ParseFloat(strings.TrimSpace(value), 64); err == nil && !math.IsNaN(score) && !math.IsInf(score, 0) {
					result.RiskScore = score
					result.HasRiskScore = true
				}
			}
		}
	}
	if result.Verdict == VerdictClean && (result.BotFlagSource == 1 || result.BotFlagSource == 2) {
		result.Verdict = VerdictFlagged
	}
	if unanchored {
		result.Verdict = VerdictError
		result.Error = "botFlagSource present but no occurrence sits inside a recognizable user payload (page structure changed?)"
		return result
	}
	if !found {
		// A flag-less body is only trustworthy when it is a real homepage AND
		// carries terminal evidence of completeness: a close-delimited stream cut
		// mid-flight reads as a clean EOF, and __next_f appears before the user
		// payload, so a truncation would otherwise classify as clean.
		if !looksLikeGrokHome(normalized) {
			result.Verdict = VerdictError
			result.Error = "grok.com 200 but body exposes no botFlag fields and no homepage markers"
		} else if !looksComplete(normalized) {
			result.Verdict = VerdictError
			result.Error = "grok.com 200 homepage markers without terminal evidence (truncated body)"
		}
	}
	return result
}

// Checker performs RSC checks over a direct (no-proxy) browser-TLS client.
type Checker struct {
	// baseURL overrides https://grok.com/ for tests (empty means production).
	baseURL string
	Timeout time.Duration
}

// NewChecker builds a checker; timeout<=0 falls back to 45s like regc.
func NewChecker(timeout time.Duration) *Checker {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	return &Checker{Timeout: timeout}
}

func (c *Checker) newClient() (tlsclient.HttpClient, error) {
	return tlsclient.NewHttpClient(tlsclient.NewNoopLogger(),
		tlsclient.WithTimeoutSeconds(int(c.Timeout.Seconds())+1),
		tlsclient.WithClientProfile(profiles.Chrome_146),
		tlsclient.WithNotFollowRedirects(),
	)
}

// Check fetches https://grok.com/ with the SSO cookie and classifies the
// account. Transient statuses (403/429/502/503) retry once on a fresh client.
func (c *Checker) Check(ctx context.Context, ssoToken string) Result {
	ssoToken = strings.TrimSpace(ssoToken)
	if ssoToken == "" {
		return Result{Verdict: VerdictError, Error: "empty sso token", CheckedAt: time.Now().UTC()}
	}
	result := c.attempt(ctx, ssoToken)
	if result.HTTPStatus == 403 || result.HTTPStatus == 429 || result.HTTPStatus == 502 || result.HTTPStatus == 503 {
		select {
		case <-ctx.Done():
			return result
		case <-time.After(1200 * time.Millisecond):
		}
		retried := c.attempt(ctx, ssoToken)
		if retried.Verdict != VerdictError || retried.HTTPStatus == 200 {
			return retried
		}
	}
	return result
}

func (c *Checker) attempt(ctx context.Context, ssoToken string) Result {
	now := time.Now().UTC()
	client, err := c.newClient()
	if err != nil {
		return Result{Verdict: VerdictError, Error: fmt.Sprintf("build client: %v", err), CheckedAt: now}
	}
	defer client.CloseIdleConnections()
	target := c.baseURL
	if target == "" {
		target = "https://grok.com/"
	}
	request, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, target, nil)
	if err != nil {
		return Result{Verdict: VerdictError, Error: fmt.Sprintf("build request: %v", err), CheckedAt: now}
	}
	request.Header.Set("Accept", "text/html,application/xhtml+xml")
	request.Header.Set("Accept-Encoding", "gzip, deflate, br")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cookie", "sso="+ssoToken+"; sso-rw="+ssoToken)
	response, err := client.Do(request)
	if err != nil {
		return Result{Verdict: VerdictError, Error: fmt.Sprintf("grok.com request failed: %v", err), CheckedAt: now}
	}
	defer func() { _ = response.Body.Close() }()
	status := response.StatusCode
	if status != 200 {
		return Result{Verdict: VerdictError, HTTPStatus: status, Error: fmt.Sprintf("grok.com HTTP %d", status), CheckedAt: now}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return Result{Verdict: VerdictError, HTTPStatus: status, Error: fmt.Sprintf("read body: %v", err), CheckedAt: now}
	}
	if response.ContentLength > 0 && int64(len(body)) < response.ContentLength {
		return Result{Verdict: VerdictError, HTTPStatus: status, Error: fmt.Sprintf("truncated body: %d of %d bytes", len(body), response.ContentLength), CheckedAt: now}
	}
	result := ParseRisk(string(body))
	result.HTTPStatus = status
	result.CheckedAt = now
	return result
}

// secretPairPattern strips credential-shaped key=value pairs from upstream
// RSC payload text so a compromised upstream cannot smuggle secrets into
// persisted verdict diagnostics or risk logs (defense in depth; the payload
// is upstream-controlled). Shared by the persistence layer (pre-save) and
// the risk service (pre-log) — both sides must see the same rule.
var secretPairPattern = regexp.MustCompile(`(?i)((?:access|refresh|id|sso|session|device)[_-]?token|client[_-]?secret|password|authorization|cookie|code[_-]?verifier)=[^&\s'<>]+`)

// RedactSecrets removes credential-shaped pairs from upstream RSC text.
// It is exported so every consumer of RSC payload fragments (logs, storage)
// applies the identical rule instead of drifting copies.
func RedactSecrets(value string) string {
	return secretPairPattern.ReplaceAllString(value, `$1=[REDACTED]`)
}
