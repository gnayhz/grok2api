package config

import (
	"strings"
	"testing"
	"time"
)

func TestAccountRiskMethodValidation(t *testing.T) {
	base := func(method string) AccountRiskConfig {
		return AccountRiskConfig{RSCCheck: AccountRiskRSCConfig{Method: method, Concurrency: 2, Timeout: Duration(30 * time.Second), OnDenied: "flag"}}
	}
	for _, method := range []string{"", "ssoProbe", " ssoProbe "} {
		if err := base(method).Validate(); err != nil {
			t.Fatalf("method %q must validate: %v", method, err)
		}
	}
	// homepage 解析器已删除:拒绝启动而不是静默把所有账号读作 clean。
	if err := base("homepage").Validate(); err == nil || !strings.Contains(err.Error(), "homepage") {
		t.Fatalf("removed homepage method must be rejected with a migration hint, got %v", err)
	}
	if err := base("rscPayload").Validate(); err == nil || !strings.Contains(err.Error(), "method") {
		t.Fatalf("unknown method must be rejected with a method error, got %v", err)
	}
}

func TestDefaultAccountRiskMethodIsSSOProbe(t *testing.T) {
	if got := DefaultAccountRiskConfig().RSCCheck.Method; got != "ssoProbe" {
		t.Fatalf("default rscCheck method = %q, want ssoProbe (grok.com stopped delivering RSC payload flags)", got)
	}
}
