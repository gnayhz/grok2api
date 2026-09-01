package egress

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	domain "github.com/chenyme/grok2api/backend/internal/domain/egress"
	"github.com/chenyme/grok2api/backend/internal/infra/security"
)

func TestSanitizeCloudflareCookiesDropsControlsAndNonCloudflareValues(t *testing.T) {
	value := SanitizeCloudflareCookies("CF_CLEARANCE=valid; __cf_bm=bad\r\nX-Leak: yes; sso=secret; cf_chl_test=ok")
	if value != "cf_clearance=valid; cf_chl_test=ok" {
		t.Fatalf("sanitized cookies = %q", value)
	}
	if strings.Contains(strings.ToLower(value), "sso") || strings.Contains(value, "\r") || strings.Contains(value, "\n") {
		t.Fatalf("unsafe cookie value = %q", value)
	}
}

func TestNormalizeProxyURLValidatesStructure(t *testing.T) {
	vmess := "vmess://" + base64.RawStdEncoding.EncodeToString([]byte(`{"v":"2","ps":"node","add":"proxy.example","port":"443","id":"123e4567-e89b-12d3-a456-426614174000","aid":"0","scy":"auto","net":"ws","tls":"tls","sni":"edge.example","host":"edge.example","path":"/ws"}`))
	for _, raw := range []string{
		"http://user:password@127.0.0.1:8080", "https://proxy.example:8443",
		"socks4://127.0.0.1:1080", "socks4a://proxy.example:1080",
		"socks5://user:password@127.0.0.1:1080", "socks5h://user:password@proxy.example:1080",
		"trojan://password@proxy.example:443?security=tls&sni=edge.example#remark",
		"vless://123e4567-e89b-12d3-a456-426614174000@proxy.example:443?encryption=none&security=tls&sni=edge.example#remark",
		"ss://YWVzLTEyOC1nY206c2VjcmV0@proxy.example:8388#remark",
		vmess,
	} {
		value, err := NormalizeProxyURL(raw)
		if err != nil || value == "" {
			t.Fatalf("valid proxy %q = %q, err = %v", raw, value, err)
		}
	}
	for _, invalid := range []string{
		"file:///tmp/proxy", "https://", "http://proxy.example/path", "http://proxy.example\r\nX-Leak: yes",
		"vless://uuid@127.0.0.1:443?encryption=none&flow=xtls-rprx-vision#remark",
		"ss://base64#remark", "vmess://base64#remark", "hysteria://127.0.0.1:443",
		"hysteria2://127.0.0.1:443", "tuic://user:pass@127.0.0.1:443", "tuicv5://user:pass@127.0.0.1:443",
	} {
		if _, err := NormalizeProxyURL(invalid); err == nil {
			t.Fatalf("invalid proxy accepted: %q", invalid)
		}
	}
}

func TestNormalizeProxyURLStripsTunnelRemarks(t *testing.T) {
	base := "trojan://password@proxy.example:443?security=tls&sni=edge.example"
	one, err := NormalizeProxyURL(base + "#one")
	if err != nil {
		t.Fatal(err)
	}
	two, err := NormalizeProxyURL(base + "#two")
	if err != nil {
		t.Fatal(err)
	}
	if one != two || strings.Contains(one, "#") {
		t.Fatalf("normalized tunnel identities = %q and %q", one, two)
	}
}

func TestNormalizeProxyURLAllowsAccountPlaceholderOnlyInUsername(t *testing.T) {
	value, err := NormalizeProxyURL("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	if value != "socks5h://Default.%7Baccount%7D:token@resin:2260" && value != "socks5h://Default.{account}:token@resin:2260" {
		t.Fatalf("normalized Resin proxy = %q", value)
	}
	if !strings.Contains(value, ProxyAccountPlaceholder) {
		t.Fatalf("account placeholder was lost: %q", value)
	}
	for _, invalid := range []string{
		"socks5h://user:token@{account}:2260",
		"socks5h://user:{account}@resin:2260",
		"socks5h://{account}:{account}@resin:2260",
		"socks5h://grok2api_account_placeholder:token@{account}:2260",
	} {
		if _, err := NormalizeProxyURL(invalid); err == nil {
			t.Fatalf("invalid account placeholder accepted: %q", invalid)
		}
	}
}

func TestProxyDisplayKeepsEndpointAndRedactsCredentials(t *testing.T) {
	standard := ProxyDisplay("socks5h://operator:super-secret@proxy.example:1080")
	if standard != "socks5h://operator:%2A%2A%2A@proxy.example:1080" && standard != "socks5h://operator:***@proxy.example:1080" {
		t.Fatalf("standard proxy display = %q", standard)
	}
	if strings.Contains(standard, "super-secret") {
		t.Fatalf("standard proxy display leaked password: %q", standard)
	}
	vless := "vless://123e4567-e89b-12d3-a456-426614174000@proxy.example:443?encryption=none&security=tls&sni=edge.example"
	tunnel := ProxyDisplay(vless)
	if tunnel != "vless://***@proxy.example:443" || strings.Contains(tunnel, "123e4567") {
		t.Fatalf("tunnel proxy display = %q", tunnel)
	}
}

