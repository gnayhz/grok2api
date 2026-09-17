package console

import (
	"net/http"
	"strings"

	infraegress "github.com/chenyme/grok2api/backend/internal/infra/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/provider/browserheaders"
)

func applyBrowserHeaders(request *http.Request, token string, lease *infraegress.Lease) {
	userAgent := strings.TrimSpace(lease.UserAgent)
	if userAgent == "" {
		userAgent = infraegress.DefaultUserAgent
	}
	browserheaders.ApplyBrowserRequestHeaders(request.Header, userAgent,
		infraegress.BuildSSOCookie(token, lease.CFCookies), "https://console.x.ai", "https://console.x.ai/")
}
