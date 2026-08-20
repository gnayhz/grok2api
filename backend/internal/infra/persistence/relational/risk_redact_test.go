package relational

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestSaveRiskVerdictRedactsSecretsInDetails: botFlagDetails and Error are
// upstream-controlled strings persisted into account_risk_verdicts and later
// logged by the risk service. A compromised upstream echoing credentials in
// key=value form must not get them stored (defense-in-depth redaction at the
// persistence boundary).
func TestSaveRiskVerdictRedactsSecretsInDetails(t *testing.T) {
	ctx := context.Background()
	database := openTestDatabase(t)
	repo := NewRiskRepository(database)
	err := repo.SaveRiskVerdict(ctx, AccountRiskVerdict{
		AccountID: 501, Verdict: "denied",
		BotFlagDtl: "policy=deny,risk=0.9,event=x sso_token=sso-leak-123 client_secret=cs-leak-456",
		Error:      "refresh failed: refresh_token=rt-leak-789",
		CheckedAt:  time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := repo.GetRiskVerdict(ctx, 501)
	if err != nil {
		t.Fatal(err)
	}
	combined := stored.BotFlagDtl + " " + stored.Error
	for _, leak := range []string{"sso-leak-123", "cs-leak-456", "rt-leak-789"} {
		if strings.Contains(combined, leak) {
			t.Fatalf("secret %q persisted into verdict details: details=%q error=%q", leak, stored.BotFlagDtl, stored.Error)
		}
	}
}
