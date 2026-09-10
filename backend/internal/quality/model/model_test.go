package model

import "testing"

func TestAccountSchedulableStates(t *testing.T) {
	cases := map[AccountState]bool{
		AccountActive:    true,
		AccountRemanded:  false,
		AccountSentenced: false,
	}
	for state, want := range cases {
		if got := state.Schedulable(); got != want {
			t.Fatalf("account %s schedulable=%v want=%v", state, got, want)
		}
	}
}

func TestExitSchedulableStates(t *testing.T) {
	if !ExitAvailable.Schedulable() || ExitRemanded.Schedulable() || ExitBanned.Schedulable() {
		t.Fatal("exit scheduling predicate is inconsistent")
	}
}

func TestAccountTransitionMatrix(t *testing.T) {
	legal := []struct{ from, to AccountState }{
		{AccountActive, AccountRemanded},
		{AccountRemanded, AccountActive},
		{AccountRemanded, AccountSentenced},
	}
	for _, item := range legal {
		if !CanTransitionAccount(item.from, item.to) {
			t.Fatalf("legal account transition rejected: %s -> %s", item.from, item.to)
		}
	}
	if !CanTransitionAccount(AccountActive, AccountActive) {
		t.Fatal("active -> active must be idempotent")
	}
	for _, item := range []struct{ from, to AccountState }{
		{AccountActive, AccountSentenced},
		{AccountSentenced, AccountActive},
		{AccountSentenced, AccountRemanded},
		{AccountRemanded, AccountRemanded},
	} {
		if CanTransitionAccount(item.from, item.to) {
			t.Fatalf("illegal account transition accepted: %s -> %s", item.from, item.to)
		}
	}
}

func TestExitTransitionMatrix(t *testing.T) {
	for _, item := range []struct{ from, to ExitState }{
		{ExitAvailable, ExitRemanded},
		{ExitRemanded, ExitAvailable},
		{ExitRemanded, ExitBanned},
		{ExitBanned, ExitAvailable},
	} {
		if !CanTransitionExit(item.from, item.to) {
			t.Fatalf("legal exit transition rejected: %s -> %s", item.from, item.to)
		}
	}
	if CanTransitionExit(ExitBanned, ExitRemanded) || CanTransitionExit(ExitAvailable, ExitBanned) {
		t.Fatal("exit ban must require a remanded case and cannot be reopened directly")
	}
}

func TestPoolTunnelNeverBanned(t *testing.T) {
	if NodePoolSticky.QualityBanApplicable() || NodePoolPerRequest.QualityBanApplicable() {
		t.Fatal("pool tunnel nodes must not receive an IP-level ban")
	}
	if !NodeFixed.QualityBanApplicable() || !NodeWebhook.QualityBanApplicable() {
		t.Fatal("fixed and webhook nodes must support an IP-level ban")
	}
}

func TestOutcomeDecidability(t *testing.T) {
	if !OutcomeDelivered.Decidable() || !OutcomeDegraded.Decidable() || OutcomeError.Decidable() {
		t.Fatal("only delivered and degraded outcomes are quality evidence")
	}
}

func TestCaseStatuses(t *testing.T) {
	if CaseInvestigating.Closed() {
		t.Fatal("investigating case must remain open")
	}
	for _, status := range []CaseStatus{CaseAccountGuilty, CaseExitGuilty, CaseDismissed} {
		if !status.Closed() {
			t.Fatalf("case status %s must be terminal", status)
		}
	}
}
