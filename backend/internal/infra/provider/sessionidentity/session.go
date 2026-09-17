package sessionidentity

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domainegress "github.com/chenyme/grok2api/backend/internal/domain/egress"
	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
	"github.com/chenyme/grok2api/backend/internal/pkg/texts"
	"github.com/chenyme/grok2api/backend/internal/port/provider"
	"io"
	"net/http"
	"strings"
	"time"
)

const responseBodyLimit = 64 << 10

// Fetch 通过 Grok Web Session 接口读取 SSO 账号的稳定身份元数据。
// Web 与 Console 共用该链路，确保代理、Cookie、UA 和 Resin 身份一致。
func Fetch(ctx context.Context, baseURL string, credential account.Credential, egress infraegress.CredentialLeaser, cipher security.Cryptor) (provider.AccountIdentity, error) {
	if credential.AuthType != account.AuthTypeSSO || (credential.Provider != account.ProviderWeb && credential.Provider != account.ProviderConsole) {
		return provider.AccountIdentity{}, fmt.Errorf("仅 Grok Web 与 Console SSO 账号支持身份同步")
	}
	if egress == nil || cipher == nil {
		return provider.AccountIdentity{}, fmt.Errorf("Session 身份同步依赖未初始化")
	}
	token, err := cipher.Decrypt(credential.EncryptedAccessToken)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	if strings.TrimSpace(token) == "" {
		return provider.AccountIdentity{}, provider.ErrUnauthorized
	}
	// Session 身份读取属于凭据语义(会话验证),缺标默认推理类会污染推理徽标。
	ctx = infraegress.WithTrafficClass(ctx, domainegress.TrafficClassCredential)
	lease, err := egress.AcquireCredential(ctx, domainegress.ScopeWeb, credential)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	defer lease.Release()
	return FetchWithLease(ctx, baseURL, token, lease)
}

// FetchWithLease resolves Session identity through an already selected Web
// egress lease. It keeps just-in-time Gateway identity resolution on the same
// physical exit, browser fingerprint, and Clearance as the following request.
func FetchWithLease(ctx context.Context, baseURL, token string, lease *infraegress.Lease) (provider.AccountIdentity, error) {
	if lease == nil {
		return provider.AccountIdentity{}, fmt.Errorf("Session 身份同步租约未初始化")
	}
	if strings.TrimSpace(token) == "" {
		return provider.AccountIdentity{}, provider.ErrUnauthorized
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	origin := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, origin+"/api/auth/session", nil)
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	request.Header = browserHeaders(token, origin, lease)
	response, err := lease.Do(request)
	if err != nil {
		lease.Observe(0, err)
		return provider.AccountIdentity{}, err
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(response.Body, responseBodyLimit+1))
	if err != nil {
		return provider.AccountIdentity{}, err
	}
	if len(body) > responseBodyLimit {
		return provider.AccountIdentity{}, fmt.Errorf("Grok Session 响应超过安全上限")
	}
	lease.Observe(response.StatusCode, nil)
	if response.StatusCode == http.StatusUnauthorized {
		return provider.AccountIdentity{}, provider.ErrUnauthorized
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return provider.AccountIdentity{}, fmt.Errorf("Grok Session 接口返回 %d", response.StatusCode)
	}
	return Parse(body)
}

func Parse(body []byte) (provider.AccountIdentity, error) {
	var value struct {
		Status  string `json:"status"`
		Session struct {
			UserID         string `json:"userId"`
			Email          string `json:"email"`
			OrganizationID string `json:"organizationId"`
		} `json:"session"`
		User struct {
			ID     string `json:"id"`
			UserID string `json:"userId"`
			Sub    string `json:"sub"`
			Email  string `json:"email"`
			TeamID string `json:"teamId"`
		} `json:"user"`
		ID     string `json:"id"`
		UserID string `json:"userId"`
		Sub    string `json:"sub"`
		Email  string `json:"email"`
		TeamID string `json:"teamId"`
	}
	if err := json.Unmarshal(body, &value); err != nil {
		return provider.AccountIdentity{}, fmt.Errorf("解析 Grok Session: %w", err)
	}
	// Reject unavailable sessions before accepting residual identity fields that may still be present.
	status := strings.TrimSpace(value.Status)
	if strings.EqualFold(status, "unauthenticated") {
		return provider.AccountIdentity{}, provider.ErrUnauthorized
	}
	if strings.EqualFold(status, "blocked") {
		return provider.AccountIdentity{}, fmt.Errorf("%w: session status blocked", provider.ErrUnauthorized)
	}
	identity := provider.AccountIdentity{
		UserID: texts.FirstNonEmpty(value.Session.UserID, value.User.ID, value.User.UserID, value.User.Sub, value.ID, value.UserID, value.Sub),
		Email:  texts.FirstNonEmpty(value.Session.Email, value.User.Email, value.Email),
		TeamID: texts.FirstNonEmpty(value.Session.OrganizationID, value.User.TeamID, value.TeamID),
	}
	identity.UserID = strings.TrimSpace(identity.UserID)
	identity.Email = strings.TrimSpace(identity.Email)
	identity.TeamID = strings.TrimSpace(identity.TeamID)
	if identity.UserID == "" && identity.Email == "" {
		return provider.AccountIdentity{}, fmt.Errorf("Grok Session 缺少账号身份")
	}
	return identity, nil
}

func browserHeaders(token, origin string, lease *infraegress.Lease) http.Header {
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	value := http.Header{}
	browserheaders.ApplyBrowserRequestHeaders(value, userAgent,
		infraegress.BuildSSOCookie(token, lease.CFCookies), "", origin+"/")
	return value
}

// SanitizeSSOToken 清洗导入文本中的 SSO token：去掉 sso= 前缀、按分号截断
// cookie 尾部并移除控制字符。Console 与 Web 导入路径共用，语义不得分叉。
func SanitizeSSOToken(value string) string {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(strings.ToLower(value), "sso=") {
		value = strings.TrimSpace(value[len("sso="):])
	}
	if token, _, found := strings.Cut(value, ";"); found {
		value = token
	}
	return strings.TrimSpace(strings.NewReplacer("\r", "", "\n", "", "\x00", "").Replace(value))
}
