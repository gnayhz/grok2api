package account

import "testing"

func TestWebChatAndConsoleQuotaModeClassification(t *testing.T) {
	for _, mode := range []string{QuotaModeWebAuto, QuotaModeWebFast, QuotaModeWebExpert, QuotaModeWebHeavy} {
		if !IsWebChatQuotaMode(mode) {
			t.Fatalf("%s should be a Web chat quota mode", mode)
		}
	}
	if IsWebChatQuotaMode("weekly") || IsWebChatQuotaMode(QuotaModeWebVideo) {
		t.Fatal("non-chat Web modes must not classify as chat")
	}
	for _, mode := range []string{QuotaModeConsole, QuotaModeConsoleImage, QuotaModeConsoleVideo} {
		if !IsConsoleUsageQuotaMode(mode) {
			t.Fatalf("%s should be a Console usage quota mode", mode)
		}
	}
	if IsConsoleUsageQuotaMode("unknown") {
		t.Fatal("unknown Console mode must not be usage-controlling")
	}
}

func TestQuotaWindowControlsRouting(t *testing.T) {
	for _, mode := range []string{QuotaModeConsole, QuotaModeConsoleImage, QuotaModeConsoleVideo} {
		if !QuotaWindowControlsRouting(ProviderConsole, mode) {
			t.Fatalf("%s must control its matching Console route", mode)
		}
	}
	if QuotaWindowControlsRouting(ProviderConsole, "unknown") {
		t.Fatal("unknown Console quota mode must not control routing")
	}
	if !QuotaWindowControlsRouting(ProviderWeb, "weekly") {
		t.Fatal("Web quota windows must keep controlling routing")
	}
}
