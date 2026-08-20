package rsc

import (
	"strings"
	"testing"
	"time"
)

func homepage(payload string) string {
	escaped := strings.ReplaceAll(payload, "\"", "\\\"")
	return "<!doctype html><script>self.__next_f.push([1,\"" + escaped + "\"])</script>"
}

// Ported from regc parse_rsc_risk tests.
func TestParsesRegistrationDeny(t *testing.T) {
	body := homepage("{\"user\":{\"botFlagSource\":1,\"botFlagDetails\":\"policy=deny,risk=0.95,event=$registration\"}}")
	result := ParseRisk(body)
	if result.Verdict != VerdictDenied || result.BotFlagSource != 1 || !result.HasRiskScore || result.RiskScore != 0.95 {
		t.Fatalf("deny fixture = %#v", result)
	}
	if !strings.Contains(result.BotFlagDetails, "$registration") {
		t.Fatalf("details = %q", result.BotFlagDetails)
	}
}

func TestParsesFlaggedSourceWithoutDeny(t *testing.T) {
	body := homepage("{\"user\":{\"botFlagSource\":2,\"botFlagDetails\":\"castle_token: no_token event=$login source=Web\"}}")
	result := ParseRisk(body)
	if result.Verdict != VerdictFlagged || result.BotFlagSource != 2 {
		t.Fatalf("flagged fixture = %#v", result)
	}
}

func TestCleanAccountHasNoFlag(t *testing.T) {
	body := homepage("{\"user\":{\"email\":\"a@b.c\"}}")
	result := ParseRisk(body)
	if result.Verdict != VerdictClean || result.BotFlagSource != 0 {
		t.Fatalf("clean fixture = %#v", result)
	}
}

func TestNullSourceIsNotFlagged(t *testing.T) {
	body := homepage("{\"user\":{\"botFlagSource\":null}}")
	result := ParseRisk(body)
	if result.Verdict != VerdictClean {
		t.Fatalf("null-source fixture = %#v", result)
	}
}

// A Cloudflare challenge page must never classify as clean.
func TestChallengePageIsError(t *testing.T) {
	result := ParseRisk("<html><body>Attention Required! | Cloudflare<script>challenge</script></body></html>")
	if result.Verdict != VerdictError {
		t.Fatalf("challenge page = %#v, want error", result)
	}
}

func TestEmptyBodyIsError(t *testing.T) {
	result := ParseRisk("")
	if result.Verdict != VerdictError {
		t.Fatalf("empty body = %#v, want error", result)
	}
}

// Real-world shape observed in production: nested escaping plus risk score.
func TestProductionRiskShape(t *testing.T) {
	body := homepage("{\"user\":{\"botFlagSource\":1,\"botFlagDetails\":\"policy=deny,risk=0.86,event=$registration\"}}")
	result := ParseRisk(body)
	if result.Verdict != VerdictDenied || result.RiskScore != 0.86 || !result.Risky() {
		t.Fatalf("production fixture = %#v", result)
	}
}

func TestNewCheckerDefaultTimeout(t *testing.T) {
	if got := NewChecker(0).Timeout; got != 45*time.Second {
		t.Fatalf("default timeout = %s", got)
	}
}

func TestCheckEmptyToken(t *testing.T) {
	result := NewChecker(time.Second).Check(t.Context(), "  ")
	if result.Verdict != VerdictError {
		t.Fatalf("empty token = %#v", result)
	}
}