func TestPublicNodeProxyMetadataIsStableAcrossEncryptionNonces(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher)
	proxyURL := "http://user:secret@proxy.example:8080"
	firstEncrypted, err := cipher.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	secondEncrypted, err := cipher.Encrypt(proxyURL)
	if err != nil {
		t.Fatal(err)
	}
	first := service.publicNode(domain.Node{EncryptedProxyURL: firstEncrypted}, nil)
	second := service.publicNode(domain.Node{EncryptedProxyURL: secondEncrypted}, nil)
	if first.ProxyDisplay == "" || first.ProxyFingerprint == "" || first.ProxyFingerprint != second.ProxyFingerprint {
		t.Fatalf("proxy metadata first=%#v second=%#v", first, second)
	}
	if strings.Contains(first.ProxyDisplay, "secret") {
		t.Fatalf("proxy display leaked password: %q", first.ProxyDisplay)
	}
}

// 节点不再绑定 scope：任何节点可被任何路由目标引用，Input 只校验名称。
func TestApplyInputValidatesName(t *testing.T) {
	service := &Service{}
	if _, err := service.applyInput(domain.Node{}, Input{Name: "", Enabled: true}, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("empty name error = %v", err)
	}
	if _, err := service.applyInput(domain.Node{}, Input{Name: strings.Repeat("x", 161), Enabled: true}, true); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("oversized name error = %v", err)
	}
}

// 浏览器凭证不再属于节点输入：管理员编辑绝不改写托管 Clearance 状态。
func TestApplyInputNeverTouchesManagedClearance(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	existingCookie, err := cipher.Encrypt("cf_clearance=managed")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher)
	value, err := service.applyInput(domain.Node{UserAgent: "solved-agent", EncryptedCloudflareCookie: existingCookie}, Input{Name: "node", Enabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if value.UserAgent != "solved-agent" {
		t.Fatalf("managed userAgent must survive admin edits, got %q", value.UserAgent)
	}
	if value.EncryptedCloudflareCookie != existingCookie {
		t.Fatal("managed clearance cookie must survive admin edits")
	}
}

func TestPublicNodeReportsAccountBoundProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	encryptedProxy, err := cipher.Encrypt("socks5h://Default.{account}:token@resin:2260")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher)
	cooldown := time.Now().UTC().Add(time.Minute)
	public := service.publicNode(domain.Node{
		EncryptedProxyURL: encryptedProxy, Health: 0.2,
		FailureCount: 3, CooldownUntil: &cooldown, LastError: "legacy failure",
	}, nil)
	if !public.AccountBoundProxy {
		t.Fatal("Resin proxy was not reported as account-bound")
	}
	if !public.ProxyPool {
		t.Fatal("account-bound proxy was not reported as a proxy pool")
	}
	if public.Health != 1 || public.FailureCount != 0 || public.CooldownUntil != nil || public.LastError != "" {
		t.Fatalf("proxy pool exposed obsolete node health: %#v", public)
	}
	if service.publicNode(domain.Node{}, nil).AccountBoundProxy {
		t.Fatal("direct node was reported as account-bound")
	}
}

func TestApplyInputResetsHealthOnlyWhenEgressConfigurationChanges(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher)
	cooldown := time.Now().UTC().Add(time.Minute)
	base := domain.Node{
		Name: "node", Enabled: true, Health: 0.2,
		FailureCount: 4, CooldownUntil: &cooldown, LastError: "transport error",
	}

	renamed, err := service.applyInput(base, Input{Name: "renamed", Enabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Health != base.Health || renamed.FailureCount != base.FailureCount || renamed.CooldownUntil == nil || renamed.LastError != base.LastError {
		t.Fatalf("name-only edit reset health: %#v", renamed)
	}
	legacyPool := base
	legacyPool.ProxyPool = true
	legacyPool.EncryptedProxyURL, err = cipher.Encrypt("socks5h://proxy.example:1080")
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := service.applyInput(legacyPool, Input{Name: "renamed", Enabled: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !preserved.ProxyPool {
		t.Fatal("an update without proxyPool disabled the existing mode")
	}

	proxyURL := "socks5h://proxy.example:1080"
	proxyPool := true
	changed, err := service.applyInput(base, Input{Name: "node", Enabled: true, ProxyPool: &proxyPool, ProxyURL: &proxyURL}, false)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Health != 1 || changed.FailureCount != 0 || changed.CooldownUntil != nil || changed.LastError != "" || !changed.ProxyPool {
		t.Fatalf("egress configuration did not reset health: %#v", changed)
	}
}

func TestProxyPoolRequiresConfiguredProxy(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(nil, cipher)
	proxyPool := true
	_, err = service.applyInput(domain.Node{}, Input{
		Name: "pool", Enabled: true, ProxyPool: &proxyPool,
	}, true)
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("proxy pool without a proxy error = %v", err)
	}
}

// publicNode 的"代理池模式"投影必须与 domain 唯一判定一致,
// 不得在应用层重新发明规则。
func TestPublicNodePoolModeMatchesDomainRule(t *testing.T) {
	cipher, err := security.NewCipher("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{cipher: cipher}
	templateURL, err := cipher.Encrypt("http://{account}:secret@gw.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	plainURL, err := cipher.Encrypt("http://plain.example:8080")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		node domain.Node
		want bool
	}{
		{"plain node", domain.Node{ID: 1, EncryptedProxyURL: plainURL}, false},
		{"flag node", domain.Node{ID: 2, EncryptedProxyURL: plainURL, ProxyPool: true}, false},
		{"flag+rotation", domain.Node{ID: 4, EncryptedProxyURL: plainURL, ProxyPool: true, RotationEnabled: true}, true},
		{"template node", domain.Node{ID: 3, EncryptedProxyURL: templateURL}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			public := s.publicNode(tc.node, nil)
			if public.ProxyPool != tc.want {
				t.Fatalf("publicNode.ProxyPool = %v, want %v", public.ProxyPool, tc.want)
			}
		})
	}
}
