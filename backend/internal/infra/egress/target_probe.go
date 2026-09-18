package egress

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/chenyme/grok2api/backend/internal/domain/account"
	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/port/physical"
)

// ProbeBuildTarget observes the destination-facing IP through the same Build
// transport and account identity as generation. No credentials are sent and no
// health/configuration state is written. Per-request rotating nodes cannot make
// an assertion about the intervening generation's IP.
func (m *Manager) ProbeBuildTarget(ctx context.Context, credential account.Credential, nodeID uint64, baseURL string) (key string, family int, binding uint64, err error) {
	if nodeID == 0 {
		return "", 0, 0, errors.New("fixed node required")
	}
	node, err := m.repository.GetEgressNode(ctx, nodeID)
	if err != nil {
		return "", 0, 0, err
	}
	if !node.Enabled || node.ProxyPool {
		return "", 0, 0, errors.New("fixed enabled node required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return "", 0, 0, errors.New("invalid build target")
	}
	u.Path, u.RawPath, u.RawQuery, u.Fragment = "/cdn-cgi/trace", "", "", ""
	done, err := m.tasks.begin("probe")
	if err != nil {
		return "", 0, 0, err
	}
	defer done()
	ctx, cancel := m.tasks.context(ctx, 10*time.Second)
	defer cancel()
	ctx = physical.WithQualityVerificationNode(ctx, nodeID)
	lease, err := m.AcquireCredential(ctx, domain.ScopeBuild, credential)
	if err != nil {
		return "", 0, 0, err
	}
	defer lease.Release()
	if lease.NodeID != nodeID || lease.proxyPool {
		return "", 0, 0, errors.New("target path changed")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", 0, 0, err
	}
	response, err := lease.Do(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		return "", 0, 0, err
	}
	if response == nil || response.Body == nil || response.StatusCode != http.StatusOK {
		return "", 0, 0, errors.New("target trace unavailable")
	}
	if response.Request != nil && response.Request.URL.Host != u.Host {
		return "", 0, 0, errors.New("target trace redirected")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8193))
	if err != nil || len(body) > 8192 {
		return "", 0, 0, errors.New("target trace incomplete")
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, "ip=") {
			continue
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(strings.TrimPrefix(line, "ip=")))
		if err != nil {
			return "", 0, 0, errors.New("target trace address invalid")
		}
		ip = ip.Unmap()
		family = 6
		if ip.Is4() {
			family = 4
		}
		return fmt.Sprintf("%x", sha256.Sum256([]byte(u.Host+"|"+ip.String()))), family, lease.healthBindingRevision, nil
	}
	return "", 0, 0, errors.New("target trace address missing")
}
